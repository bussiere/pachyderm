package server

import (
	"fmt"
	"sort"
	"time"

	"github.com/dancannon/gorethink"
	"github.com/golang/protobuf/proto"
	"github.com/pachyderm/pachyderm/src/client/pfs"
	ppsclient "github.com/pachyderm/pachyderm/src/client/pps"
	"github.com/pachyderm/pachyderm/src/server/pps/persist"

	"go.pedge.io/pb/go/google/protobuf"
	"go.pedge.io/pkg/time"
	"go.pedge.io/proto/rpclog"
	"go.pedge.io/proto/time"
	"golang.org/x/net/context"
)

const (
	jobInfosTable              Table = "JobInfos"
	pipelineNameIndex          Index = "PipelineName"
	pipelineNameAndCommitIndex Index = "PipelineNameAndCommitIndex"
	commitIndex                Index = "CommitIndex"

	pipelineInfosTable Table = "PipelineInfos"
	pipelineShardIndex Index = "Shard"

	connectTimeoutSeconds = 5
)

type Table string
type PrimaryKey string
type Index string

var (
	tables = []Table{
		jobInfosTable,
		pipelineInfosTable,
	}

	tableToTableCreateOpts = map[Table][]gorethink.TableCreateOpts{
		jobInfosTable: []gorethink.TableCreateOpts{
			gorethink.TableCreateOpts{
				PrimaryKey: "JobID",
			},
		},
		pipelineInfosTable: []gorethink.TableCreateOpts{
			gorethink.TableCreateOpts{
				PrimaryKey: "PipelineName",
			},
		},
	}
)

// InitDBs prepares a RethinkDB instance to be used by the rethink server.
// Rethink servers will error if they are pointed at databases that haven't had InitDBs run on them.
func InitDBs(address string, databaseName string) error {
	session, err := connect(address)
	if err != nil {
		return err
	}
	if _, err := gorethink.DBCreate(databaseName).RunWrite(session); err != nil {
		return err
	}
	for _, table := range tables {
		tableCreateOpts, ok := tableToTableCreateOpts[table]
		if ok {
			if _, err := gorethink.DB(databaseName).TableCreate(table, tableCreateOpts...).RunWrite(session); err != nil {
				return err
			}
		} else {
			if _, err := gorethink.DB(databaseName).TableCreate(table).RunWrite(session); err != nil {
				return err
			}
		}
	}

	// Create indexes
	if _, err := gorethink.DB(databaseName).Table(jobInfosTable).IndexCreate(pipelineNameIndex).RunWrite(session); err != nil {
		return err
	}
	if _, err := gorethink.DB(databaseName).Table(jobInfosTable).IndexCreate(commitIndex).RunWrite(session); err != nil {
		return err
	}
	if _, err := gorethink.DB(databaseName).Table(jobInfosTable).IndexCreateFunc(
		pipelineNameAndCommitIndex,
		func(row gorethink.Term) interface{} {
			return []interface{}{
				row.Field(pipelineNameIndex),
				row.Field(commitIndex),
			}
		}).RunWrite(session); err != nil {
		return err
	}
	if _, err := gorethink.DB(databaseName).Table(pipelineInfosTable).IndexCreate(pipelineShardIndex).RunWrite(session); err != nil {
		return err
	}

	return nil
}

// CheckDBs checks that we have all the tables/indices we need
func CheckDBs(address string, databaseName string) error {
	session, err := connect(address)
	if err != nil {
		return err
	}

	for _, table := range tables {
		if _, err := gorethink.DB(databaseName).Table(table).Wait().RunWrite(session); err != nil {
			return err
		}
	}

	if _, err := gorethink.DB(databaseName).Table(jobInfosTable).IndexWait(pipelineNameIndex).RunWrite(session); err != nil {
		return err
	}

	if _, err := gorethink.DB(databaseName).Table(jobInfosTable).IndexWait(commitIndex).RunWrite(session); err != nil {
		return err
	}

	if _, err := gorethink.DB(databaseName).Table(jobInfosTable).IndexWait(pipelineNameAndCommitIndex).RunWrite(session); err != nil {
		return err
	}

	if _, err := gorethink.DB(databaseName).Table(pipelineInfosTable).IndexWait(pipelineShardIndex).RunWrite(session); err != nil {
		return err
	}

	return nil
}

type rethinkAPIServer struct {
	protorpclog.Logger
	session      *gorethink.Session
	databaseName string
	timer        pkgtime.Timer
}

func newRethinkAPIServer(address string, databaseName string) (*rethinkAPIServer, error) {
	session, err := connect(address)
	if err != nil {
		return nil, err
	}
	return &rethinkAPIServer{
		protorpclog.NewLogger("pachyderm.ppsclient.persist.API"),
		session,
		databaseName,
		pkgtime.NewSystemTimer(),
	}, nil
}

func (a *rethinkAPIServer) Close() error {
	return a.session.Close()
}

// Timestamp cannot be set
func (a *rethinkAPIServer) CreateJobInfo(ctx context.Context, request *persist.JobInfo) (response *persist.JobInfo, err error) {
	defer func(start time.Time) { a.Log(request, response, err, time.Since(start)) }(time.Now())
	if request.JobID == "" {
		return nil, fmt.Errorf("request.JobID should be set")
	}
	if request.CreatedAt != nil {
		return nil, fmt.Errorf("request.CreatedAt should be unset")
	}
	if request.CommitIndex != "" {
		return nil, fmt.Errorf("request.CommitIndex should be unset")
	}
	request.CreatedAt = prototime.TimeToTimestamp(time.Now())
	var commits []*pfs.Commit
	for _, input := range request.Inputs {
		commits = append(commits, input.Commit)
	}
	request.CommitIndex, err = genCommitIndex(commits)
	if err != nil {
		return nil, err
	}
	if err := a.insertMessage(jobInfosTable, request); err != nil {
		return nil, err
	}
	return request, nil
}

func (a *rethinkAPIServer) InspectJob(ctx context.Context, request *ppsclient.InspectJobRequest) (response *persist.JobInfo, err error) {
	defer func(start time.Time) { a.Log(request, response, err, time.Since(start)) }(time.Now())
	if request.Job == nil {
		return nil, fmt.Errorf("request.Job cannot be nil")
	}

	jobInfo := &persist.JobInfo{}
	var mustHaveFields []interface{}
	if request.BlockState {
		mustHaveFields = append(mustHaveFields, "State")
	}
	if err := a.waitMessageByPrimaryKey(
		jobInfosTable,
		request.Job.ID,
		jobInfo,
		func(jobInfo gorethink.Term) gorethink.Term {
			if request.BlockState {
				return jobInfo.Field("State").Ne(ppsclient.JobState_JOB_STATE_RUNNING)
			}
			return gorethink.Expr(true)
		},
	); err != nil {
		return nil, err
	}
	return jobInfo, nil
}

func (a *rethinkAPIServer) ListJobInfos(ctx context.Context, request *ppsclient.ListJobRequest) (response *persist.JobInfos, retErr error) {
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	query := a.getTerm(jobInfosTable)
	commitIndexVal, err := genCommitIndex(request.InputCommit)
	if err != nil {
		return nil, err
	}
	if request.Pipeline != nil && len(request.InputCommit) > 0 {
		query = query.GetAllByIndex(
			pipelineNameAndCommitIndex,
			gorethink.Expr([]interface{}{request.Pipeline.Name, commitIndexVal}),
		)
	} else if request.Pipeline != nil {
		query = query.GetAllByIndex(
			pipelineNameIndex,
			request.Pipeline.Name,
		)
	} else if len(request.InputCommit) > 0 {
		query = query.GetAllByIndex(
			commitIndex,
			gorethink.Expr(commitIndexVal),
		)
	}
	cursor, err := query.Run(a.session)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := cursor.Close(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	result := &persist.JobInfos{}
	for {
		jobInfo := &persist.JobInfo{}
		if !cursor.Next(jobInfo) {
			break
		}
		result.JobInfo = append(result.JobInfo, jobInfo)
	}
	if err := cursor.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (a *rethinkAPIServer) DeleteJobInfo(ctx context.Context, request *ppsclient.Job) (response *google_protobuf.Empty, err error) {
	defer func(start time.Time) { a.Log(request, response, err, time.Since(start)) }(time.Now())
	if err := a.deleteMessageByPrimaryKey(jobInfosTable, request.ID); err != nil {
		return nil, err
	}
	return google_protobuf.EmptyInstance, nil
}

func (a *rethinkAPIServer) CreateJobOutput(ctx context.Context, request *persist.JobOutput) (response *google_protobuf.Empty, err error) {
	defer func(start time.Time) { a.Log(request, response, err, time.Since(start)) }(time.Now())
	if err := a.updateMessage(jobInfosTable, request); err != nil {
		return nil, err
	}
	return google_protobuf.EmptyInstance, nil
}

func (a *rethinkAPIServer) CreateJobState(ctx context.Context, request *persist.JobState) (response *google_protobuf.Empty, err error) {
	defer func(start time.Time) { a.Log(request, response, err, time.Since(start)) }(time.Now())
	if err := a.updateMessage(jobInfosTable, request); err != nil {
		return nil, err
	}
	return google_protobuf.EmptyInstance, nil
}

// timestamp cannot be set
func (a *rethinkAPIServer) CreatePipelineInfo(ctx context.Context, request *persist.PipelineInfo) (response *persist.PipelineInfo, err error) {
	defer func(start time.Time) { a.Log(request, response, err, time.Since(start)) }(time.Now())
	if request.CreatedAt != nil {
		return nil, ErrTimestampSet
	}
	request.CreatedAt = a.now()
	if err := a.insertMessage(pipelineInfosTable, request); err != nil {
		return nil, err
	}
	return request, nil
}

func (a *rethinkAPIServer) GetPipelineInfo(ctx context.Context, request *ppsclient.Pipeline) (response *persist.PipelineInfo, err error) {
	defer func(start time.Time) { a.Log(request, response, err, time.Since(start)) }(time.Now())
	pipelineInfo := &persist.PipelineInfo{}
	if err := a.getMessageByPrimaryKey(pipelineInfosTable, request.Name, pipelineInfo); err != nil {
		return nil, err
	}
	return pipelineInfo, nil
}

func (a *rethinkAPIServer) ListPipelineInfos(ctx context.Context, request *persist.ListPipelineInfosRequest) (response *persist.PipelineInfos, retErr error) {
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	query := a.getTerm(pipelineInfosTable)
	if request.Shard != nil {
		query = query.GetAllByIndex(pipelineShardIndex, request.Shard.Number)
	}
	cursor, err := query.Run(a.session)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := cursor.Close(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	result := &persist.PipelineInfos{}
	for {
		pipelineInfo := &persist.PipelineInfo{}
		if !cursor.Next(pipelineInfo) {
			break
		}
		result.PipelineInfo = append(result.PipelineInfo, pipelineInfo)
	}
	if err := cursor.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (a *rethinkAPIServer) DeletePipelineInfo(ctx context.Context, request *ppsclient.Pipeline) (response *google_protobuf.Empty, err error) {
	defer func(start time.Time) { a.Log(request, response, err, time.Since(start)) }(time.Now())
	if err := a.deleteMessageByPrimaryKey(pipelineInfosTable, request.Name); err != nil {
		return nil, err
	}
	return google_protobuf.EmptyInstance, nil
}

type PipelineChangeFeed struct {
	OldVal *persist.PipelineInfo `gorethink:"old_val,omitempty"`
	NewVal *persist.PipelineInfo `gorethink:"new_val,omitempty"`
}

func (a *rethinkAPIServer) SubscribePipelineInfos(request *persist.SubscribePipelineInfosRequest, server persist.API_SubscribePipelineInfosServer) (retErr error) {
	defer func(start time.Time) { a.Log(request, nil, retErr, time.Since(start)) }(time.Now())
	query := a.getTerm(pipelineInfosTable)
	if request.Shard != nil {
		query = query.GetAllByIndex(pipelineShardIndex, request.Shard.Number)
	}

	cursor, err := query.Changes(gorethink.ChangesOpts{
		IncludeInitial: request.IncludeInitial,
	}).Run(a.session)
	if err != nil {
		return err
	}

	var change PipelineChangeFeed
	for cursor.Next(&change) {
		if change.NewVal != nil {
			server.Send(&persist.PipelineInfoChange{
				Pipeline: change.NewVal,
			})
		} else if change.OldVal != nil {
			server.Send(&persist.PipelineInfoChange{
				Pipeline: change.OldVal,
				Removed:  true,
			})
		} else {
			return fmt.Errorf("neither old_val nor new_val was present in the changefeed; this is likely a bug")
		}
	}
	return cursor.Err()
}

func (a *rethinkAPIServer) StartPod(ctx context.Context, request *ppsclient.Job) (response *persist.JobInfo, retErr error) {
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	return a.shardOp(ctx, request, "PodsStarted")
}

func (a *rethinkAPIServer) SucceedPod(ctx context.Context, request *ppsclient.Job) (response *persist.JobInfo, retErr error) {
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	return a.shardOp(ctx, request, "PodsSucceeded")
}

func (a *rethinkAPIServer) FailPod(ctx context.Context, request *ppsclient.Job) (response *persist.JobInfo, retErr error) {
	defer func(start time.Time) { a.Log(request, response, retErr, time.Since(start)) }(time.Now())
	return a.shardOp(ctx, request, "PodsFailed")
}

func (a *rethinkAPIServer) shardOp(ctx context.Context, request *ppsclient.Job, field string) (response *persist.JobInfo, retErr error) {
	cursor, err := a.getTerm(jobInfosTable).Get(request.ID).Update(map[string]interface{}{
		field: gorethink.Row.Field(field).Add(1).Default(0),
	}, gorethink.UpdateOpts{
		ReturnChanges: true,
	}).Field("changes").Field("new_val").Run(a.session)
	if err != nil {
		return nil, err
	}

	var jobInfo persist.JobInfo
	success := cursor.Next(&jobInfo)
	if !success {
		return nil, cursor.Err()
	}

	return &jobInfo, nil
}

func (a *rethinkAPIServer) insertMessage(table Table, message proto.Message) error {
	_, err := a.getTerm(table).Insert(message).RunWrite(a.session)
	return err
}

func (a *rethinkAPIServer) updateMessage(table Table, message proto.Message) error {
	_, err := a.getTerm(table).Insert(message, gorethink.InsertOpts{Conflict: "update"}).RunWrite(a.session)
	return err
}

func (a *rethinkAPIServer) getMessageByPrimaryKey(table Table, key interface{}, message proto.Message) error {
	cursor, err := a.getTerm(table).Get(key).Default(gorethink.Error("value not found")).Run(a.session)
	if err != nil {
		return err
	}
	if cursor.Next(message) {
		return cursor.Err()
	}
	return nil
}

func (a *rethinkAPIServer) deleteMessageByPrimaryKey(table Table, value interface{}) (retErr error) {
	_, err := a.getTerm(table).Get(value).Delete().RunWrite(a.session)
	return err
}

func (a *rethinkAPIServer) waitMessageByPrimaryKey(
	table Table,
	key interface{},
	message proto.Message,
	predicate func(term gorethink.Term) gorethink.Term,
) (retErr error) {
	term := a.getTerm(table).
		Get(key).
		Default(gorethink.Error("value not found")).
		Changes(gorethink.ChangesOpts{
			IncludeInitial: true,
		}).
		Field("new_val").
		Filter(predicate)
	cursor, err := term.Run(a.session)
	if err != nil {
		return err
	}
	defer func() {
		if err := cursor.Close(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	cursor.Next(message)
	return cursor.Err()
}

func (a *rethinkAPIServer) getTerm(table Table) gorethink.Term {
	return gorethink.DB(a.databaseName).Table(table)
}

func (a *rethinkAPIServer) now() *google_protobuf.Timestamp {
	return prototime.TimeToTimestamp(a.timer.Now())
}

func connect(address string) (*gorethink.Session, error) {
	return gorethink.Connect(gorethink.ConnectOpts{
		Address: address,
		Timeout: connectTimeoutSeconds * time.Second,
	})
}

func genCommitIndex(commits []*pfs.Commit) (string, error) {
	var commitIDs []string
	for _, commit := range commits {
		if len(commit.ID) == 0 {
			return "", fmt.Errorf("can't generate index for commit \"%s/%s\"", commit.Repo.Name, commit.ID)
		}
		commitIDs = append(commitIDs, commit.ID[0:10])
	}
	sort.Strings(commitIDs)
	var result []byte
	for _, commitID := range commitIDs {
		result = append(result, commitID...)
	}
	return string(result), nil
}

package fuse

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"github.com/pachyderm/pachyderm/src/client"
	pfsclient "github.com/pachyderm/pachyderm/src/client/pfs"
	"github.com/pachyderm/pachyderm/src/client/pkg/uuid"
	"go.pedge.io/lion/proto"
	"go.pedge.io/proto/time"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

type filesystem struct {
	apiClient client.APIClient
	Filesystem
	inodes   map[string]uint64
	lock     sync.RWMutex
	handleID string
}

func newFilesystem(
	pfsAPIClient pfsclient.APIClient,
	shard *pfsclient.Shard,
	commitMounts []*CommitMount,
) *filesystem {
	return &filesystem{
		apiClient: client.APIClient{PfsAPIClient: pfsAPIClient},
		Filesystem: Filesystem{
			shard,
			commitMounts,
		},
		inodes:   make(map[string]uint64),
		lock:     sync.RWMutex{},
		handleID: uuid.NewWithoutDashes(),
	}
}

func (f *filesystem) Root() (result fs.Node, retErr error) {
	defer func() {
		protolion.Debug(&Root{&f.Filesystem, getNode(result), errorToString(retErr)})
	}()
	return &directory{
		f,
		Node{
			File: &pfsclient.File{
				Commit: &pfsclient.Commit{
					Repo: &pfsclient.Repo{},
				},
			},
		},
	}, nil
}

type directory struct {
	fs *filesystem
	Node
}

func (d *directory) Attr(ctx context.Context, a *fuse.Attr) (retErr error) {
	defer func() {
		protolion.Debug(&DirectoryAttr{&d.Node, &Attr{uint32(a.Mode)}, errorToString(retErr)})
	}()

	a.Valid = time.Nanosecond
	if d.Write {
		a.Mode = os.ModeDir | 0775
	} else {
		a.Mode = os.ModeDir | 0555
	}
	a.Inode = d.fs.inode(d.File)
	a.Mtime = prototime.TimestampToTime(d.Modified)
	return nil
}

func (d *directory) Lookup(ctx context.Context, name string) (result fs.Node, retErr error) {
	defer func() {
		protolion.Debug(&DirectoryLookup{&d.Node, name, getNode(result), errorToString(retErr)})
	}()
	if d.File.Commit.Repo.Name == "" {
		return d.lookUpRepo(ctx, name)
	}
	if d.File.Commit.ID == "" {
		return d.lookUpCommit(ctx, name)
	}
	return d.lookUpFile(ctx, name)
}

func (d *directory) ReadDirAll(ctx context.Context) (result []fuse.Dirent, retErr error) {
	defer func() {
		var dirents []*Dirent
		for _, dirent := range result {
			dirents = append(dirents, &Dirent{dirent.Inode, dirent.Name})
		}
		protolion.Debug(&DirectoryReadDirAll{&d.Node, dirents, errorToString(retErr)})
	}()
	if d.File.Commit.Repo.Name == "" {
		return d.readRepos(ctx)
	}
	if d.File.Commit.ID == "" {
		commitMount := d.fs.getCommitMount(d.getRepoOrAliasName())
		if commitMount != nil && commitMount.Commit.ID != "" {
			d.File.Commit.ID = commitMount.Commit.ID
			d.Shard = commitMount.Shard
			return d.readFiles(ctx)
		}
		return d.readCommits(ctx)
	}
	return d.readFiles(ctx)
}

func (d *directory) Create(ctx context.Context, request *fuse.CreateRequest, response *fuse.CreateResponse) (result fs.Node, _ fs.Handle, retErr error) {
	defer func() {
		protolion.Debug(&DirectoryCreate{&d.Node, getNode(result), errorToString(retErr)})
	}()
	if d.File.Commit.ID == "" {
		return nil, 0, fuse.EPERM
	}
	directory := d.copy()
	directory.File.Path = path.Join(directory.File.Path, request.Name)
	localResult := &file{
		directory: *directory,
		size:      0,
		local:     true,
	}
	response.Flags |= fuse.OpenDirectIO | fuse.OpenNonSeekable
	handle := localResult.newHandle()
	return localResult, handle, nil
}

func (d *directory) Mkdir(ctx context.Context, request *fuse.MkdirRequest) (result fs.Node, retErr error) {
	defer func() {
		protolion.Debug(&DirectoryMkdir{&d.Node, getNode(result), errorToString(retErr)})
	}()
	if d.File.Commit.ID == "" {
		return nil, fuse.EPERM
	}
	if err := d.fs.apiClient.MakeDirectory(d.File.Commit.Repo.Name, d.File.Commit.ID, path.Join(d.File.Path, request.Name)); err != nil {
		return nil, err
	}
	localResult := d.copy()
	localResult.File.Path = path.Join(localResult.File.Path, request.Name)
	return localResult, nil
}

func (d *directory) Remove(ctx context.Context, req *fuse.RemoveRequest) (retErr error) {
	defer func() {
		protolion.Debug(&FileRemove{&d.Node, errorToString(retErr)})
	}()
	return d.fs.apiClient.DeleteFile(d.Node.File.Commit.Repo.Name, d.Node.File.Commit.ID, filepath.Join(d.Node.File.Path, req.Name))
}

type file struct {
	directory
	size    int64
	local   bool
	handles []*handle
}

func (f *file) Attr(ctx context.Context, a *fuse.Attr) (retErr error) {
	defer func() {
		protolion.Debug(&FileAttr{&f.Node, &Attr{uint32(a.Mode)}, errorToString(retErr)})
	}()
	if f.directory.Write {
		// If the file is from an open commit, we just pretend that it's
		// an empty file.
		a.Size = 0
	} else {
		fileInfo, err := f.fs.apiClient.InspectFile(
			f.File.Commit.Repo.Name,
			f.File.Commit.ID,
			f.File.Path,
			f.fs.getFromCommitID(f.getRepoOrAliasName()),
			f.Shard,
		)
		if err != nil && !f.local {
			return err
		}
		if fileInfo != nil {
			a.Size = fileInfo.SizeBytes
			a.Mtime = prototime.TimestampToTime(fileInfo.Modified)
		}
	}
	a.Mode = 0666
	a.Inode = f.fs.inode(f.File)
	return nil
}

func (f *file) Open(ctx context.Context, request *fuse.OpenRequest, response *fuse.OpenResponse) (_ fs.Handle, retErr error) {
	defer func() {
		protolion.Debug(&FileOpen{&f.Node, errorToString(retErr)})
	}()
	response.Flags |= fuse.OpenDirectIO | fuse.OpenNonSeekable
	return f.newHandle(), nil
}

func (f *file) Fsync(ctx context.Context, req *fuse.FsyncRequest) error {
	for _, h := range f.handles {
		if h.w != nil {
			w := h.w
			h.w = nil
			if err := w.Close(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *filesystem) inode(file *pfsclient.File) uint64 {
	f.lock.RLock()
	inode, ok := f.inodes[key(file)]
	f.lock.RUnlock()
	if ok {
		return inode
	}
	f.lock.Lock()
	defer f.lock.Unlock()
	if inode, ok := f.inodes[key(file)]; ok {
		return inode
	}
	newInode := uint64(len(f.inodes))
	f.inodes[key(file)] = newInode
	return newInode
}

func (f *file) newHandle() *handle {
	h := &handle{
		f: f,
	}

	f.handles = append(f.handles, h)

	return h
}

type handle struct {
	f       *file
	w       io.WriteCloser
	written int
}

func (h *handle) Read(ctx context.Context, request *fuse.ReadRequest, response *fuse.ReadResponse) (retErr error) {
	defer func() {
		protolion.Debug(&FileRead{&h.f.Node, errorToString(retErr)})
	}()
	var buffer bytes.Buffer
	if err := h.f.fs.apiClient.GetFile(
		h.f.File.Commit.Repo.Name,
		h.f.File.Commit.ID,
		h.f.File.Path,
		request.Offset,
		int64(request.Size),
		h.f.fs.getFromCommitID(h.f.getRepoOrAliasName()),
		h.f.Shard,
		&buffer,
	); err != nil {
		if grpc.Code(err) == codes.NotFound {
			// This happens when trying to read from a file in an open
			// commit. We could catch this at `open(2)` time and never
			// get here, but Open is currently not a remote operation.
			//
			// ENOENT from read(2) is weird, let's call this EINVAL
			// instead.
			return fuse.Errno(syscall.EINVAL)
		}
		return err
	}
	response.Data = buffer.Bytes()
	return nil
}

func (h *handle) Write(ctx context.Context, request *fuse.WriteRequest, response *fuse.WriteResponse) (retErr error) {
	defer func() {
		protolion.Debug(&FileWrite{&h.f.Node, errorToString(retErr)})
	}()
	if h.w == nil {
		w, err := h.f.fs.apiClient.PutFileWriter(
			h.f.File.Commit.Repo.Name, h.f.File.Commit.ID, h.f.File.Path, h.f.fs.handleID)
		if err != nil {
			return err
		}
		h.w = w
	}
	// repeated is how many bytes in this write have already been sent in
	// previous call to Write. Why does the OS send us the same data twice in
	// different calls? Good question, this is a behavior that's only been
	// observed on osx, not on linux.
	repeated := h.written - int(request.Offset)
	if repeated < 0 {
		return fmt.Errorf("gap in bytes written, (OpenNonSeekable should make this impossible)")
	}
	written, err := h.w.Write(request.Data[repeated:])
	if err != nil {
		return err
	}
	response.Size = written + repeated
	h.written += written
	if h.f.size < request.Offset+int64(written) {
		h.f.size = request.Offset + int64(written)
	}
	return nil
}

func (h *handle) Flush(ctx context.Context, req *fuse.FlushRequest) error {
	if h.w != nil {
		w := h.w
		h.w = nil
		if err := w.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (h *handle) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	return nil
}

func (d *directory) copy() *directory {
	return &directory{
		fs: d.fs,
		Node: Node{
			File: &pfsclient.File{
				Commit: &pfsclient.Commit{
					Repo: &pfsclient.Repo{
						Name: d.File.Commit.Repo.Name,
					},
					ID: d.File.Commit.ID,
				},
				Path: d.File.Path,
			},
			Write:     d.Write,
			Shard:     d.Shard,
			RepoAlias: d.RepoAlias,
		},
	}
}

func (d *directory) getRepoOrAliasName() string {
	if d.RepoAlias != "" {
		return d.RepoAlias
	}
	return d.File.Commit.Repo.Name
}

func (f *filesystem) getCommitMount(nameOrAlias string) *CommitMount {
	if len(f.CommitMounts) == 0 {
		return &CommitMount{
			Commit: client.NewCommit(nameOrAlias, ""),
			Shard:  f.Shard,
		}
	}

	// We prefer alias matching over repo name matching, since there can be
	// two commit mounts with the same repo but different aliases, such as
	// "out" and "prev"
	for _, commitMount := range f.CommitMounts {
		if commitMount.Alias == nameOrAlias {
			return commitMount
		}
	}
	for _, commitMount := range f.CommitMounts {
		if commitMount.Commit.Repo.Name == nameOrAlias {
			return commitMount
		}
	}

	return nil
}

func (f *filesystem) getFromCommitID(nameOrAlias string) string {
	commitMount := f.getCommitMount(nameOrAlias)
	if commitMount == nil || commitMount.FromCommit == nil {
		return ""
	}
	return commitMount.FromCommit.ID
}

func (d *directory) lookUpRepo(ctx context.Context, name string) (fs.Node, error) {
	commitMount := d.fs.getCommitMount(name)
	if commitMount == nil {
		return nil, fuse.EPERM
	}
	repoInfo, err := d.fs.apiClient.InspectRepo(commitMount.Commit.Repo.Name)
	if err != nil {
		return nil, err
	}
	if repoInfo == nil {
		return nil, fuse.ENOENT
	}
	result := d.copy()
	result.File.Commit.Repo.Name = commitMount.Commit.Repo.Name
	result.File.Commit.ID = commitMount.Commit.ID
	result.RepoAlias = commitMount.Alias
	result.Shard = commitMount.Shard

	commitInfo, err := d.fs.apiClient.InspectCommit(
		commitMount.Commit.Repo.Name,
		commitMount.Commit.ID,
	)
	if err != nil {
		return nil, err
	}
	if commitInfo.CommitType == pfsclient.CommitType_COMMIT_TYPE_READ {
		result.Write = false
	} else {
		result.Write = true
	}
	result.Modified = commitInfo.Finished

	return result, nil
}

func (d *directory) lookUpCommit(ctx context.Context, name string) (fs.Node, error) {
	commitInfo, err := d.fs.apiClient.InspectCommit(
		d.File.Commit.Repo.Name,
		name,
	)
	if err != nil {
		return nil, err
	}
	if commitInfo == nil {
		return nil, fuse.ENOENT
	}
	result := d.copy()
	result.File.Commit.ID = name
	if commitInfo.CommitType == pfsclient.CommitType_COMMIT_TYPE_READ {
		result.Write = false
	} else {
		result.Write = true
	}
	result.Modified = commitInfo.Finished
	return result, nil
}

func (d *directory) lookUpFile(ctx context.Context, name string) (fs.Node, error) {
	var fileInfo *pfsclient.FileInfo
	var err error

	if d.Node.Write {
		// Basically, if the directory is writable, we are looking up files
		// from an open commit.  In this case, we want to return an empty file,
		// because sometimes you want to remove a file but a remove operation
		// is usually proceeded with a lookup operation, and the remove operation
		// would not be able to proceed if the lookup failed.  Therefore, we want
		// the lookup to not fail, so we return an empty file.
		fileInfo = &pfsclient.FileInfo{
			File: &pfsclient.File{
				Path: path.Join(d.File.Path, name),
			},
			FileType:  pfsclient.FileType_FILE_TYPE_REGULAR,
			SizeBytes: 0,
		}
	} else {
		fileInfo, err = d.fs.apiClient.InspectFile(
			d.File.Commit.Repo.Name,
			d.File.Commit.ID,
			path.Join(d.File.Path, name),
			d.fs.getFromCommitID(d.getRepoOrAliasName()),
			d.Shard,
		)
		if err != nil {
			return nil, fuse.ENOENT
		}
	}

	// We want to inherit the metadata other than the path, which should be the
	// path currently being looked up
	directory := d.copy()
	directory.File.Path = fileInfo.File.Path
	switch fileInfo.FileType {
	case pfsclient.FileType_FILE_TYPE_REGULAR:
		return &file{
			directory: *directory,
			size:      int64(fileInfo.SizeBytes),
			local:     false,
		}, nil
	case pfsclient.FileType_FILE_TYPE_DIR:
		return directory, nil
	default:
		return nil, fmt.Errorf("Unrecognized FileType.")
	}
}

func (d *directory) readRepos(ctx context.Context) ([]fuse.Dirent, error) {
	var result []fuse.Dirent
	if len(d.fs.CommitMounts) == 0 {
		repoInfos, err := d.fs.apiClient.ListRepo(nil)
		if err != nil {
			return nil, err
		}
		for _, repoInfo := range repoInfos {
			result = append(result, fuse.Dirent{Name: repoInfo.Repo.Name, Type: fuse.DT_Dir})
		}
	} else {
		for _, mount := range d.fs.CommitMounts {
			name := mount.Commit.Repo.Name
			if mount.Alias != "" {
				name = mount.Alias
			}
			result = append(result, fuse.Dirent{Name: name, Type: fuse.DT_Dir})
		}
	}
	return result, nil
}

func (d *directory) readCommits(ctx context.Context) ([]fuse.Dirent, error) {
	commitInfos, err := d.fs.apiClient.ListCommit([]string{d.File.Commit.Repo.Name},
		nil, client.CommitTypeNone, false, false, nil)
	if err != nil {
		return nil, err
	}
	var result []fuse.Dirent
	for _, commitInfo := range commitInfos {
		result = append(result, fuse.Dirent{Name: commitInfo.Commit.ID, Type: fuse.DT_Dir})
	}
	return result, nil
}

func (d *directory) readFiles(ctx context.Context) ([]fuse.Dirent, error) {
	fileInfos, err := d.fs.apiClient.ListFile(
		d.File.Commit.Repo.Name,
		d.File.Commit.ID,
		d.File.Path,
		d.fs.getFromCommitID(d.getRepoOrAliasName()),
		d.Shard,
		// setting recurse to false for performance reasons
		// it does however means that we won't know the correct sizes of directories
		false,
	)
	if err != nil {
		return nil, err
	}
	var result []fuse.Dirent
	for _, fileInfo := range fileInfos {
		shortPath := strings.TrimPrefix(fileInfo.File.Path, d.File.Path)
		if shortPath[0] == '/' {
			shortPath = shortPath[1:]
		}
		switch fileInfo.FileType {
		case pfsclient.FileType_FILE_TYPE_REGULAR:
			result = append(result, fuse.Dirent{Name: shortPath, Type: fuse.DT_File})
		case pfsclient.FileType_FILE_TYPE_DIR:
			result = append(result, fuse.Dirent{Name: shortPath, Type: fuse.DT_Dir})
		default:
			continue
		}
	}
	return result, nil
}

// TODO this code is duplicate elsewhere, we should put it somehwere.
func errorToString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func getNode(node fs.Node) *Node {
	switch n := node.(type) {
	default:
		return nil
	case *directory:
		return &n.Node
	case *file:
		return &n.Node
	}
}

func key(file *pfsclient.File) string {
	return fmt.Sprintf("%s/%s/%s", file.Commit.Repo.Name, file.Commit.ID, file.Path)
}

package termite

import (
	"fmt"
	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/termite/attr"
	"github.com/hanwen/termite/cba"
	"github.com/hanwen/termite/stats"
	"log"
	"net/rpc"
	"os"
	"sync"
	"time"
)

type RpcFs struct {
	fuse.DefaultFileSystem
	cache  *cba.ContentCache
	client *rpc.Client

	// Roots that we should try to fetch locally.
	localRoots []string
	timings    *stats.TimerStats
	attr       *attr.AttributeCache
	id         string

	// Below code is used to make sure we only fetch each hash
	// once.
	mutex    sync.Mutex
	cond     *sync.Cond
	fetching map[string]bool
}

func NewRpcFs(server *rpc.Client, cache *cba.ContentCache) *RpcFs {
	me := &RpcFs{}
	me.client = server
	me.timings = stats.NewTimerStats()
	me.attr = attr.NewAttributeCache(
		func(n string) *attr.FileAttr {
			return me.fetchAttr(n)
		}, nil)
	me.cond = sync.NewCond(&me.mutex)
	me.fetching = map[string]bool{}
	me.cache = cache
	return me
}

func (me *RpcFs) Close() {
	me.client.Close()
}

func (me *RpcFs) innerFetch(start, end int, hash string) ([]byte, error) {
	req := &cba.ContentRequest{
		Hash:  hash,
		Start: start,
		End:   end,
	}
	rep := &cba.ContentResponse{}
	startT := time.Nanoseconds()
	err := me.client.Call("FsServer.FileContent", req, rep)
	dt := time.Nanoseconds() - startT
	me.timings.Log("FsServer.FileContent", dt)
	me.timings.LogN("FsServer.FileContentBytes", int64(len(rep.Chunk)), dt)
	return rep.Chunk, err
}

func (me *RpcFs) FetchHash(a *attr.FileAttr) error {
	e := me.FetchHashOnce(a)
	if e == nil && a.Size < me.cache.Options.MemMaxSize {
		me.cache.FaultIn(a.Hash)
	}
	return e
}

func (me *RpcFs) FetchHashOnce(a *attr.FileAttr) error {
	me.mutex.Lock()
	defer me.mutex.Unlock()
	h := a.Hash
	for !me.cache.HasHash(h) && me.fetching[h] {
		me.cond.Wait()
	}
	if me.cache.HasHash(h) {
		return nil
	}
	me.fetching[h] = true
	me.mutex.Unlock()
	log.Printf("Fetching contents for file %s: %x", a.Path, h)
	saved, err := me.cache.Fetch(
		func(s, e int) ([]byte, error) {
			return me.innerFetch(s, e, a.Hash)
		})
	if err == nil && saved != h {
		log.Fatalf("RpcFs.FetchHashOnce: fetch corruption got %x want %x", saved, h)
	}

	me.mutex.Lock()
	delete(me.fetching, h)
	me.cond.Broadcast()
	return err
}

func (me *RpcFs) Update(req *UpdateRequest, resp *UpdateResponse) error {
	me.updateFiles(req.Files)
	return nil
}

func (me *RpcFs) updateFiles(files []*attr.FileAttr) {
	me.attr.Update(files)
}

func (me *RpcFs) fetchAttr(n string) *attr.FileAttr {
	req := &AttrRequest{
		Name:   n,
		Origin: me.id,
	}
	start := time.Nanoseconds()
	rep := &AttrResponse{}
	err := me.client.Call("FsServer.GetAttr", req, rep)
	dt := time.Nanoseconds() - start
	me.timings.Log("FsServer.GetAttr", dt)
	if err != nil {
		// fatal?
		log.Println("GetAttr error:", err)
		return nil
	}

	var wanted *attr.FileAttr
	for _, attr := range rep.Attrs {
		if attr.Path == n {
			wanted = attr
		}
	}

	// TODO - if we got a deletion, we should refetch the parent.
	return wanted
}

func (me *RpcFs) considerSaveLocal(a *attr.FileAttr) {
	absPath := a.Path
	if a.Deletion() || !a.FileInfo.IsRegular() {
		return
	}
	found := false
	for _, root := range me.localRoots {
		if HasDirPrefix(absPath, root) {
			found = true
		}
	}
	if !found {
		return
	}

	fi, _ := os.Lstat(absPath)
	if fi == nil {
		return
	}
	if attr.EncodeFileInfo(*fi) != attr.EncodeFileInfo(*a.FileInfo) {
		return
	}
}

////////////////////////////////////////////////////////////////
// FS API

func (me *RpcFs) String() string {
	return "RpcFs"
}

func (me *RpcFs) OpenDir(name string, context *fuse.Context) (chan fuse.DirEntry, fuse.Status) {
	r := me.attr.GetDir(name)
	if r.Deletion() {
		return nil, fuse.ENOENT
	}
	if !r.FileInfo.IsDirectory() {
		return nil, fuse.EINVAL
	}

	c := make(chan fuse.DirEntry, len(r.NameModeMap))
	for k, mode := range r.NameModeMap {
		c <- fuse.DirEntry{
			Name: k,
			Mode: uint32(mode),
		}
	}
	close(c)
	return c, fuse.OK
}

type rpcFsFile struct {
	fuse.File
	os.FileInfo
}

func (me *rpcFsFile) GetAttr() (*os.FileInfo, fuse.Status) {
	return &me.FileInfo, fuse.OK
}

func (me *rpcFsFile) String() string {
	return fmt.Sprintf("rpcFsFile(%s)", me.File.String())
}

func (me *RpcFs) Open(name string, flags uint32, context *fuse.Context) (fuse.File, fuse.Status) {
	if flags&fuse.O_ANYWRITE != 0 {
		return nil, fuse.EPERM
	}
	a := me.attr.Get(name)
	if a == nil {
		return nil, fuse.ENOENT
	}
	if a.Deletion() {
		return nil, fuse.ENOENT
	}

	if err := me.FetchHash(a); err != nil {
		log.Printf("Error fetching contents %v", err)
		return nil, fuse.EIO
	}

	if contents := me.cache.ContentsIfLoaded(a.Hash); contents != nil {
		return &fuse.WithFlags{
			File: &rpcFsFile{
				fuse.NewDataFile(contents),
				*a.FileInfo,
			},
			FuseFlags: fuse.FOPEN_KEEP_CACHE,
		}, fuse.OK
	}
	return &fuse.WithFlags{
		File: &rpcFsFile{
			&LazyLoopbackFile{Name: me.cache.Path(a.Hash)},
			*a.FileInfo,
		},
		FuseFlags: fuse.FOPEN_KEEP_CACHE,
	}, fuse.OK
}

func (me *RpcFs) Readlink(name string, context *fuse.Context) (string, fuse.Status) {
	a := me.attr.Get(name)
	if a == nil {
		return "", fuse.ENOENT
	}

	if a.Deletion() {
		return "", fuse.ENOENT
	}
	if !a.FileInfo.IsSymlink() {
		return "", fuse.EINVAL
	}

	// TODO - kick off getattr on destination.
	return a.Link, fuse.OK
}

func (me *RpcFs) GetAttr(name string, context *fuse.Context) (*os.FileInfo, fuse.Status) {
	r := me.attr.Get(name)
	if r == nil {
		return nil, fuse.ENOENT
	}
	if r.Hash != "" {
		go me.FetchHash(r)
	}
	return r.FileInfo, r.Status()
}

func (me *RpcFs) Access(name string, mode uint32, context *fuse.Context) (code fuse.Status) {
	if mode == fuse.F_OK {
		_, code := me.GetAttr(name, context)
		return code
	}
	if mode&fuse.W_OK != 0 {
		return fuse.EACCES
	}
	return fuse.OK
}

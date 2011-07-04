package termite

import (
	"fmt"
	"path/filepath"
	"os"
	"log"
	"io/ioutil"
	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/go-fuse/unionfs"
	"os/user"
	"strings"
	"syscall"
)
type WorkerFuseFs struct {
	rwDir  string
	tmpDir string
	mount  string
	*fuse.MountState
	unionFs *unionfs.UnionFs
}

type WorkerTask struct {
	fuseFs *WorkerFuseFs
	*WorkRequest
	*WorkReply
	masterWorker *MasterWorker
}

const _DELETIONS = "DELETIONS"

func (me *MasterWorker) newWorkerFuseFs() (*WorkerFuseFs, os.Error) {
	w := WorkerFuseFs{}

	tmpDir, err := ioutil.TempDir("", "rpcfs-tmp")
	w.tmpDir = tmpDir
	type dirInit struct {
		dst *string
		val string
	}

	for _, v := range []dirInit{
		dirInit{&w.rwDir, "rw"},
		dirInit{&w.mount, "mnt"},
	} {
		*v.dst = filepath.Join(w.tmpDir, v.val)
		err = os.Mkdir(*v.dst, 0700)
		if err != nil {
			return nil, err
		}
	}

	rwFs := fuse.NewLoopbackFileSystem(w.rwDir)
	roFs := NewRpcFs(me.fileServer, me.daemon.contentCache)

	// High ttl, since all writes come through fuse.
	ttl := 100.0
	opts := unionfs.UnionFsOptions{
		BranchCacheTTLSecs:   ttl,
		DeletionCacheTTLSecs: ttl,
		DeletionDirName:      _DELETIONS,
	}
	mOpts := fuse.FileSystemOptions{
		EntryTimeout:    ttl,
		AttrTimeout:     ttl,
		NegativeTimeout: ttl,
	}

	w.unionFs = unionfs.NewUnionFs("ufs", []fuse.FileSystem{rwFs, roFs}, opts)

	swFs := []fuse.SwitchedFileSystem{
		{"dev", &DevNullFs{}, true},

		// TODO - share RpcFs with writable parts.
		{"", me.readonlyRpcFs, false},

		// TODO - configurable.
		{"tmp", w.unionFs, false},

		// TODO - should use socket location as boundary.
		{me.writableRoot, w.unionFs, false},
	}


	conn := fuse.NewFileSystemConnector(fuse.NewSwitchFileSystem(swFs), &mOpts)
	w.MountState = fuse.NewMountState(conn)
	w.MountState.Mount(w.mount, &fuse.MountOptions{AllowOther: true})
	if err != nil {
		return nil, err
	}

	go w.MountState.Loop(true)

	return &w, nil
}

func (me *WorkerFuseFs) Stop() {
	me.MountState.Unmount()
	os.RemoveAll(me.tmpDir)
}

func (me *MasterWorker) newWorkerTask(req *WorkRequest, rep *WorkReply) (*WorkerTask, os.Error) {
	fuseFs, err := me.getWorkerFuseFs()
	if err != nil {
		return nil, err
	}
	return &WorkerTask{
		WorkRequest: req,
		WorkReply:   rep,
		masterWorker: me,
		fuseFs:      fuseFs,
	}, nil
}

func (me *WorkerTask) Run() os.Error {
	rStdout, wStdout, err := os.Pipe()
	rStderr, wStderr, err := os.Pipe()

	attr := os.ProcAttr{
		Env:   me.WorkRequest.Env,
		Files: []*os.File{nil, wStdout, wStderr},
	}

	nobody, err := user.Lookup("nobody")
	if err != nil {
		return err
	}

	chroot := me.masterWorker.daemon.ChrootBinary
	cmd := []string{chroot, "-dir", me.WorkRequest.Dir,
		"-uid", fmt.Sprintf("%d", nobody.Uid), "-gid", fmt.Sprintf("%d", nobody.Gid),
		"-binary", me.WorkRequest.Binary,
		me.fuseFs.mount}

	newcmd := make([]string, len(cmd)+len(me.WorkRequest.Argv))
	copy(newcmd, cmd)
	copy(newcmd[len(cmd):], me.WorkRequest.Argv)

	log.Println("starting cmd", newcmd)
	proc, err := os.StartProcess(chroot, newcmd, &attr)
	if err != nil {
		log.Println("Error", err)
		return err
	}

	wStdout.Close()
	wStderr.Close()

	stdout, err := ioutil.ReadAll(rStdout)
	stderr, err := ioutil.ReadAll(rStderr)

	me.WorkReply.Exit, err = proc.Wait(0)
	me.WorkReply.Stdout = string(stdout)
	me.WorkReply.Stderr = string(stderr)

	// TODO - look at rw directory, and serialize the files into WorkReply.
	err = me.fillReply()
	if err != nil {
		me.fuseFs.Stop()
		// TODO - anything else needed to discard?
	} else {
		me.masterWorker.ReturnFuse(me.fuseFs)
	}

	return err
}

func (me *WorkerTask) VisitFile(path string, osInfo *os.FileInfo) {
	fi := FileInfo{
		FileInfo: *osInfo,
	}

	ftype := osInfo.Mode &^ 07777
	switch ftype {
	case syscall.S_IFREG:
		fi.Hash = me.masterWorker.daemon.contentCache.SavePath(path)
	case syscall.S_IFLNK:
		val, err := os.Readlink(path)
		if err != nil {
			// TODO - fail rpc.
			log.Fatal("Readlink error.")
		}
		fi.LinkContent = val
	default:
		log.Fatalf("Unknown file type %o", ftype)
	}

	me.savePath(path, fi)

	// TODO - error handling.
	os.Remove(path)
}

func (me *WorkerTask) savePath(path string, fi FileInfo) {
	if !strings.HasPrefix(path, me.fuseFs.rwDir) {
		log.Println("Weird file", path)
		return
	}

	fi.Path = path[len(me.fuseFs.rwDir):]
	if fi.Path == "/" + _DELETIONS {
		return
	}

	me.WorkReply.Files = append(me.WorkReply.Files, fi)
}

func (me *WorkerTask) VisitDir(path string, osInfo *os.FileInfo) bool {
	me.savePath(path, FileInfo{FileInfo: *osInfo})

	// TODO - save dir to delete.
	return true
}

func (me *WorkerTask) fillReply() os.Error {
	dir := filepath.Join(me.fuseFs.rwDir, _DELETIONS)
	_, err := os.Lstat(dir)
	if err == nil {
		matches, err := filepath.Glob(dir + "/*")
		if err != nil {
			return err
		}

		for _, m := range matches {
			fullPath := filepath.Join(dir, m)
			contents, err := ioutil.ReadFile(fullPath)
			if err != nil {
				return err
			}

			me.WorkReply.Files = append(me.WorkReply.Files, FileInfo{
				Delete: true,
				Path:   string(contents),
			})
			err = os.Remove(fullPath)
			if err != nil {
				return err
			}
		}

		if err != nil {
			return err
		}
	}
	filepath.Walk(me.fuseFs.rwDir, me, nil)
	return nil
}
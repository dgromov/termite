package termite

import (
	"fmt"
	"github.com/hanwen/go-fuse/fuse"
	"http"
	"io/ioutil"
	"log"
	"os"
	"rand"
	"rpc"
	"testing"
	"time"
)

type testCase struct {
	worker          *WorkerDaemon
	master          *Master
	coordinator     *Coordinator
	secret          []byte
	tmp             string
	socket          string
	coordinatorPort int
	workerPort      int
	tester          *testing.T
}

func testEnv() []string {
	return []string{
		"PATH=/bin:/usr/bin",
		"USER=nobody",
	}
}

func NewTestCase(t *testing.T) *testCase {
	me := new(testCase)
	me.tester = t
	me.secret = RandomBytes(20)
	me.tmp, _ = ioutil.TempDir("", "")

	workerTmp := me.tmp + "/worker-tmp"
	os.Mkdir(workerTmp, 0700)
	me.worker = NewWorkerDaemon(me.secret, workerTmp,
		me.tmp+"/worker-cache", 1)

	// TODO - pick unused port
	me.coordinatorPort = int(rand.Int31n(60000) + 1024)
	c := NewCoordinator(me.secret)
	rpc.Register(c)
	rpc.HandleHTTP()
	go c.PeriodicCheck()

	coordinatorAddr := fmt.Sprintf(":%d", me.coordinatorPort)
	go http.ListenAndServe(coordinatorAddr, nil)
	// TODO - can we do without the sleeps?
	time.Sleep(0.1e9) // wait for daemon to start up

	me.workerPort = int(rand.Int31n(60000) + 1024)
	go me.worker.RunWorkerServer(me.workerPort, coordinatorAddr)

	// wait worker to be registered on coordinator.
	time.Sleep(0.1e9)

	masterCache := NewContentCache(me.tmp + "/master-cache")
	me.master = NewMaster(
		masterCache, coordinatorAddr,
		[]string{},
		me.secret, []string{}, 1)

	me.master.SetKeepAlive(1.0)
	me.socket = me.tmp + "/master-socket"
	go me.master.Start(me.socket)

	wd := me.tmp + "/wd"
	os.MkdirAll(wd, 0755)
	time.Sleep(0.1e9) // wait for all daemons to start up
	return me
}

func (me *testCase) Clean() {
	me.master.mirrors.dropConnections()
	// TODO - should have explicit worker shutdown routine.
	time.Sleep(0.1e9)
	os.RemoveAll(me.tmp)
}

func (me *testCase) Run(req WorkRequest) (rep WorkReply) {
	rpcConn := OpenSocketConnection(me.socket, RPC_CHANNEL)
	client := rpc.NewClient(rpcConn)

	err := client.Call("LocalMaster.Run", &req, &rep)
	if err != nil {
		me.tester.Fatal("LocalMaster.Run: ", err)
	}
	return rep
}

// Simple end-to-end test.  It skips the chroot, but should give a
// basic assurance that things work as expected.
func TestEndToEndBasic(t *testing.T) {
	if os.Geteuid() == 0 {
		log.Println("This test should not run as root")
		return
	}

	tc := NewTestCase(t)
	defer tc.Clean()

	req := WorkRequest{
		StdinId: ConnectionId(),
		Binary:  "/usr/bin/tee",
		Argv:    []string{"/usr/bin/tee", "output.txt"},
		Env:     testEnv(),

		// Will not be filtered, since /tmp/foo is more
		// specific than /tmp
		Dir:   tc.tmp + "/wd",
		Debug: true,
	}

	// TODO - should separate dial/listen in the daemons?
	stdinConn := OpenSocketConnection(tc.socket, req.StdinId)
	go func() {
		stdinConn.Write([]byte("hello"))
		stdinConn.Close()
	}()

	tc.Run(req)
	content, err := ioutil.ReadFile(tc.tmp + "/wd/output.txt")
	if err != nil {
		t.Error(err)
	}
	if string(content) != "hello" {
		t.Error("content:", content)
	}

	tc.Run(WorkRequest{
		Binary: "/bin/rm",
		Argv:   []string{"/bin/rm", "output.txt"},
		Env:     testEnv(),
		Dir:    tc.tmp + "/wd",
		Debug:  true,
	})

	if fi, _ := os.Lstat(tc.tmp + "/wd/output.txt"); fi != nil {
		t.Error("file should have been deleted", fi)
	}

	// Test keepalive.
	time.Sleep(2e9)

	statusReq := &WorkerStatusRequest{}
	statusRep := &WorkerStatusResponse{}
	tc.worker.Status(statusReq, statusRep)
	if len(statusRep.MirrorStatus) > 0 {
		t.Error("Processes still alive.")
	}
}

// This shows a case that is not handled correctly yet: we have no way
// to flush the cache on negative entries.
func TestEndToEndNegativeNotify(t *testing.T) {
	if os.Geteuid() == 0 {
		log.Println("This test should not run as root")
		return
	}

	tc := NewTestCase(t)
	defer tc.Clean()

	rep := tc.Run(WorkRequest{
		Binary: "/bin/cat",
		Argv:   []string{"/bin/cat", "output.txt"},
		Env:     testEnv(),
		Dir:    tc.tmp + "/wd",
		Debug:  true,
	})

	if rep.Exit.ExitStatus() == 0 {
		t.Fatal("expect exit status != 0")
	}

	newContent := []byte("new content")
	hash := tc.master.cache.Save(newContent)
	updated := []FileAttr{
		FileAttr{
			Path:     tc.tmp + "/wd/output.txt",
			FileInfo: &os.FileInfo{Mode: fuse.S_IFREG | 0644, Size: int64(len(newContent))},
			Hash:     hash,
			Content:  newContent,
		},
	}
	tc.master.mirrors.queueFiles(nil, updated)

	rep = tc.Run(WorkRequest{
		Binary: "/bin/cat",
		Argv:   []string{"/bin/cat", "output.txt"},
		Env:    testEnv(),
		Dir:    tc.tmp + "/wd",
		Debug:  true,
	})

	if rep.Exit.ExitStatus() != 0 {
		t.Fatal("expect exit status == 0", rep.Exit.ExitStatus())
	}
	log.Println("new content:", rep.Stdout)
	if string(rep.Stdout) != string(newContent) {
		t.Error("Mismatch", string(rep.Stdout), string(newContent))
	}
}

func TestEndToEndMove(t *testing.T) {
	if os.Geteuid() == 0 {
		log.Println("This test should not run as root")
		return
	}

	tc := NewTestCase(t)
	defer tc.Clean()

	rep := tc.Run(WorkRequest{
		Binary: "/bin/mkdir",
		Argv:   []string{"/bin/mkdir", "-p", "a/b/c"},
		Env:    testEnv(),
		Dir:    tc.tmp + "/wd",
	})
	if rep.Exit.ExitStatus() != 0 {
		t.Fatalf("mkdir should exit cleanly. Rep %v", rep)
	}
	rep = tc.Run(WorkRequest{
		Binary: "/bin/mv",
		Argv:   []string{"/bin/mv", "a", "q"},
		Env:    testEnv(),
		Dir:    tc.tmp + "/wd",
	})
	if rep.Exit.ExitStatus() != 0 {
		t.Fatalf("mv should exit cleanly. Rep %v", rep)
	}

	if fi, err := os.Lstat(tc.tmp + "/wd/q/b/c"); err != nil || !fi.IsDirectory() {
		t.Errorf("dir should have been moved. Err %v, fi %v", err, fi)
	}
}

func TestEndToEndSymlink(t *testing.T) {
	if os.Geteuid() == 0 {
		log.Println("This test should not run as root")
		return
	}

	tc := NewTestCase(t)
	defer tc.Clean()

	err := os.Symlink("oldlink", tc.tmp + "/wd/symlink")
	if err != nil {
		t.Fatal("oldlink symlink", err)
	}

	rep := tc.Run(WorkRequest{
		Binary: "/bin/touch",
		Argv:   []string{"/bin/touch", "file.txt"},
		Env:    testEnv(),
		Dir:    tc.tmp + "/wd",
	})

	if fi, err := os.Lstat(tc.tmp + "/wd/file.txt"); err != nil || !fi.IsRegular() || fi.Size != 0 {
		t.Fatalf("wd/file.txt was not created. Err: %v, fi: %v", err, fi)
	}
	if rep.Exit.ExitStatus() != 0 {
		t.Fatalf("touch should exit cleanly. Rep %v", rep)
	}
	rep = tc.Run(WorkRequest{
		Binary: "/bin/ln",
		Argv:   []string{"/bin/ln", "-sf", "foo", "symlink"},
		Env:    testEnv(),
		Dir:    tc.tmp + "/wd",
	})
	if rep.Exit.ExitStatus() != 0 {
		t.Fatalf("ln -s should exit cleanly. Rep %v", rep)
	}

	if fi, err := os.Lstat(tc.tmp + "/wd/symlink"); err != nil || !fi.IsSymlink() {
		t.Errorf("should have symlink. Err %v, fi %v", err, fi)
	}
}

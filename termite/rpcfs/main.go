package main

import (
	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/go-fuse/rpcfs"
	"fmt"
	"flag"
	"log"
	"os"
	"rpc"
)

var _ = log.Printf

func main() {
	cachedir := flag.String("cachedir", "/tmp/rpcfs-cache", "content cache")
	server := flag.String("server", "localhost:1234", "file server")
	secret := flag.String("secret", "secr3t", "shared password for authentication")

	flag.Parse()
	if flag.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "usage: %s MOUNTPOINT\n", os.Args[0])
		os.Exit(2)
	}

	rpcConn, err := rpcfs.SetupClient(*server, []byte(*secret))
	if err != nil {
		log.Fatal("dialing:", err)
	}

	var fs fuse.FileSystem
	fs = rpcfs.NewRpcFs(rpc.NewClient(rpcConn), *cachedir)
	conn := fuse.NewFileSystemConnector(fs, nil)
	state := fuse.NewMountState(conn)
	opts := fuse.MountOptions{}
	if os.Geteuid() == 0 {
		opts.AllowOther = true
	}

	state.Mount(flag.Arg(0), &opts)
	if err != nil {
		fmt.Printf("Mount fail: %v\n", err)
		os.Exit(1)
	}

	state.Debug = true
	state.Loop(false)
}

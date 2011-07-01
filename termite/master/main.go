package main

import (
	"flag"
	"log"
	"github.com/hanwen/go-fuse/rpcfs"
	"io/ioutil"
	"strings"
)

func main() {
	cachedir := flag.String("cachedir", "/tmp/fsserver-cache", "content cache")
	serverAddress := flag.String("fileserver", "localhost:1234", "local file server")
	workers := flag.String("workers", "localhost:1235", "comma separated list of worker addresses")
	socket := flag.String("socket", "/tmp/termite-socket", "socket to listen for commands")
	exclude := flag.String("exclude", "/proc", "prefixes to not export.")
	secretFile := flag.String("secret", "/tmp/secret.txt", "file containing password.")

	flag.Parse()
	secret, err := ioutil.ReadFile(*secretFile)
	if err != nil {
		log.Fatal("ReadFile", err)
	}

	workerList := strings.Split(*workers, ",", -1)
	excludeList := strings.Split(*exclude, ",", -1)
	master := rpcfs.NewMaster(
		*cachedir, workerList, secret, excludeList)
	master.Start(*serverAddress, *socket)
}




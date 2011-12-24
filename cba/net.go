package cba

import (
	"io"
	"log"
	"net/rpc"
	"os"
)

// Client is a thread-safe interface to fetching over a connection.
type Client struct {
	store  *Store
	client *rpc.Client
}

func (store *Store) NewClient(conn io.ReadWriteCloser) *Client {
	return &Client{
		store:  store,
		client: rpc.NewClient(conn),
	}
}

func (c *Client) Close() {
	c.client.Close()
}

func (c *Store) ServeConn(conn io.ReadWriteCloser) {
	s := Server{c}
	rpcServer := rpc.NewServer()
	rpcServer.Register(&s)
	rpcServer.ServeConn(conn)
	conn.Close()
}

type Server struct {
	store *Store
}

func (s *Server) ServeChunk(req *Request, rep *Response) (err error) {
	e := s.store.ServeChunk(req, rep)
	return e
}

func (st *Store) ServeChunk(req *Request, rep *Response) (err error) {
	if !st.HasHash(req.Hash) {
		rep.Have = false
		return nil
	}

	rep.Have = true
	if c := st.ContentsIfLoaded(req.Hash); c != nil {
		if req.End > len(c) {
			req.End = len(c)
		}
		rep.Chunk = c[req.Start:req.End]
		rep.Size = len(c)
		return nil
	}

	f, err := os.Open(st.Path(req.Hash))
	if err != nil {
		return err
	}
	defer f.Close()

	rep.Chunk = make([]byte, req.End-req.Start)
	n, err := f.ReadAt(rep.Chunk, int64(req.Start))
	rep.Chunk = rep.Chunk[:n]
	rep.Size = n

	if err == io.EOF {
		err = nil
	}
	return err
}

func (c *Client) Fetch(want string) (bool, error) {
	chunkSize := 1 << 18
	buf := make([]byte, chunkSize)

	var output *HashWriter
	written := 0

	var saved string
	for {

		req := &Request{
			Hash:  want,
			Start: written,
			End:   written + chunkSize,
		}
		rep := &Response{Chunk: buf}
		err := c.client.Call("Server.ServeChunk", req, rep)
		if err != nil || !rep.Have {
			return false, err
		}

		// is this a bug in the rpc package?
		content := rep.Chunk[:rep.Size]

		if len(content) < chunkSize && written == 0 {
			saved = c.store.Save(content)
			break
		} else if output == nil {
			output = c.store.NewHashWriter()
			defer output.Close()
		}

		n, err := output.Write(content)
		written += n
		if err != nil {
			return false, err
		}
		if len(content) < chunkSize {
			break
		}
	}
	if output != nil {
		output.Close()
		saved = string(output.Sum())
	}
	if want != saved {
		log.Fatalf("file corruption: got %x want %x", saved, want)
	}
	return true, nil
}
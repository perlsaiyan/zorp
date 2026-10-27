package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"
)

// Handler answers one request. It may block (e.g. a sync); each connection
// is served on its own goroutine.
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, error)

type Server struct {
	handler Handler
	log     *slog.Logger

	mu   sync.Mutex
	subs map[*conn]struct{}
}

func NewServer(h Handler, log *slog.Logger) *Server {
	return &Server{handler: h, log: log, subs: map[*conn]struct{}{}}
}

// Serve accepts connections until ctx is done or the listener closes.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.serveConn(ctx, nc)
		}()
	}
}

// Broadcast sends an event to every subscribed connection. Slow or dead
// subscribers are dropped rather than allowed to block the daemon.
func (s *Server) Broadcast(name string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		s.log.Error("encoding event", "event", name, "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.subs {
		if err := c.write(message{Event: name, Data: raw}); err != nil {
			delete(s.subs, c)
			c.nc.Close()
		}
	}
}

type conn struct {
	nc  net.Conn
	wmu sync.Mutex
	enc *json.Encoder
}

const writeTimeout = 2 * time.Second

func (c *conn) write(m message) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.nc.SetWriteDeadline(time.Now().Add(writeTimeout))
	return c.enc.Encode(m)
}

func (s *Server) serveConn(ctx context.Context, nc net.Conn) {
	c := &conn{nc: nc, enc: json.NewEncoder(nc)}
	defer func() {
		s.mu.Lock()
		delete(s.subs, c)
		s.mu.Unlock()
		nc.Close()
	}()
	stop := context.AfterFunc(ctx, func() { nc.Close() })
	defer stop()

	sc := bufio.NewScanner(nc)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var req message
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			c.write(message{Error: "malformed request: " + err.Error()})
			continue
		}
		if req.Method == MethodSubscribe {
			s.mu.Lock()
			s.subs[c] = struct{}{}
			s.mu.Unlock()
			c.write(message{ID: req.ID, Result: json.RawMessage(`true`)})
			continue
		}
		resp := message{ID: req.ID}
		result, err := s.handler(ctx, req.Method, req.Params)
		if err != nil {
			resp.Error = err.Error()
		} else if resp.Result, err = json.Marshal(result); err != nil {
			resp.Error = "encoding result: " + err.Error()
		}
		if err := c.write(resp); err != nil {
			return
		}
	}
}

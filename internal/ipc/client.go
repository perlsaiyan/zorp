package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
)

// Client is a connection to the daemon. Calls are serialized; use a separate
// Client for Subscribe.
type Client struct {
	nc     net.Conn
	sc     *bufio.Scanner
	mu     sync.Mutex
	nextID int64
}

// RemoteError is an error returned by the daemon.
type RemoteError string

func (e RemoteError) Error() string { return string(e) }

func Dial(ctx context.Context, socket string) (*Client, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(nc)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	return &Client{nc: nc, sc: sc}, nil
}

func (c *Client) Close() error { return c.nc.Close() }

// Call sends a request and decodes the result into result (which may be nil).
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.nextID++
	req := message{ID: c.nextID, Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		req.Params = raw
	}
	stop := context.AfterFunc(ctx, func() { c.nc.Close() })
	defer stop()

	if err := json.NewEncoder(c.nc).Encode(req); err != nil {
		return c.ctxErr(ctx, err)
	}
	for {
		m, err := c.read()
		if err != nil {
			return c.ctxErr(ctx, err)
		}
		if m.Event != "" || m.ID != req.ID {
			continue // events on a subscribed conn, or a stale reply
		}
		if m.Error != "" {
			return RemoteError(m.Error)
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(m.Result, result)
	}
}

// Subscribe asks for events and returns a channel of them. The channel closes
// when the connection does; the Client must not be used for Calls afterwards.
func (c *Client) Subscribe(ctx context.Context) (<-chan Event, error) {
	if err := c.Call(ctx, MethodSubscribe, nil, nil); err != nil {
		return nil, err
	}
	ch := make(chan Event, 16)
	go func() {
		defer close(ch)
		stop := context.AfterFunc(ctx, func() { c.nc.Close() })
		defer stop()
		for {
			m, err := c.read()
			if err != nil {
				return
			}
			if m.Event == "" {
				continue
			}
			select {
			case ch <- Event{Name: m.Event, Data: m.Data}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

func (c *Client) read() (message, error) {
	var m message
	if !c.sc.Scan() {
		if err := c.sc.Err(); err != nil {
			return m, err
		}
		return m, errors.New("daemon closed the connection")
	}
	if err := json.Unmarshal(c.sc.Bytes(), &m); err != nil {
		return m, fmt.Errorf("malformed reply from daemon: %w", err)
	}
	return m, nil
}

func (c *Client) ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestCallAndSubscribe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sock := filepath.Join(t.TempDir(), "zorp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		switch method {
		case MethodStatus:
			return Status{State: StateIdle, PID: 42}, nil
		case MethodSync:
			var p SyncParams
			json.Unmarshal(params, &p)
			if p.Full {
				return nil, errors.New("not today")
			}
			return "ok", nil
		}
		return nil, errors.New("unknown")
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go srv.Serve(ctx, ln)

	c, err := Dial(ctx, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var st Status
	if err := c.Call(ctx, MethodStatus, nil, &st); err != nil || st.PID != 42 {
		t.Fatalf("status = %+v, %v", st, err)
	}
	err = c.Call(ctx, MethodSync, SyncParams{Full: true}, nil)
	var rerr RemoteError
	if !errors.As(err, &rerr) || rerr != "not today" {
		t.Fatalf("remote error = %v", err)
	}

	sub, err := Dial(ctx, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	events, err := sub.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	srv.Broadcast(EventChanged, Changed{FolderIDs: []int64{7}})
	select {
	case ev := <-events:
		var ch Changed
		if ev.Name != EventChanged || json.Unmarshal(ev.Data, &ch) != nil || len(ch.FolderIDs) != 1 || ch.FolderIDs[0] != 7 {
			t.Errorf("event = %s %s", ev.Name, ev.Data)
		}
	case <-ctx.Done():
		t.Fatal("no event received")
	}
}

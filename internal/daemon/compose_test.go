package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/store"
)

func TestSafeModeOnlySendsToSelf(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "zorp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.UpsertAccount(context.Background(), "Tom@Example.com", "Tom"); err != nil {
		t.Fatal(err)
	}
	d := &daemon{cfg: &config.Config{Safety: config.Safety{SafeMode: true}}, store: st}
	self := ipc.Address{Addr: "tom@example.com"}
	other := ipc.Address{Addr: "alice@example.com"}
	for _, tc := range []struct {
		p  ipc.ComposeParams
		ok bool
	}{
		{ipc.ComposeParams{To: []ipc.Address{self}}, true},
		{ipc.ComposeParams{To: []ipc.Address{self}, Cc: []ipc.Address{self}}, true},
		{ipc.ComposeParams{To: []ipc.Address{other}}, false},
		{ipc.ComposeParams{To: []ipc.Address{self}, Bcc: []ipc.Address{other}}, false},
		{ipc.ComposeParams{To: []ipc.Address{self}, Cc: []ipc.Address{other}}, false},
	} {
		err := d.checkRecipients(context.Background(), tc.p)
		if (err == nil) != tc.ok {
			t.Errorf("%+v: err=%v", tc.p, err)
		}
	}

	// The compose handler applies it before doing anything (d has no engine,
	// so reaching Graph would panic).
	params, _ := json.Marshal(ipc.ComposeParams{Kind: "new", Send: true, To: []ipc.Address{other}})
	if _, err := d.compose(context.Background(), params); err == nil || !strings.Contains(err.Error(), "guardrail") {
		t.Errorf("compose: %v", err)
	}
	params, _ = json.Marshal(ipc.ComposeParams{Kind: "reply", Send: true, To: []ipc.Address{self}})
	if _, err := d.compose(context.Background(), params); err == nil || !strings.Contains(err.Error(), "needs the message") {
		t.Errorf("reply without ref: %v", err)
	}
}

func TestSendGuardrailSettings(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "zorp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.UpsertAccount(context.Background(), "tom@example.com", "Tom")
	yes := true
	colleague := ipc.ComposeParams{To: []ipc.Address{{Addr: "bob@example.com"}}, Cc: []ipc.Address{{Addr: "x@other.org"}}}

	d := &daemon{cfg: &config.Config{Safety: config.Safety{SafeMode: true, SendAllow: []string{"example.com"}}}, store: st}
	err = d.checkRecipients(context.Background(), colleague)
	if err == nil || !strings.Contains(err.Error(), "x@other.org") || strings.Contains(err.Error(), "bob@") {
		t.Errorf("send_allow: %v", err)
	}
	d.cfg.Safety.AllowSend = &yes
	if err := d.checkRecipients(context.Background(), colleague); err != nil {
		t.Errorf("allow_send: %v", err)
	}
}

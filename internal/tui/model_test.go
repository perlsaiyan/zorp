package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/tom/zorp/config"
	"github.com/tom/zorp/internal/store"
)

func newTestModel(t *testing.T) *Model {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "zorp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	acct, err := st.UpsertAccount(ctx, "test@example.com", "Test")
	if err != nil {
		t.Fatal(err)
	}
	err = st.ReplaceFolders(ctx, acct, []store.Folder{
		{GraphID: "inbox", Name: "Inbox", WellKnown: "inbox"},
		{GraphID: "sent", Name: "Sent Items", WellKnown: "sentitems"},
		{GraphID: "zz", Name: "Zebra"},
		{GraphID: "aa", Name: "Apples"},
		{GraphID: "kid", Name: "Child", ParentGraphID: "inbox"},
	})
	if err != nil {
		t.Fatal(err)
	}
	folders, _ := st.Folders(ctx, acct)
	ids := map[string]int64{}
	for _, f := range folders {
		ids[f.GraphID] = f.ID
		if f.GraphID == "inbox" || f.GraphID == "sent" {
			st.SetDeltaLink(ctx, f.ID, "x", time.Now()) // mark as synced
		}
	}
	var msgs []store.Message
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for i := range 50 {
		msgs = append(msgs, store.Message{
			GraphID:    fmt.Sprintf("m%02d", i),
			Subject:    fmt.Sprintf("Subject number %d with a fairly long title that will need truncating", i),
			From:       store.Address{Name: fmt.Sprintf("Sender %d", i), Addr: "s@example.com"},
			ReceivedAt: base.Add(time.Duration(i) * time.Hour),
			IsRead:     i%3 == 0,
		})
	}
	// Hostile content: escape sequences and wide characters.
	msgs = append(msgs, store.Message{
		GraphID: "evil", Subject: "\x1b]52;c;cHduZWQ=\x07Hi \x1b[2J日本語テキスト", ReceivedAt: base.Add(100 * time.Hour),
		From: store.Address{Name: "\x1b[31mMallory\x1b[0m"},
	})
	if err := st.ApplyDelta(ctx, acct, ids["inbox"], store.DeltaBatch{Upserts: msgs}); err != nil {
		t.Fatal(err)
	}
	// Marketing-style invisible padding (soft hyphen, ZWNJ, CGJ) and a bidi override.
	padding := strings.Repeat("\u034f\u00ad\u200c   ", 80)
	st.SetBody(ctx, acct, "evil", "line one\n\x1b[2Jline two\twith tab\n"+padding+"\n"+padding+"\n\u202eevil.exe")

	cfg := &config.Config{}
	m, err := New(ctx, cfg, st, store.Account{ID: acct, UPN: "test@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func press(m *Model, keys ...string) {
	for _, k := range keys {
		m.handleKey(k)
	}
}

// deliver runs a command and feeds its message back, as the runtime would
// (only for commands that finish quickly and don't need the daemon).
func deliver(m *Model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	if msg := cmd(); msg != nil {
		if _, isBatch := msg.(tea.BatchMsg); !isBatch {
			m.Update(msg)
		}
	}
}

func TestViewFitsEveryTerminalSize(t *testing.T) {
	m := newTestModel(t)
	sizes := [][2]int{{0, 0}, {1, 1}, {20, 3}, {40, 10}, {59, 20}, {80, 24}, {120, 40}, {250, 70}}
	for _, lay := range []layout{previewRight, previewBottom, previewHidden} {
		for _, reading := range []bool{false, true} {
			for _, sz := range sizes {
				m.layout, m.reading = lay, reading
				m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
				content := m.View().Content
				if sz[0] == 0 {
					continue
				}
				lines := strings.Split(content, "\n")
				if len(lines) > sz[1] {
					t.Errorf("layout %d reading=%v %dx%d: %d lines", lay, reading, sz[0], sz[1], len(lines))
				}
				for i, l := range lines {
					if w := ansi.StringWidth(l); w != sz[0] {
						t.Errorf("layout %d reading=%v %dx%d: line %d is %d wide: %q", lay, reading, sz[0], sz[1], i, w, ansi.Strip(l))
						break
					}
				}
			}
		}
	}
}

func TestNavigationAndScrolling(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	if len(m.msgs) != 51 || m.msgs[0].GraphID != "evil" {
		t.Fatalf("expected newest-first inbox, got %d msgs, first %q", len(m.msgs), m.msgs[0].GraphID)
	}
	press(m, "j", "j", "j")
	if m.msgCur != 3 {
		t.Errorf("cursor = %d, want 3", m.msgCur)
	}
	press(m, "G")
	if m.msgCur != 50 {
		t.Errorf("G: cursor = %d, want 50", m.msgCur)
	}
	h := m.geom().listH
	if m.msgTop != 51-h {
		t.Errorf("msgTop = %d, want %d (cursor visible at bottom)", m.msgTop, 51-h)
	}
	press(m, "g", "g")
	if m.msgCur != 0 || m.msgTop != 0 {
		t.Errorf("gg: cursor=%d top=%d", m.msgCur, m.msgTop)
	}
	press(m, "k")
	if m.msgCur != 0 {
		t.Errorf("k at top moved cursor to %d", m.msgCur)
	}
}

func TestFolderTreeOrderAndSwitching(t *testing.T) {
	m := newTestModel(t)
	var names []string
	for _, f := range m.folders {
		names = append(names, strings.Repeat(">", f.depth)+f.Name)
	}
	want := "Inbox >Child Sent Items Apples Zebra"
	if got := strings.Join(names, " "); got != want {
		t.Errorf("folder order = %q, want %q", got, want)
	}
	if m.folders[0].unread == 0 {
		t.Error("inbox unread count should come from the local cache")
	}

	press(m, "g", "s")
	if f, _ := m.currentFolder(); f.WellKnown != "sentitems" || len(m.msgs) != 0 {
		t.Errorf("g s: folder %q with %d msgs", f.Name, len(m.msgs))
	}
	press(m, "g", "i")
	if f, _ := m.currentFolder(); f.WellKnown != "inbox" || len(m.msgs) != 51 {
		t.Errorf("g i: folder %q with %d msgs", f.Name, len(m.msgs))
	}

	// Unsynced folder shows a hint instead of an empty list.
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	m.focus = paneFolders
	m.folderCur = len(m.folders) - 1 // Zebra
	press(m, "enter")
	if !strings.Contains(ansi.Strip(m.View().Content), "isn't synced") {
		t.Error("unsynced folder hint missing")
	}
}

func TestUntrustedTextIsSanitized(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	deliver(m, m.selectMessage()) // reads the body for "evil"
	if m.bodyState != bodyReady {
		t.Fatalf("body state = %v", m.bodyState)
	}
	content := m.View().Content
	for _, bad := range []string{"\x1b]52", "\x1b[2J", "\x07", "\x1b[31mMallory"} {
		if strings.Contains(content, bad) {
			t.Errorf("view contains raw sequence %q", bad)
		}
	}
	plain := ansi.Strip(content)
	for _, bad := range []string{"\u00ad", "\u200c", "\u034f", "\u202e"} {
		if strings.Contains(content, bad) {
			t.Errorf("view contains invisible character %U", []rune(bad)[0])
		}
	}
	for _, want := range []string{"Mallory", "日本語", "line two    with tab", "evil.exe"} {
		if !strings.Contains(plain, want) {
			t.Errorf("view missing %q", want)
		}
	}
}

func TestReaderOpensAndPages(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	press(m, "j", "enter")
	if !m.reading {
		t.Fatal("enter should open the reader")
	}
	press(m, "n")
	if m.msgCur != 2 {
		t.Errorf("n: cursor = %d, want 2", m.msgCur)
	}
	press(m, "q")
	if m.reading {
		t.Error("q should close the reader, not quit")
	}
}

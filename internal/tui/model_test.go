package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/compose"
	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/render"
	"github.com/perlsaiyan/zorp/internal/store"
	"github.com/perlsaiyan/zorp/internal/termgfx"
	"github.com/perlsaiyan/zorp/internal/theme"
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
	st.SetBody(ctx, acct, "evil", store.Body{Type: "text", Text: "line one\n\x1b[2Jline two\twith tab\n" + padding + "\n" + padding + "\n\u202eevil.exe"})

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

func TestMovePicker(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	press(m, "m")
	if m.picker == nil {
		t.Fatal("m should open the picker")
	}
	if len(m.picker.matches) != len(m.folders)-1 {
		t.Errorf("picker should list every folder but the current one: %d of %d", len(m.picker.matches), len(m.folders))
	}
	press(m, "z", "b") // fuzzy: Zebra
	if len(m.picker.matches) != 1 || m.picker.matches[0].label != "Zebra" {
		t.Fatalf("filter 'zb' = %+v", m.picker.matches)
	}
	// The view must still fit while the picker is open.
	for i, l := range strings.Split(m.View().Content, "\n") {
		if w := ansi.StringWidth(l); w != 100 {
			t.Errorf("line %d width %d", i, w)
		}
	}
	press(m, "backspace", "backspace", "x", "y", "q")
	if m.picker == nil || len(m.picker.matches) != 0 {
		t.Error("typing q in the picker should filter, not quit")
	}
	press(m, "esc")
	if m.picker != nil {
		t.Error("esc should close the picker")
	}
	press(m, "m", "z")
	if cmd := m.handleKey("enter"); cmd == nil || m.picker != nil {
		t.Error("enter should close the picker and send a move")
	}
}

func TestActionKeysProduceCommands(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	for _, k := range []string{"u", "f", "d", "a"} {
		if cmd := m.handleKey(k); cmd == nil {
			t.Errorf("%s produced no command", k)
		}
	}
	// Opening an unread message marks it read; opening a read one doesn't.
	m.msgCur = 1 // m49: unread (49 % 3 != 0)
	if cmd := m.handleKey("enter"); cmd == nil {
		t.Error("opening an unread message should mark it read")
	}
	m.reading = false
	m.msgCur = 2 // m48: read
	if cmd := m.handleKey("enter"); cmd != nil {
		t.Error("opening a read message shouldn't send anything")
	}
	// In the folder pane these keys don't act on messages.
	m.reading, m.focus = false, paneFolders
	if cmd, ok := m.actionKey("d"); ok || cmd != nil {
		t.Error("d in the folder pane should not delete")
	}
}

func TestDragDividersAndClick(t *testing.T) {
	m := newTestModel(t)
	m.cfg.UI.Mouse = true
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 30})
	g := m.geom()
	listX := g.folderW + 1
	divX := listX + g.listW

	if d := m.hitDivider(divX, 5); d != previewDivider {
		t.Fatalf("hit at preview divider = %v", d)
	}
	m.Update(tea.MouseClickMsg{X: divX, Y: 5, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: divX - 30, Y: 5, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: divX - 30, Y: 5, Button: tea.MouseLeft})
	if g2 := m.geom(); g2.listW != g.listW-30 || g2.previewW != g.previewW+30 {
		t.Errorf("after drag list=%d preview=%d, want %d and %d", g2.listW, g2.previewW, g.listW-30, g.previewW+30)
	}
	if m.dragging != noDivider {
		t.Error("release should end the drag")
	}

	// Folder divider, dragged absurdly far: clamped.
	m.Update(tea.MouseClickMsg{X: g.folderW, Y: 3, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: 500, Y: 3, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: 500, Y: 3})
	if fw := m.geom().folderW; fw != 80 {
		t.Errorf("folder width = %d, want clamped to half the screen (80)", fw)
	}
	for i, l := range strings.Split(m.View().Content, "\n") {
		if w := ansi.StringWidth(l); w != 160 {
			t.Errorf("after resizing, line %d is %d wide", i, w)
		}
	}

	// Click a message row selects it; the wheel moves the cursor.
	g = m.geom()
	m.Update(tea.MouseClickMsg{X: g.folderW + 5, Y: 1 + 4, Button: tea.MouseLeft})
	if m.msgCur != 4 || m.focus != paneList {
		t.Errorf("click selected %d (focus %v), want 4", m.msgCur, m.focus)
	}
	m.Update(tea.MouseWheelMsg{X: g.folderW + 5, Y: 3, Button: tea.MouseWheelDown})
	if m.msgCur != 7 {
		t.Errorf("wheel: cursor %d, want 7", m.msgCur)
	}

	// Bottom layout: horizontal divider.
	m.layout = previewBottom
	m.refreshPreview(false)
	g = m.geom()
	if d := m.hitDivider(g.folderW+10, 1+g.listH); d != bottomDivider {
		t.Fatalf("bottom divider not hit: %v", d)
	}
	m.Update(tea.MouseClickMsg{X: g.folderW + 10, Y: 1 + g.listH, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: g.folderW + 10, Y: 1 + g.listH + 5, Button: tea.MouseLeft})
	if m.geom().listH != g.listH+5 {
		t.Errorf("bottom drag: listH %d, want %d", m.geom().listH, g.listH+5)
	}
}

func TestMouseOffIgnoresMouse(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 30})
	m.Update(tea.MouseClickMsg{X: 60, Y: 5, Button: tea.MouseLeft})
	if m.msgCur != 0 {
		t.Error("mouse events should be ignored when ui.mouse is off")
	}
	if m.View().MouseMode != tea.MouseModeNone {
		t.Error("mouse capture should be off")
	}
}

func TestFolderSyncKeys(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	if cmd := m.handleKey("+"); cmd != nil || !strings.Contains(m.errText, "already synced") {
		t.Errorf("+ on synced inbox: cmd=%v err=%q", cmd != nil, m.errText)
	}
	m.focus = paneFolders
	m.folderCur = len(m.folders) - 1 // Zebra, not synced
	if cmd := m.handleKey("+"); cmd == nil {
		t.Error("+ on an unsynced folder should ask the daemon to sync it")
	}
	m.handleKey("-")
	if !strings.Contains(m.errText, "isn't synced") {
		t.Errorf("- on unsynced folder: %q", m.errText)
	}
	m.folderCur = 0 // Inbox
	m.handleKey("-")
	if m.confirm == nil {
		t.Fatal("- on a synced folder should ask for confirmation")
	}
	if cmd := m.handleKey("n"); cmd != nil || m.confirm != nil {
		t.Error("n should cancel")
	}
}

func TestDeleteArchivesSetting(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	m.cfg.Keys.DeleteArchives = true
	if !strings.Contains(m.bottomBar(), "d archive") {
		t.Errorf("hint should say d archives: %s", ansi.Strip(m.bottomBar()))
	}
	m.handleKey("X")
	if m.confirm == nil || !strings.Contains(m.confirm.prompt, "Deleted Items") {
		t.Fatal("X should ask before deleting")
	}
	if cmd := m.handleKey("y"); cmd == nil {
		t.Error("y should confirm the delete")
	}
}

func typeText(m *Model, s string) {
	for _, r := range s {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func submit(m *Model) tea.Cmd {
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	return cmd
}

func TestSearch(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	inbox := len(m.msgs)

	press(m, "/")
	if m.prompt == nil {
		t.Fatal("/ didn't open the prompt")
	}
	typeText(m, "number 4")
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "/number 4") {
		t.Errorf("prompt not shown:\n%s", view)
	}
	deliver(m, submit(m))
	if m.prompt != nil || m.search == nil {
		t.Fatal("enter didn't start a search")
	}
	// "number 4" and "number 40".."number 49" (words match prefixes).
	if len(m.msgs) != 11 || m.msgs[0].GraphID != "m49" {
		t.Fatalf("results = %d, first %v", len(m.msgs), m.msgs[0].GraphID)
	}
	view = ansi.Strip(m.View().Content)
	if !strings.Contains(view, "search: number 4  11 found") || !strings.Contains(view, "Inbox") {
		t.Errorf("results view:\n%s", view)
	}

	// The cache changing re-runs the search and keeps the cursor.
	press(m, "j", "j")
	cur, _ := m.currentMessage()
	deliver(m, m.refreshSearch())
	if now, _ := m.currentMessage(); now.GraphID != cur.GraphID || len(m.msgs) != 11 {
		t.Errorf("refresh moved the cursor from %s to %s", cur.GraphID, now.GraphID)
	}

	// / reopens the prompt with the query to edit; esc there keeps the results.
	press(m, "/")
	if m.prompt == nil || m.prompt.Value() != "number 4" {
		t.Fatal("prompt not prefilled")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.prompt != nil || len(m.msgs) != 11 {
		t.Error("esc in the prompt should only close it")
	}

	// esc in the list goes back to the folder.
	deliver(m, m.handleKey("esc"))
	if m.search != nil || len(m.msgs) != inbox {
		t.Errorf("after clearing: search=%v msgs=%d", m.search, len(m.msgs))
	}

	// Bad syntax and unknown folders are reported, not searched.
	press(m, "/")
	typeText(m, "is:spam")
	deliver(m, submit(m))
	if m.search != nil || !strings.Contains(m.errText, "is:spam") {
		t.Errorf("bad query: search=%v err=%q", m.search, m.errText)
	}
	press(m, "/")
	typeText(m, "in:nowhere")
	deliver(m, submit(m))
	if !strings.Contains(m.errText, "nowhere") {
		t.Errorf("unknown folder: err=%q", m.errText)
	}
	press(m, "esc")

	// Nothing cached: falls back to the server when the query allows it.
	press(m, "/")
	typeText(m, "zanzibar")
	cmd := submit(m)
	if msg := cmd(); msg != nil {
		if next := m.handleSearchResult(msg.(searchResultMsg)); next == nil {
			t.Error("no server search started")
		}
	}
	if m.search.kql != "zanzibar" || !m.search.running {
		t.Errorf("fallback: %+v", m.search)
	}
	if view := ansi.Strip(m.View().Content); !strings.Contains(view, "Searching the server") {
		t.Errorf("fallback view:\n%s", view)
	}

	// ...but not when it uses local-only filters.
	press(m, "esc", "/")
	typeText(m, "zanzibar is:unread")
	deliver(m, submit(m))
	if m.search.kql != "" || m.search.running {
		t.Errorf("local-only query went to the server: %+v", m.search)
	}
	if view := ansi.Strip(m.View().Content); !strings.Contains(view, "No matches. Start the search with ?") {
		t.Errorf("no-match view:\n%s", view)
	}

	// Opening a folder from the folder pane leaves the search.
	press(m, "h", "enter")
	if m.search != nil || len(m.msgs) != inbox {
		t.Errorf("opening a folder: search=%v msgs=%d", m.search, len(m.msgs))
	}
}

func TestSearchViewFits(t *testing.T) {
	m := newTestModel(t)
	press(m, "/")
	typeText(m, "subject")
	for _, sz := range [][2]int{{20, 5}, {80, 24}, {200, 50}} {
		m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		for i, l := range strings.Split(m.View().Content, "\n") {
			if w := ansi.StringWidth(l); w != sz[0] {
				t.Errorf("%dx%d prompt open: line %d is %d wide", sz[0], sz[1], i, w)
			}
		}
	}
	deliver(m, submit(m))
	for _, sz := range [][2]int{{20, 5}, {80, 24}, {200, 50}} {
		m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		for i, l := range strings.Split(m.View().Content, "\n") {
			if w := ansi.StringWidth(l); w != sz[0] {
				t.Errorf("%dx%d results: line %d is %d wide", sz[0], sz[1], i, w)
			}
		}
	}
}

// prepareCompose starts composing and runs the preparation, as the runtime
// would, up to the point where the editor would open.
func prepareCompose(t *testing.T, m *Model, key string) *composing {
	t.Helper()
	cmd := m.handleKey(key)
	if cmd == nil {
		t.Fatalf("%s: no command (err %q)", key, m.errText)
	}
	ready, ok := cmd().(composeReadyMsg)
	if !ok || ready.err != nil {
		t.Fatalf("%s: prepare = %+v", key, ready)
	}
	if m.composeReady(ready) == nil || m.composing == nil {
		t.Fatalf("%s: editor not started (err %q)", key, m.errText)
	}
	return m.composing
}

func editFile(t *testing.T, c *composing, edit func(string) string) {
	t.Helper()
	data, err := os.ReadFile(c.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.path, []byte(edit(string(data))), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestComposeNewReviewAndDiscard(t *testing.T) {
	m := newTestModel(t)
	m.cfg.Paths.ComposeDir = t.TempDir()
	m.cfg.Compose.Signature = "Tom"
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	// Quitting the editor without writing anything discards quietly.
	c := prepareCompose(t, m, "c")
	if st, _ := os.Stat(c.path); st.Mode().Perm() != 0o600 {
		t.Errorf("compose file mode %v", st.Mode())
	}
	m.editorDone(nil)
	if m.composing != nil || !strings.Contains(m.notice, "discarded") {
		t.Errorf("unchanged file should discard: %+v %q", m.composing, m.notice)
	}
	if _, err := os.Stat(c.path); !os.IsNotExist(err) {
		t.Error("compose file left behind")
	}

	c = prepareCompose(t, m, "c")
	tmpl, _ := os.ReadFile(c.path)
	if !strings.Contains(string(tmpl), "\n-- \nTom\n") {
		t.Errorf("signature missing:\n%s", tmpl)
	}
	editFile(t, c, func(s string) string {
		s = strings.Replace(s, "To: \n", "To: Alice <alice@example.com>\n", 1)
		s = strings.Replace(s, "Subject: \n", "Subject: Lunch?\n", 1)
		s = strings.Replace(s, "Attach: \n", "Attach: ~/definitely/not/here.pdf\n", 1)
		return s + "\nThursday?\n"
	})
	m.editorDone(nil)
	view := ansi.Strip(m.View().Content)
	for _, want := range []string{"New message", "needs fixing", "To:       Alice <alice@example.com>", "Subject:  Lunch?", "✗ Attach:", "Thursday?", "y send"} {
		if !strings.Contains(view, want) {
			t.Errorf("review missing %q:\n%s", want, view)
		}
	}
	if m.handleKey("y") != nil || !strings.Contains(m.errText, "fix the problems") {
		t.Errorf("sent with problems: %q", m.errText)
	}

	// Fix the attachment: a real file.
	file := filepath.Join(t.TempDir(), "agenda.txt")
	os.WriteFile(file, []byte("x"), 0o600)
	editFile(t, c, func(s string) string { return strings.Replace(s, "~/definitely/not/here.pdf", file, 1) })
	m.editorDone(nil)
	if len(c.problems) != 0 || len(c.attach) != 1 || !strings.Contains(ansi.Strip(m.View().Content), "agenda.txt (1 bytes)") {
		t.Errorf("after fixing: problems=%v attach=%v", c.problems, c.attach)
	}
	if m.handleKey("y") == nil || c.busy != "Sending…" {
		t.Error("y should send")
	}
	// The daemon refused (e.g. safe mode): back on the review screen.
	m.Update(composeDoneMsg{send: true, err: errors.New("safe mode: messages can only be sent to yourself")})
	if m.composing == nil || c.busy != "" || !strings.Contains(m.errText, "safe mode") {
		t.Errorf("refused send: composing=%v err=%q", m.composing, m.errText)
	}

	// q asks first; y discards and removes the file.
	m.handleKey("q")
	if m.confirm == nil {
		t.Fatal("no discard confirmation")
	}
	deliver(m, m.handleKey("y"))
	if m.composing != nil {
		t.Error("not discarded")
	}
	if _, err := os.Stat(c.path); !os.IsNotExist(err) {
		t.Error("compose file left behind after discard")
	}
}

func TestComposeSendOutcomes(t *testing.T) {
	m := newTestModel(t)
	m.cfg.Paths.ComposeDir = t.TempDir()
	for _, tc := range []struct {
		msg    composeDoneMsg
		notice string
		err    string
	}{
		{composeDoneMsg{send: true}, "Sent.", ""},
		{composeDoneMsg{send: false}, "Saved to Drafts.", ""},
		{composeDoneMsg{send: true, res: ipc.ComposeResult{DraftID: "d1", SendError: "boom (the message is saved in Drafts)"}}, "", "Drafts and press e"},
	} {
		c := prepareCompose(t, m, "c")
		editFile(t, c, func(s string) string { return strings.Replace(s, "To: \n", "To: a@example.com\n", 1) + "hi\n" })
		m.editorDone(nil)
		m.Update(tc.msg)
		if m.composing != nil || m.notice != tc.notice || !strings.Contains(m.errText, tc.err) {
			t.Errorf("%+v: composing=%v notice=%q err=%q", tc.msg, m.composing, m.notice, m.errText)
		}
		if _, err := os.Stat(c.path); !os.IsNotExist(err) {
			t.Error("compose file left behind")
		}
		m.notice, m.errText = "", ""
	}
}

func TestReplyTemplateQuotesOriginal(t *testing.T) {
	m := newTestModel(t)
	m.cfg.Paths.ComposeDir = t.TempDir()
	// The newest message ("evil") has a cached body with hostile content.
	c := prepareCompose(t, m, "R")
	data, _ := os.ReadFile(c.path)
	text := string(data)
	if !strings.Contains(text, "Subject: Re: ") || !strings.Contains(text, "> line one") || strings.Contains(text, "\x1b") {
		t.Errorf("reply template:\n%q", text)
	}
	if c.kind != compose.ReplyAll || c.ref != "evil" {
		t.Errorf("composing = %+v", c)
	}
	// e only edits drafts.
	m.composing = nil
	if m.handleKey("e") != nil || !strings.Contains(m.errText, "isn't in Drafts") {
		t.Errorf("e on a non-draft: %q", m.errText)
	}
}

func TestStaleRefreshDoesNotSwitchFolderBack(t *testing.T) {
	m := newTestModel(t)
	refresh := loadCmd(m.st, m.account.ID, m.folderID, "") // started before the switch
	m.openWellKnown("sentitems")
	sent := m.folderID
	m.Update(refresh())
	if m.folderID != sent || len(m.msgs) != 0 {
		t.Errorf("stale inbox refresh switched folder %d -> %d (%d msgs)", sent, m.folderID, len(m.msgs))
	}
}

func TestMarkdownReviewShowsFormattedResult(t *testing.T) {
	m := newTestModel(t)
	m.cfg.Paths.ComposeDir = t.TempDir()
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c := prepareCompose(t, m, "r")
	tmpl, _ := os.ReadFile(c.path)
	if !strings.Contains(string(tmpl), "Format: markdown\n") || !strings.Contains(string(tmpl), "<!-- zorp: the original below") {
		t.Fatalf("template:\n%s", tmpl)
	}
	editFile(t, c, func(s string) string {
		s = strings.Replace(s, "To: \n", "To: a@example.com\n", 1)
		head, body, _ := strings.Cut(s, "\n\n")
		return head + "\n\nHi **there**\n\n- one\n- two\n" + body
	})
	m.editorDone(nil)
	view := ansi.Strip(m.View().Content)
	for _, want := range []string{"Format:   Markdown, sent formatted (HTML)", "Hi there", "• one", "The original message follows here"} {
		if !strings.Contains(view, want) {
			t.Errorf("review missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "**there**") || strings.Contains(view, "> line one") {
		t.Errorf("review shows the source, not the result:\n%s", view)
	}
}

func TestOpenFallsBackToClipboard(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "10.0.0.1 5000 10.0.0.2 22")
	t.Setenv("DISPLAY", ":0")
	cfg := &config.Config{UI: config.UI{Open: "auto"}}
	if canOpenLocally(cfg) {
		t.Error("SSH session should not open locally")
	}
	cfg.UI.Open = "browser"
	if !canOpenLocally(cfg) {
		t.Error("ui.open = browser should open locally")
	}
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_TTY", "")
	cfg.UI.Open = "auto"
	if !canOpenLocally(cfg) {
		t.Error("local desktop should open locally")
	}
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	if canOpenLocally(cfg) {
		t.Error("no display should not open locally")
	}

	m := newTestModel(t)
	m.cfg.UI.Open = "copy"
	gw := "https://protect.checkpoint.com/v2/r01/___https://example.com/register___.YzJ1"
	if m.openURL(gw) == nil || !strings.Contains(m.notice, "copied") || !strings.Contains(m.notice, "https://example.com/register") {
		t.Errorf("copy: notice %q", m.notice)
	}
	m.notice = ""
	if m.exported(exportedMsg{path: "/run/user/1000/zorp-view/x.html"}) == nil || !strings.Contains(m.notice, "/run/user/1000/zorp-view/x.html") {
		t.Errorf("exported: notice %q", m.notice)
	}
	m.notice = ""
	if m.openDone(openDoneMsg{target: "https://x.example", err: errors.New("xdg-open: not found")}) == nil || !strings.Contains(m.notice, "copied") {
		t.Errorf("open failure: notice %q", m.notice)
	}
}

func TestGraphicsDetectionAndInlineImages(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	// The terminal answers the startup query: Kitty graphics, 10×20 cells.
	m.gfx.known = false
	m.Update(uv.KittyGraphicsEvent{Options: kitty.Options{ID: termgfx.QueryID}, Payload: []byte("OK")})
	m.Update(uv.CellSizeEvent{Width: 10, Height: 20})
	if m.gfx.known {
		t.Error("known before the device attributes reply")
	}
	m.Update(uv.PrimaryDeviceAttributesEvent{62, 22})
	if !m.gfx.known || !m.gfx.support.Kitty || m.gfx.cell != (termgfx.CellSize{W: 10, H: 20}) {
		t.Fatalf("detection: %+v", m.gfx)
	}

	// A message with an inline image whose body is ready.
	cur, _ := m.currentMessage()
	m.doc = render.FromHTML(`<p>Chart:</p><p><img src="cid:chart@x" alt="chart"></p><p>End</p>`)
	m.bodyState, m.bodyID = bodyReady, cur.ID
	cmd := m.maybeLoadImages()
	if cmd == nil || !m.gfx.loading || m.gfx.msgID != cur.ID {
		t.Fatalf("no image load started: %+v", m.gfx)
	}
	if m.maybeLoadImages() != nil {
		t.Error("second load started while one is running")
	}

	inline := &termgfx.Image{ID: 5, Cols: 20, Rows: 3, Transmit: "\x1b_Ga=T,i=5;AAAA\x1b\\"}
	big := &termgfx.Image{ID: 6, Cols: 500, Rows: 2, Transmit: "x"}
	att := &termgfx.Image{ID: 7, Cols: 10, Rows: 2, Transmit: "\x1b_Ga=T,i=7;AAAA\x1b\\"}
	w, _ := m.previewWidth()
	tx := m.imagesReady(imagesMsg{seq: m.bodySeq, msgID: cur.ID, width: w,
		inline: map[string]*termgfx.Image{"chart@x": inline}, attached: []namedImage{{"photo.jpg", att}, {"huge.png", big}}})
	if tx == nil {
		t.Fatal("images not transmitted")
	}
	content := m.previewContent(w)
	if strings.Count(content, string(kitty.Placeholder)) != 20*3+10*2 {
		t.Errorf("placeholder cells = %d", strings.Count(content, string(kitty.Placeholder)))
	}
	plainView := ansi.Strip(content)
	if strings.Contains(plainView, "[image: chart]") || !strings.Contains(plainView, "▸ photo.jpg") || strings.Contains(plainView, "huge.png") {
		t.Errorf("preview:\n%s", plainView)
	}
	for i, l := range strings.Split(m.View().Content, "\n") {
		if lw := ansi.StringWidth(l); lw != 120 {
			t.Errorf("line %d is %d wide", i, lw)
		}
	}

	// Results for another message (the cursor moved) are dropped.
	m.gfx.msgID = -1
	if m.imagesReady(imagesMsg{msgID: cur.ID, width: w}) != nil {
		t.Error("stale images used")
	}
}

func TestGraphicsOffKeepsTextPlaceholders(t *testing.T) {
	m := newTestModel(t)
	m.cfg.UI.Graphics = "none"
	if m.startGraphics() != nil || !m.gfx.known || m.gfx.support.Kitty {
		t.Errorf("graphics = none: %+v", m.gfx)
	}
	m.doc = render.FromHTML(`<img src="cid:a@b" alt="logo">`)
	m.bodyState = bodyReady
	if m.maybeLoadImages() != nil {
		t.Error("images loaded with graphics off")
	}
}

// archiveCurrent does what the daemon does when a is pressed: moves the
// message (to Apples here) and reports the change.
func archiveCurrent(t *testing.T, m *Model) store.Message {
	t.Helper()
	cur, _ := m.currentMessage()
	press(m, "a") // the daemon call it returns isn't run
	ctx := context.Background()
	dest, err := m.st.FolderByName(ctx, m.account.ID, "Apples")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.st.Enqueue(ctx, m.account.ID, cur.GraphID, store.Mutation{Kind: store.OpMove, Dest: dest}); err != nil {
		t.Fatal(err)
	}
	return cur
}

func TestCursorMovesToNextAfterArchive(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	press(m, "j", "j", "j", "j", "j")
	next := m.msgs[m.msgCur+1]
	archiveCurrent(t, m)
	deliver(m, m.handleEvent(ipc.Event{Name: ipc.EventChanged}))
	if got, _ := m.currentMessage(); got.GraphID != next.GraphID || m.msgCur != 5 {
		t.Errorf("folder: cursor on %s (row %d), want %s (row 5)", got.GraphID, m.msgCur, next.GraphID)
	}

	// In search results the archived message still matches (now in
	// Apples); the cursor moves past it and the list doesn't scroll back.
	press(m, "/")
	typeText(m, "number")
	deliver(m, submit(m))
	for range 20 {
		press(m, "j")
	}
	row, top := m.msgCur, m.msgTop
	next = m.msgs[row+1]
	archived := archiveCurrent(t, m)
	deliver(m, m.refreshSearch())
	if got, _ := m.currentMessage(); got.GraphID != next.GraphID || m.msgCur != row+1 || m.msgTop < top {
		t.Errorf("search: cursor on %s (row %d, top %d), want %s (row %d, top >= %d)", got.GraphID, m.msgCur, m.msgTop, next.GraphID, row+1, top)
	}
	if m.msgs[row].GraphID != archived.GraphID {
		t.Error("archived message should still be listed in the results")
	}
	// Another refresh leaves the cursor where it is.
	deliver(m, m.refreshSearch())
	if got, _ := m.currentMessage(); got.GraphID != next.GraphID {
		t.Errorf("second refresh moved the cursor to %s", got.GraphID)
	}
}

func TestSelection(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	first, second := m.msgs[0], m.msgs[1]
	press(m, "space", "space")
	if len(m.selected) != 2 || !m.selected[first.GraphID] || !m.selected[second.GraphID] || m.msgCur != 2 {
		t.Fatalf("selected %v, cursor %d", m.selected, m.msgCur)
	}
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "2 selected") || !strings.Contains(view, "✓N") && !strings.Contains(view, "✓ ") {
		t.Errorf("selection not shown:\n%s", view)
	}
	press(m, "k", "space") // unselect the second
	if len(m.selected) != 1 {
		t.Errorf("toggle off: %v", m.selected)
	}

	// Archiving the selection asks first, with the count, and then clears it.
	press(m, "a")
	if m.confirm == nil || !strings.Contains(m.confirm.prompt, "Archive 1 message?") {
		t.Fatalf("confirm = %+v", m.confirm)
	}
	if cmd := m.handleKey("y"); cmd == nil || len(m.selected) != 0 {
		t.Errorf("y: cmd %v, selected %v", cmd, m.selected)
	}

	// * selects everything listed; again clears; esc clears too.
	press(m, "*")
	if len(m.selected) != len(m.msgs) {
		t.Errorf("* selected %d of %d", len(m.selected), len(m.msgs))
	}
	press(m, "*")
	if len(m.selected) != 0 {
		t.Error("second * should clear")
	}
	press(m, "*", "esc")
	if len(m.selected) != 0 {
		t.Error("esc should clear")
	}

	// Move with a selection moves them all; reply still uses the cursor.
	press(m, "space", "space", "space", "m")
	if m.picker == nil || len(m.picker.moving) != 3 || !strings.Contains(m.picker.title, "3 messages") {
		t.Fatalf("move picker = %+v", m.picker)
	}
	press(m, "esc")

	// Selections don't survive opening another folder.
	press(m, "g", "s")
	if len(m.selected) != 0 {
		t.Error("selection kept across folders")
	}
}

func TestTerminalColorsTint(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	t.Cleanup(func() { applyTheme(theme.New(theme.ModeAuto, false, nil, nil)) })
	plain := styleCursor.Render("x")
	_, cmd := m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	if cmd == nil {
		t.Fatal("24-bit terminal: no colour queries sent")
	}
	m.Update(tea.BackgroundColorMsg{Color: color.RGBA{30, 30, 46, 255}})
	m.Update(tea.ForegroundColorMsg{Color: color.RGBA{205, 214, 244, 255}})
	m.Update(uv.UnknownOscEvent("\x1b]4;4;rgb:89/b4/fa\x07"))
	if m.colors.pal.ANSI[4] == nil || !strings.Contains(styleCursor.Render("x"), "48;2;") || styleCursor.Render("x") == plain {
		t.Errorf("no tint after the terminal reported its colours: %q", styleCursor.Render("x"))
	}

	// ui.color = "ansi" keeps the plain look.
	m.cfg.UI.Color = theme.ModeANSI
	m.rebuildTheme()
	if strings.Contains(styleCursor.Render("x"), "48;2;") {
		t.Error("ansi mode tinted")
	}
}

func TestNotifyEvents(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	t.Setenv("TERM", "xterm-ghostty")
	t.Setenv("TMUX", "")
	data, _ := json.Marshal(ipc.Notify{Title: "Alice", Body: "Lunch?"})
	cmd := m.handleEvent(ipc.Event{Name: ipc.EventNotify, Data: data})
	if cmd == nil {
		t.Fatal("no terminal notification")
	}
	if raw := fmt.Sprint(cmd()); !strings.Contains(raw, "]777;notify;Alice;Lunch?") {
		t.Errorf("sequence = %q", raw)
	}

	// A clicked notification puts the cursor on the message.
	target := m.msgs[7].GraphID
	data, _ = json.Marshal(ipc.MessageRef{GraphID: target})
	deliver(m, m.handleEvent(ipc.Event{Name: ipc.EventOpen, Data: data}))
	if cur, _ := m.currentMessage(); cur.GraphID != target {
		t.Errorf("cursor on %s, want %s", cur.GraphID, target)
	}
}

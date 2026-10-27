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

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/store"
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

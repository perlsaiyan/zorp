package tui

import (
	"context"
	"os"
	"os/exec"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/render"
)

// Opening links and messages. On a desktop they go to the default browser
// (xdg-open). Over SSH, or with no display, there's nothing local to open
// them with, so the link (or the exported file's path) is copied to the
// clipboard of the terminal you're sitting at, via OSC 52.

type (
	// exportedMsg is a message written out as HTML for viewing.
	exportedMsg struct {
		path string
		err  error
	}
	// openDoneMsg reports xdg-open's result for target.
	openDoneMsg struct {
		target string
		err    error
	}
)

// canOpenLocally reports whether xdg-open has a desktop to open things on.
func canOpenLocally(cfg *config.Config) bool {
	switch cfg.UI.Open {
	case "browser":
		return true
	case "copy":
		return false
	}
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return false
	}
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

// openURL opens a link from a message, or copies it when it can't be
// opened here. The link is the real target (a security gateway's rewrite,
// if any), so the gateway still checks it; the notice shows where it goes.
func (m *Model) openURL(url string) tea.Cmd {
	if canOpenLocally(m.cfg) {
		return func() tea.Msg { return openDoneMsg{url, xdgOpen(url)} }
	}
	m.notice = "Link copied to your clipboard: " + clean(render.DisplayURL(url))
	return tea.SetClipboard(url)
}

// openMessage exports the message as standalone HTML (inline images
// embedded, remote content blocked), then opens it or copies its path.
func (m *Model) openMessage(graphID string) tea.Cmd {
	cfg := m.cfg
	m.notice = "Preparing the message for the browser…"
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		var res ipc.ExportHTMLResult
		err := callDaemon(ctx, cfg, ipc.MethodExportHTML, ipc.ExportHTMLParams{GraphID: graphID}, &res)
		return exportedMsg{res.Path, err}
	}
}

func (m *Model) exported(r exportedMsg) tea.Cmd {
	m.notice = ""
	if r.err != nil {
		m.errText = "open: " + r.err.Error()
		return nil
	}
	if canOpenLocally(m.cfg) {
		return func() tea.Msg { return openDoneMsg{r.path, xdgOpen(r.path)} }
	}
	m.notice = "No browser here; saved as " + clean(r.path) + " (path copied)"
	return tea.SetClipboard(r.path)
}

// openDone falls back to copying when xdg-open fails.
func (m *Model) openDone(r openDoneMsg) tea.Cmd {
	if r.err == nil {
		return nil
	}
	m.notice = "Couldn't open it (" + r.err.Error() + "); copied to your clipboard instead"
	return tea.SetClipboard(r.target)
}

// xdgOpen hands a path or URL to the desktop's default handler. Its output
// is discarded so it can't scribble over the UI.
func xdgOpen(target string) error {
	cmd := exec.Command("xdg-open", target)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() // reap it
	return nil
}

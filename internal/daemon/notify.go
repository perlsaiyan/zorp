package daemon

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"

	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/mailsync"
	"github.com/perlsaiyan/zorp/internal/notify"
)

// After a failed desktop notification (no notification server, e.g. an
// SSH session), notifications go through the terminal for a while.
const desktopRetry = time.Hour

// notifyNew sends notifications for mail that arrived in this sync pass.
func (d *daemon) notifyNew(ctx context.Context, res *mailsync.Result) {
	var arrivals []notify.Arrival
	for _, fr := range res.FolderResults {
		if !fr.Initial && len(fr.New) > 0 {
			arrivals = append(arrivals, notify.Arrival{Folder: fr.Folder, WellKnown: fr.WellKnown, Messages: fr.New})
		}
	}
	if len(arrivals) == 0 {
		return
	}
	acct, err := d.store.DefaultAccount(ctx)
	if err != nil {
		return
	}
	notes := notify.Build(d.cfg.Notify, notify.Select(d.cfg.Notify, arrivals, acct.UPN, time.Now()))
	for _, n := range notes {
		go d.deliver(n) // a desktop notification takes a moment to confirm
	}
	if len(notes) > 0 {
		d.log.Info("new mail notification", "notifications", len(notes))
	}
}

// notifyTest sends a test notification the way new mail would be.
func (d *daemon) notifyTest() ipc.NotifyTestResult {
	if !d.cfg.Notify.Enabled || d.cfg.Notify.Method == "none" {
		return ipc.NotifyTestResult{Via: "none"}
	}
	return ipc.NotifyTestResult{Via: d.deliver(notify.Note{Title: "zorp", Body: "Test notification: new mail would look like this."})}
}

// deliver shows a notification on the desktop, or asks clients to show it
// in their terminals. It returns which it used.
func (d *daemon) deliver(n notify.Note) string {
	method := d.cfg.Notify.Method
	d.mu.Lock()
	desktopOK := time.Now().After(d.desktopFailedUntil)
	d.mu.Unlock()
	if (method == "desktop" || (method == "auto" && desktopOK)) && notify.DesktopAvailable() {
		// The notification stays clickable for a while; this context
		// outlives the sync pass.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		err := notify.Desktop(ctx, n, func() {
			defer cancel()
			d.notificationClicked(n)
		})
		if err == nil {
			return "desktop"
		}
		cancel()
		d.log.Warn("desktop notification failed; using the terminal", "err", err)
		if errors.Is(err, notify.ErrNoServer) {
			d.mu.Lock()
			d.desktopFailedUntil = time.Now().Add(desktopRetry)
			d.mu.Unlock()
		}
		if method == "desktop" {
			return "none"
		}
	}
	d.server.Broadcast(ipc.EventNotify, ipc.Notify{Title: n.Title, Body: n.Body, GraphID: n.GraphID})
	return "terminal"
}

// notificationClicked opens a reminder's join link. For mail, it tells a
// running zorp to show the message, and runs notify.click_command (e.g. to
// focus or open zorp's window).
func (d *daemon) notificationClicked(n notify.Note) {
	if n.URL != "" {
		d.openLink(n.URL)
		return
	}
	if n.GraphID != "" {
		d.server.Broadcast(ipc.EventOpen, ipc.MessageRef{GraphID: n.GraphID})
	}
	args := strings.Fields(d.cfg.Notify.ClickCommand)
	if len(args) == 0 {
		return
	}
	cmd := exec.Command(args[0], args[1:]...)
	if err := cmd.Start(); err != nil {
		d.log.Warn("notify.click_command", "err", err)
		return
	}
	go cmd.Wait()
}

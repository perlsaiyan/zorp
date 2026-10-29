package daemon

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/perlsaiyan/zorp/internal/agenda"
	"github.com/perlsaiyan/zorp/internal/notify"
	"github.com/perlsaiyan/zorp/internal/store"
)

// remindEvery is how often the cached calendar is checked for meetings
// about to start.
const remindEvery = 30 * time.Second

// remindLoop shows a reminder calendar.remind before each meeting.
func (d *daemon) remindLoop(ctx context.Context) {
	reminded := map[string]time.Time{} // event at its start time -> when it ends
	t := time.NewTicker(remindEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		for _, n := range d.dueReminders(ctx, now, reminded) {
			go d.deliver(n)
		}
		for k, end := range reminded {
			if end.Before(now) {
				delete(reminded, k)
			}
		}
	}
}

// dueReminders are the reminders to show now: meetings starting within
// calendar.remind that haven't had one. reminded records those shown.
func (d *daemon) dueReminders(ctx context.Context, now time.Time, reminded map[string]time.Time) []notify.Note {
	c, n := d.cfg.Calendar, d.cfg.Notify
	if !c.Enabled || c.Remind <= 0 || !n.Enabled || n.Method == "none" || n.Quiet(now) {
		return nil
	}
	accountID, err := d.engine.Account(ctx)
	if err != nil {
		return nil
	}
	evs, err := d.store.EventsBetween(ctx, accountID, now, now.Add(c.Remind))
	if err != nil {
		return nil
	}
	var out []notify.Note
	for _, e := range evs {
		key := e.GraphID + "@" + strconv.FormatInt(e.Start.Unix(), 10)
		if e.Details.AllDay || !e.Details.Busy() || !e.Start.After(now) || !reminded[key].IsZero() {
			continue
		}
		reminded[key] = e.End
		out = append(out, reminder(e, now, n.ShowContent))
	}
	return out
}

func reminder(e store.Event, now time.Time, content bool) notify.Note {
	mins := int(e.Start.Sub(now).Round(time.Minute) / time.Minute)
	in := "now"
	if mins > 0 {
		in = fmt.Sprintf("in %d min", mins)
	}
	if !content {
		return notify.Note{Title: "zorp", Body: "A meeting starts " + in}
	}
	body := agenda.When(e)
	link := e.Details.JoinURL
	if link == "" && strings.HasPrefix(e.Details.Location, "https://") {
		link = e.Details.Location // Zoom and others put the link here
	}
	if link != "" {
		body += " · online (click to join)"
	} else if loc := agenda.Clean(e.Details.Location); loc != "" {
		body += " · " + loc
	}
	return notify.Note{Title: agenda.Subject(e) + " " + in, Body: body, URL: link}
}

// openLink opens a web link from a clicked notification.
func (d *daemon) openLink(link string) {
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		d.log.Warn("not opening a notification link that isn't http(s)")
		return
	}
	cmd := exec.Command("xdg-open", u.String())
	if err := cmd.Start(); err != nil {
		d.log.Warn("opening a meeting link", "err", err)
		return
	}
	go cmd.Wait()
}

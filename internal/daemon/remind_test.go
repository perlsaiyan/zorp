package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/store"
)

func TestReminders(t *testing.T) {
	cfg := &config.Config{
		Calendar: config.Calendar{Enabled: true, Remind: 10 * time.Minute},
		Notify:   config.Notify{Enabled: true, Method: "auto", ShowContent: true},
	}
	d := newMeetingDaemon(t, cfg, &calendarGraph{})
	ctx := context.Background()
	acct, _ := d.engine.Account(ctx)
	now := time.Date(2026, 11, 2, 13, 52, 0, 0, time.Local)
	at := now.Add(8 * time.Minute)
	busy := store.EventDetails{Subject: "[EXTERNAL]Standup", Response: "accepted", ShowAs: "busy", JoinURL: "https://teams.example/j"}
	d.store.ApplyEvents(ctx, acct, true, []store.Event{
		{GraphID: "soon", Type: "singleInstance", Start: at, End: at.Add(30 * time.Minute), Details: busy},
		{GraphID: "free", Type: "singleInstance", Start: at, End: at.Add(time.Hour), Details: store.EventDetails{Subject: "Gym", ShowAs: "free"}},
		{GraphID: "no", Type: "singleInstance", Start: at, End: at.Add(time.Hour), Details: store.EventDetails{Subject: "Skip", Response: "declined"}},
		{GraphID: "far", Type: "singleInstance", Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Details: busy},
	}, nil, "", [2]time.Time{now.AddDate(0, 0, -1), now.AddDate(0, 0, 1)}, now)

	reminded := map[string]time.Time{}
	notes := d.dueReminders(ctx, now, reminded)
	if len(notes) != 1 || notes[0].Title != "Standup in 8 min" || notes[0].URL != "https://teams.example/j" ||
		!strings.Contains(notes[0].Body, "click to join") {
		t.Fatalf("notes = %+v", notes)
	}
	if again := d.dueReminders(ctx, now.Add(time.Minute), reminded); len(again) != 0 {
		t.Errorf("reminded twice: %+v", again)
	}

	cfg.Notify.ShowContent = false
	if n := d.dueReminders(ctx, now, map[string]time.Time{}); len(n) != 1 || strings.Contains(n[0].Title+n[0].Body, "Standup") {
		t.Errorf("without content: %+v", n)
	}
	cfg.Notify.QuietHours = "13:00-14:00"
	if n := d.dueReminders(ctx, now, map[string]time.Time{}); len(n) != 0 {
		t.Errorf("quiet hours: %+v", n)
	}
	cfg.Notify.QuietHours, cfg.Calendar.Remind = "", 0
	if n := d.dueReminders(ctx, now, map[string]time.Time{}); len(n) != 0 {
		t.Errorf("remind = 0: %+v", n)
	}
}

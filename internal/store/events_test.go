package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestEventsCacheJoinsSeriesDetails(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "zorp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	acct, _ := st.UpsertAccount(ctx, "me@example.com", "Me")
	day := time.Date(2026, 11, 2, 15, 0, 0, 0, time.UTC)
	window := [2]time.Time{day.AddDate(0, 0, -30), day.AddDate(0, 0, 180)}
	master := Event{GraphID: "s", Type: "seriesMaster", Start: day.AddDate(0, -1, 0), End: day.AddDate(0, -1, 0).Add(30 * time.Minute),
		Details: EventDetails{Subject: "Standup", Response: "accepted", ShowAs: "busy"}}
	occ := func(id string, d int) Event {
		return Event{GraphID: id, Type: "occurrence", SeriesMasterID: "s", Start: day.AddDate(0, 0, d), End: day.AddDate(0, 0, d).Add(30 * time.Minute)}
	}
	single := Event{GraphID: "x", Type: "singleInstance", Start: day.Add(time.Hour), End: day.Add(2 * time.Hour), Details: EventDetails{Subject: "Review"}}
	if err := st.ApplyEvents(ctx, acct, true, []Event{master, occ("o1", 0), occ("o2", 1), single}, nil, "link1", window, day); err != nil {
		t.Fatal(err)
	}
	evs, err := st.EventsBetween(ctx, acct, day, day.AddDate(0, 0, 2))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range evs {
		got = append(got, e.GraphID+":"+e.Details.Subject)
	}
	if want := "[o1:Standup x:Review o2:Standup]"; fmtSlice(got) != want {
		t.Errorf("events = %v, want %s (masters left out, occurrences with the master's details)", got, want)
	}

	// Renaming the series renames every occurrence.
	master.Details.Subject = "Daily standup"
	st.ApplyEvents(ctx, acct, false, []Event{master}, nil, "link2", window, day)
	if e, _ := st.EventByGraphID(ctx, acct, "o2"); e.Details.Subject != "Daily standup" {
		t.Errorf("occurrence subject = %q", e.Details.Subject)
	}
	if c, _ := st.CalendarSync(ctx, acct); c.DeltaLink != "link2" || !c.WindowStart.Equal(window[0]) {
		t.Errorf("sync state = %+v", c)
	}

	// Removing the series removes its occurrences.
	st.ApplyEvents(ctx, acct, false, nil, []string{"s"}, "", window, day)
	if evs, _ := st.EventsBetween(ctx, acct, day, day.AddDate(0, 0, 2)); len(evs) != 1 || evs[0].GraphID != "x" {
		t.Errorf("after removing the series: %+v", evs)
	}

	// A fresh pass empties the calendar and forgets the link until it ends.
	st.ApplyEvents(ctx, acct, true, nil, nil, "", window, day)
	if c, _ := st.CalendarSync(ctx, acct); c.DeltaLink != "" {
		t.Errorf("link kept during a fresh pass: %q", c.DeltaLink)
	}
	if evs, _ := st.EventsBetween(ctx, acct, day, day.AddDate(0, 0, 2)); len(evs) != 0 {
		t.Errorf("fresh pass kept %d events", len(evs))
	}
}

func fmtSlice(s []string) string {
	out := "["
	for i, x := range s {
		if i > 0 {
			out += " "
		}
		out += x
	}
	return out + "]"
}

package mailsync

import (
	"context"
	"time"

	"github.com/perlsaiyan/zorp/internal/graph"
	"github.com/perlsaiyan/zorp/internal/store"
)

// The cached calendar window: this far back and ahead of today.
const (
	CalendarBack  = 30 * 24 * time.Hour
	CalendarAhead = 180 * 24 * time.Hour
)

// CalendarWindow is the window to cache at now: from local midnight
// CalendarBack ago to CalendarAhead after today. It moves once a day.
func CalendarWindow(now time.Time) [2]time.Time {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return [2]time.Time{day.Add(-CalendarBack), day.Add(CalendarAhead + 24*time.Hour)}
}

// CalendarResult says what a calendar pass did.
type CalendarResult struct {
	Initial  bool
	Upserted int
	Removed  int
}

// SyncCalendar brings the cached calendar up to date with one delta pass.
// A delta link only covers the window it began with, so a new day starts
// a fresh pass. cal must carry calendar access (auth.Calendar).
func (e *Engine) SyncCalendar(ctx context.Context, cal *graph.Client) (CalendarResult, error) {
	accountID, err := e.ensureAccount(ctx)
	if err != nil {
		return CalendarResult{}, err
	}
	state, err := e.store.CalendarSync(ctx, accountID)
	if err != nil {
		return CalendarResult{}, err
	}
	window := CalendarWindow(e.now())
	if state.DeltaLink != "" && state.WindowStart.Equal(window[0]) && state.WindowEnd.Equal(window[1]) {
		res, err := e.calendarPass(ctx, cal, accountID, state.DeltaLink, false, window)
		if !graph.IsSyncStateExpired(err) {
			return res, err
		}
		e.log.Info("calendar delta link expired; starting over")
	}
	return e.calendarPass(ctx, cal, accountID, cal.CalendarDeltaURL(window[0], window[1]), true, window)
}

func (e *Engine) calendarPass(ctx context.Context, cal *graph.Client, accountID int64, link string, initial bool, window [2]time.Time) (CalendarResult, error) {
	res := CalendarResult{Initial: initial}
	replace := initial
	for link != "" {
		page, err := cal.EventsDelta(ctx, link)
		if err != nil {
			return res, err
		}
		var evs []store.Event
		for _, ev := range page.Events {
			evs = append(evs, toStoreEvent(ev))
		}
		if err := e.store.ApplyEvents(ctx, accountID, replace, evs, page.Removed, page.DeltaLink, window, e.now()); err != nil {
			return res, err
		}
		replace = false
		res.Upserted += len(evs)
		res.Removed += len(page.Removed)
		link = page.NextLink
	}
	return res, nil
}

func toStoreEvent(ev graph.Event) store.Event {
	out := store.Event{
		GraphID:        ev.ID,
		Type:           ev.Type,
		SeriesMasterID: ev.SeriesMasterID,
		Start:          ev.Start.Time(),
		End:            ev.End.Time(),
	}
	if ev.Type == "occurrence" {
		return out // the rest is the master's
	}
	d := store.EventDetails{
		Subject:           ev.Subject,
		AllDay:            ev.IsAllDay,
		Location:          ev.Location.DisplayName,
		IsOrganizer:       ev.IsOrganizer,
		ResponseRequested: ev.ResponseRequested,
		Response:          ev.ResponseStatus.Response,
		ShowAs:            ev.ShowAs,
		Cancelled:         ev.IsCancelled,
		JoinURL:           ev.JoinURL(),
		WebLink:           ev.WebLink,
		Preview:           ev.BodyPreview,
	}
	if ev.Organizer != nil {
		d.Organizer = store.Address{Name: ev.Organizer.EmailAddress.Name, Addr: ev.Organizer.EmailAddress.Address}
	}
	for _, a := range ev.Attendees {
		d.Attendees = append(d.Attendees, store.Attendee{
			Address:  store.Address{Name: a.EmailAddress.Name, Addr: a.EmailAddress.Address},
			Type:     a.Type,
			Response: a.Status.Response,
		})
	}
	out.Details = d
	return out
}

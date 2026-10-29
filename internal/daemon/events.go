package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/perlsaiyan/zorp/internal/compose"
	"github.com/perlsaiyan/zorp/internal/graph"
	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/markdown"
)

// Meetings the user organizes: creating, changing and cancelling them.
// Each of these can send mail (invites, updates, cancellations) to
// everyone invited, so the send guardrail covers them all.

var errCalendarOff = errors.New("calendar access is off (calendar.enabled = false)")

func (d *daemon) eventGet(ctx context.Context, params json.RawMessage) (ipc.EventDetail, error) {
	var p ipc.EventRef
	if err := json.Unmarshal(params, &p); err != nil || p.ID == "" {
		return ipc.EventDetail{}, errors.New("event-get needs an id")
	}
	if !d.cfg.Calendar.Enabled {
		return ipc.EventDetail{}, errCalendarOff
	}
	ev, err := d.cal.EventForEdit(ctx, p.ID)
	if err != nil {
		return ipc.EventDetail{}, err
	}
	out := ipc.EventDetail{
		ID: ev.ID, Type: ev.Type, SeriesMasterID: ev.SeriesMasterID, Subject: ev.Subject,
		Start: ev.Start.Time(), End: ev.End.Time(), AllDay: ev.IsAllDay, Location: ev.Location.DisplayName,
		Online: ev.IsOnlineMeeting || ev.JoinURL() != "", JoinURL: ev.JoinURL(),
		IsOrganizer: ev.IsOrganizer, Cancelled: ev.IsCancelled,
	}
	for _, a := range ev.Attendees {
		addr := ipc.Address{Name: a.EmailAddress.Name, Addr: a.EmailAddress.Address}
		switch a.Type {
		case "optional":
			out.Optional = append(out.Optional, addr)
		case "resource": // rooms: shown in Where
		default:
			out.Required = append(out.Required, addr)
		}
	}
	if ev.Body != nil {
		out.BodyType, out.Body = ev.Body.ContentType, ev.Body.Content
	}
	return out, nil
}

func (d *daemon) eventSave(ctx context.Context, params json.RawMessage) (ipc.EventSaveResult, error) {
	var p ipc.EventSaveParams
	if err := json.Unmarshal(params, &p); err != nil {
		return ipc.EventSaveResult{}, err
	}
	if !d.cfg.Calendar.Enabled {
		return ipc.EventSaveResult{}, errCalendarOff
	}
	if (p.Start == nil) != (p.End == nil) {
		return ipc.EventSaveResult{}, errors.New("give both the start and the end")
	}
	if p.Start != nil && !p.End.After(*p.Start) {
		return ipc.EventSaveResult{}, errors.New("the meeting ends before it starts")
	}
	ch := graph.EventChange{
		Subject: p.Subject, Start: p.Start, End: p.End, AllDay: p.AllDay,
		Zone: localZone(), Location: p.Location, Online: p.Online,
	}
	if p.Body != nil {
		body := *p.Body
		switch p.Format {
		case "", compose.FormatMarkdown:
			body, ch.BodyHTML = markdown.ToHTML(body), true
		case compose.FormatText:
		default:
			return ipc.EventSaveResult{}, fmt.Errorf("unknown format %q", p.Format)
		}
		ch.Body = &body
	}
	if p.Attendees != nil {
		var as []graph.Attendee
		for _, list := range []struct {
			kind  string
			addrs []ipc.Address
		}{{"required", p.Attendees.Required}, {"optional", p.Attendees.Optional}} {
			for _, a := range list.addrs {
				as = append(as, graph.Attendee{EmailAddress: graph.EmailAddress{Name: a.Name, Address: a.Addr}, Type: list.kind})
			}
		}
		ch.Attendees = &as
	}

	if p.ID == "" {
		if p.Subject == nil || strings.TrimSpace(*p.Subject) == "" || p.Start == nil {
			return ipc.EventSaveResult{}, errors.New("a new meeting needs a subject and a time")
		}
		var invited []string
		if ch.Attendees != nil {
			for _, a := range *ch.Attendees {
				invited = append(invited, a.EmailAddress.Address)
			}
		}
		notified, err := d.checkInvitees(ctx, invited)
		if err != nil {
			return ipc.EventSaveResult{}, err
		}
		ev, err := d.cal.CreateEvent(ctx, ch)
		if err != nil {
			return ipc.EventSaveResult{}, fmt.Errorf("creating the meeting: %w", err)
		}
		d.log.Info("created meeting", "invited", notified, "online", p.Online != nil && *p.Online)
		d.calendarChanged()
		return ipc.EventSaveResult{ID: ev.ID, JoinURL: ev.JoinURL(), Notified: notified}, nil
	}

	ev, err := d.cal.EventForEdit(ctx, p.ID)
	if err != nil {
		return ipc.EventSaveResult{}, err
	}
	switch {
	case !ev.IsOrganizer:
		return ipc.EventSaveResult{}, errors.New("only the organizer can change this meeting")
	case ev.IsCancelled:
		return ipc.EventSaveResult{}, errors.New("the meeting was cancelled")
	case ev.Type == "seriesMaster" && (p.Start != nil || p.AllDay != nil):
		return ipc.EventSaveResult{}, errors.New("a recurring series' times can't be changed here")
	case p.Online != nil && !*p.Online && (ev.IsOnlineMeeting || ev.JoinURL() != ""):
		return ipc.EventSaveResult{}, errors.New("the Teams meeting can't be taken off again")
	}
	// The update goes to everyone invited afterwards, and a cancellation
	// to anyone taken off.
	seen := map[string]bool{}
	var told []string
	add := func(addr string) {
		if k := strings.ToLower(addr); !seen[k] {
			seen[k] = true
			told = append(told, addr)
		}
	}
	for _, a := range ev.Attendees {
		add(a.EmailAddress.Address)
	}
	if ch.Attendees != nil {
		for _, a := range *ch.Attendees {
			add(a.EmailAddress.Address)
		}
	}
	notified, err := d.checkInvitees(ctx, told)
	if err != nil {
		return ipc.EventSaveResult{}, err
	}
	if err := d.cal.UpdateEvent(ctx, ev.ID, ch); err != nil {
		return ipc.EventSaveResult{}, fmt.Errorf("changing the meeting: %w", err)
	}
	d.log.Info("changed meeting", "notified", notified)
	d.calendarChanged()
	return ipc.EventSaveResult{ID: ev.ID, JoinURL: ev.JoinURL(), Notified: notified}, nil
}

func (d *daemon) eventCancel(ctx context.Context, params json.RawMessage) (ipc.EventCancelResult, error) {
	var p ipc.EventCancelParams
	if err := json.Unmarshal(params, &p); err != nil || p.ID == "" {
		return ipc.EventCancelResult{}, errors.New("event-cancel needs an id")
	}
	if !d.cfg.Calendar.Enabled {
		return ipc.EventCancelResult{}, errCalendarOff
	}
	ev, err := d.cal.EventForEdit(ctx, p.ID)
	if err != nil {
		return ipc.EventCancelResult{}, err
	}
	if !ev.IsOrganizer {
		return ipc.EventCancelResult{}, errors.New("only the organizer can cancel this meeting; decline it instead (I)")
	}
	var invited []string
	for _, a := range ev.Attendees {
		invited = append(invited, a.EmailAddress.Address)
	}
	notified, err := d.checkInvitees(ctx, invited)
	if err != nil {
		return ipc.EventCancelResult{}, err
	}
	if notified == 0 {
		// Nobody to tell: it's just an appointment.
		err = d.cal.DeleteEvent(ctx, ev.ID)
	} else {
		err = d.cal.CancelEvent(ctx, ev.ID, strings.TrimSpace(p.Comment))
	}
	if err != nil {
		return ipc.EventCancelResult{}, fmt.Errorf("cancelling the meeting: %w", err)
	}
	d.log.Info("cancelled meeting", "notified", notified)
	d.calendarChanged()
	return ipc.EventCancelResult{Notified: notified}, nil
}

// freeBusy looks up when people are busy: read-only, nothing is sent.
func (d *daemon) freeBusy(ctx context.Context, params json.RawMessage) ([]ipc.FreeBusy, error) {
	var p ipc.FreeBusyParams
	if err := json.Unmarshal(params, &p); err != nil || len(p.Addrs) == 0 || !p.End.After(p.Start) {
		return nil, errors.New("free-busy needs addresses and a span")
	}
	if !d.cfg.Calendar.Enabled {
		return nil, errCalendarOff
	}
	if len(p.Addrs) > 100 {
		return nil, errors.New("free-busy: too many people")
	}
	scheds, err := d.cal.GetSchedule(ctx, p.Addrs, p.Start, p.End)
	if err != nil {
		return nil, err
	}
	out := make([]ipc.FreeBusy, 0, len(scheds))
	for _, s := range scheds {
		fb := ipc.FreeBusy{Addr: s.ScheduleID}
		if s.Error != nil {
			fb.Error = s.Error.Message
			if fb.Error == "" {
				fb.Error = s.Error.ResponseCode
			}
		}
		for _, it := range s.Items {
			if it.Status == "free" {
				continue
			}
			fb.Busy = append(fb.Busy, ipc.Busy{Start: it.Start.Time(), End: it.End.Time(), Status: it.Status})
		}
		out = append(out, fb)
	}
	return out, nil
}

// checkInvitees applies the send guardrail to mail about a meeting going
// to addrs, and says how many people (other than the user) get it.
func (d *daemon) checkInvitees(ctx context.Context, addrs []string) (int, error) {
	acct, err := d.store.DefaultAccount(ctx)
	if err != nil {
		return 0, fmt.Errorf("guardrail: can't check who's invited before the first sync: %w", err)
	}
	n := 0
	var blocked []string
	for _, a := range addrs {
		if strings.EqualFold(a, acct.UPN) {
			continue
		}
		n++
		if !d.cfg.Safety.SendAllowed() && !d.cfg.Safety.MaySendTo(a, acct.UPN) {
			blocked = append(blocked, a)
		}
	}
	if len(blocked) > 0 {
		return n, fmt.Errorf("guardrail: sending is limited to yourself and safety.send_allow, so the meeting can't be sent to %s (set safety.allow_send = true)", strings.Join(blocked, ", "))
	}
	return n, nil
}

// calendarChanged has the next sync pick up a change to the calendar.
func (d *daemon) calendarChanged() {
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

// localZone is the IANA name of the local time zone, for meetings' times,
// or "" for UTC, or if it can't be told.
func localZone() string {
	zone := ianaZone()
	if strings.HasPrefix(zone, "Etc/") {
		return "" // Etc/UTC and the like: Graph wants plain UTC
	}
	return zone
}

func ianaZone() string {
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" {
		if _, err := time.LoadLocation(tz); err == nil && strings.Contains(tz, "/") {
			return tz
		}
	}
	if name := time.Local.String(); name != "Local" && strings.Contains(name, "/") {
		return name
	}
	if target, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
		if _, zone, ok := strings.Cut(target, "zoneinfo/"); ok {
			if _, err := time.LoadLocation(zone); err == nil {
				return zone
			}
		}
	}
	return ""
}

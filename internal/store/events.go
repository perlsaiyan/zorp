package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Event is a cached calendar item. For an occurrence of a series, Details
// are the series master's.
type Event struct {
	GraphID        string
	Type           string // singleInstance, occurrence, exception, seriesMaster
	SeriesMasterID string
	Start, End     time.Time
	Details        EventDetails
}

// EventDetails is everything about an event but its times.
type EventDetails struct {
	Subject     string     `json:"subject,omitempty"`
	AllDay      bool       `json:"all_day,omitempty"`
	Location    string     `json:"location,omitempty"`
	Organizer   Address    `json:"organizer"`
	IsOrganizer bool       `json:"is_organizer,omitempty"`
	Response    string     `json:"response,omitempty"` // the user's: none, accepted, tentativelyAccepted, declined, organizer, notResponded
	ShowAs      string     `json:"show_as,omitempty"`  // free, tentative, busy, oof, workingElsewhere
	Cancelled   bool       `json:"cancelled,omitempty"`
	JoinURL     string     `json:"join_url,omitempty"`
	WebLink     string     `json:"web_link,omitempty"`
	Preview     string     `json:"preview,omitempty"` // start of the description
	Attendees   []Attendee `json:"attendees,omitempty"`
}

// Attendee is someone invited, and their answer.
type Attendee struct {
	Address
	Type     string `json:"type,omitempty"` // required, optional, resource
	Response string `json:"response,omitempty"`
}

// Busy reports whether the event takes up the user's time: not
// cancelled, declined or shown as free.
func (d EventDetails) Busy() bool {
	return !d.Cancelled && d.Response != "declined" && d.ShowAs != "free"
}

// CalendarSync is where the calendar delta pass stands.
type CalendarSync struct {
	DeltaLink   string
	WindowStart time.Time
	WindowEnd   time.Time
	SyncedAt    time.Time
}

func (s *Store) CalendarSync(ctx context.Context, accountID int64) (CalendarSync, error) {
	var c CalendarSync
	var start, end, at int64
	err := s.db.QueryRowContext(ctx, `SELECT delta_link, window_start, window_end, synced_at FROM calendar_sync WHERE account_id = ?`,
		accountID).Scan(&c.DeltaLink, &start, &end, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	c.WindowStart, c.WindowEnd = unixOrZero(start), unixOrZero(end)
	c.SyncedAt = unixOrZero(at)
	return c, err
}

func unixOrZero(t int64) time.Time {
	if t == 0 {
		return time.Time{}
	}
	return time.Unix(t, 0)
}

// ApplyEvents writes a page of calendar changes. With replace, the
// cached calendar is first emptied: the page starts a fresh pass. With a
// link, the pass is done: it's saved, with its window.
func (s *Store) ApplyEvents(ctx context.Context, accountID int64, replace bool, upserts []Event, removed []string, link string, window [2]time.Time, now time.Time) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if replace {
			if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE account_id = ?`, accountID); err != nil {
				return err
			}
			// Until the pass finishes, there's no link to resume from.
			if _, err := tx.ExecContext(ctx, `UPDATE calendar_sync SET delta_link = '' WHERE account_id = ?`, accountID); err != nil {
				return err
			}
		}
		for _, id := range removed {
			if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE account_id = ? AND (graph_id = ? OR series_master_id = ?)`,
				accountID, id, id); err != nil {
				return err
			}
		}
		for _, e := range upserts {
			details := ""
			if e.Type != "occurrence" {
				b, err := json.Marshal(e.Details)
				if err != nil {
					return err
				}
				details = string(b)
			}
			_, err := tx.ExecContext(ctx, `
				INSERT INTO events (account_id, graph_id, type, series_master_id, start_at, end_at, details_json)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (account_id, graph_id) DO UPDATE SET
					type = excluded.type, series_master_id = excluded.series_master_id,
					start_at = excluded.start_at, end_at = excluded.end_at, details_json = excluded.details_json`,
				accountID, e.GraphID, e.Type, e.SeriesMasterID, e.Start.Unix(), e.End.Unix(), details)
			if err != nil {
				return err
			}
		}
		if link == "" {
			return nil
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO calendar_sync (account_id, delta_link, window_start, window_end, synced_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (account_id) DO UPDATE SET delta_link = excluded.delta_link,
				window_start = excluded.window_start, window_end = excluded.window_end, synced_at = excluded.synced_at`,
			accountID, link, window[0].Unix(), window[1].Unix(), now.Unix())
		return err
	})
}

// ForgetCalendarLink drops the saved delta link, so the next pass starts
// from scratch.
func (s *Store) ForgetCalendarLink(ctx context.Context, accountID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE calendar_sync SET delta_link = '' WHERE account_id = ?`, accountID)
	return err
}

// eventSelect reads events with occurrences' details taken from their
// series master.
const eventSelect = `
	SELECT e.graph_id, e.type, e.series_master_id, e.start_at, e.end_at,
		CASE WHEN e.details_json != '' THEN e.details_json ELSE coalesce(m.details_json, '') END
	FROM events e
	LEFT JOIN events m ON m.account_id = e.account_id AND m.graph_id = e.series_master_id AND e.series_master_id != ''`

func scanEvents(rows *sql.Rows) ([]Event, error) {
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var start, end int64
		var details string
		if err := rows.Scan(&e.GraphID, &e.Type, &e.SeriesMasterID, &start, &end, &details); err != nil {
			return nil, err
		}
		e.Start, e.End = time.Unix(start, 0), time.Unix(end, 0)
		// Only ever written by ApplyEvents; a bad row shows without details.
		_ = json.Unmarshal([]byte(details), &e.Details)
		out = append(out, e)
	}
	return out, rows.Err()
}

// EventsBetween returns the events overlapping [from, to), earliest first.
// Series masters are left out: their occurrences stand for them.
func (s *Store) EventsBetween(ctx context.Context, accountID int64, from, to time.Time) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, eventSelect+`
		WHERE e.account_id = ? AND e.type != 'seriesMaster' AND e.start_at < ? AND e.end_at > ?
		ORDER BY e.start_at, e.end_at`, accountID, to.Unix(), from.Unix())
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

// EventByGraphID looks up one cached event.
func (s *Store) EventByGraphID(ctx context.Context, accountID int64, graphID string) (Event, error) {
	rows, err := s.db.QueryContext(ctx, eventSelect+` WHERE e.account_id = ? AND e.graph_id = ?`, accountID, graphID)
	if err != nil {
		return Event{}, err
	}
	evs, err := scanEvents(rows)
	if err != nil {
		return Event{}, err
	}
	if len(evs) == 0 {
		return Event{}, ErrNotFound
	}
	return evs[0], nil
}

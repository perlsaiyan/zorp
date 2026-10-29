package store

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"
)

// Contact is someone the user has mail with, for suggestions while
// writing.
type Contact struct {
	Address
	Score float64 // how much and how lately; higher first
}

// contactHalfLife is how fast a message's weight in Score fades.
const contactHalfLife = 90 * 24 * time.Hour

// Contacts ranks everyone in the cached mail other than self: people the
// user wrote to count three times as much as people who wrote to them,
// and recent mail more than old. Each gets the latest name seen for them.
func (s *Store) Contacts(ctx context.Context, accountID int64, self string, now time.Time) ([]Contact, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.from_name, m.from_addr, m.to_json, m.cc_json, m.received_at, coalesce(f.well_known, '')
		FROM messages m JOIN folders f ON f.id = m.folder_id
		WHERE m.account_id = ? AND m.deleted = 0
		ORDER BY m.received_at`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	self = strings.ToLower(self)
	byAddr := map[string]*Contact{}
	add := func(a Address, w float64) {
		k := strings.ToLower(strings.TrimSpace(a.Addr))
		if k == "" || k == self || !strings.Contains(k, "@") {
			return
		}
		c := byAddr[k]
		if c == nil {
			c = &Contact{Address: Address{Addr: strings.TrimSpace(a.Addr)}}
			byAddr[k] = c
		}
		c.Score += w
		if n := strings.TrimSpace(a.Name); n != "" && !strings.EqualFold(n, a.Addr) {
			c.Name = n // rows come oldest first: the last name wins
		}
	}
	for rows.Next() {
		var fromName, fromAddr, to, cc, wellKnown string
		var at int64
		if err := rows.Scan(&fromName, &fromAddr, &to, &cc, &at, &wellKnown); err != nil {
			return nil, err
		}
		age := now.Sub(time.Unix(at, 0))
		w := math.Exp2(-math.Max(age.Hours(), 0) / contactHalfLife.Hours())
		var toList, ccList []Address
		_ = json.Unmarshal([]byte(to), &toList)
		_ = json.Unmarshal([]byte(cc), &ccList)
		sent := wellKnown == "sentitems" || strings.EqualFold(fromAddr, self)
		if sent {
			w *= 3
		} else {
			add(Address{Name: fromName, Addr: fromAddr}, w)
			w /= 2 // others on mail to the user
		}
		for _, a := range append(toList, ccList...) {
			add(a, w)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Contact, 0, len(byAddr))
	for _, c := range byAddr {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Addr < out[j].Addr
	})
	return out, nil
}

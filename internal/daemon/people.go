package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"github.com/perlsaiyan/zorp/internal/graph"
	"github.com/perlsaiyan/zorp/internal/ipc"
)

// people looks someone up: first among the people the user works with,
// then in the whole directory. Read-only.
func (d *daemon) people(ctx context.Context, params json.RawMessage) ([]ipc.Person, error) {
	var p ipc.PeopleParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	if !d.cfg.Contacts.Directory {
		return nil, errors.New("directory lookups are off (contacts.directory = false)")
	}
	q := strings.TrimSpace(p.Query)
	if len([]rune(q)) < 2 {
		return nil, nil
	}
	top := p.Top
	if top <= 0 || top > 25 {
		top = 10
	}
	var wg sync.WaitGroup
	var people, users []graph.Person
	var perr, uerr error
	wg.Add(2)
	go func() { defer wg.Done(); people, perr = d.dir.People(ctx, q, top) }()
	go func() { defer wg.Done(); users, uerr = d.dir.Users(ctx, q, top) }()
	wg.Wait()
	if perr != nil && uerr != nil {
		return nil, perr
	}
	seen := map[string]bool{}
	var out []ipc.Person
	for _, ps := range [][]graph.Person{people, users} {
		for _, x := range ps {
			k := strings.ToLower(x.Addr)
			if seen[k] || len(out) >= top {
				continue
			}
			seen[k] = true
			title := x.JobTitle
			if x.Department != "" {
				if title != "" {
					title += ", "
				}
				title += x.Department
			}
			out = append(out, ipc.Person{Name: x.Name, Addr: x.Addr, Title: title})
		}
	}
	return out, nil
}

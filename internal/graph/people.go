package graph

import (
	"context"
	"net/url"
	"strconv"
	"strings"
)

// Person is someone found in the directory, with their best address.
type Person struct {
	Name       string
	Addr       string
	JobTitle   string
	Department string
}

// People searches the people the user works with, ranked by relevance
// (People.Read). An empty query lists the most relevant.
func (c *Client) People(ctx context.Context, query string, top int) ([]Person, error) {
	q := url.Values{
		"$top":    {strconv.Itoa(top)},
		"$select": {"displayName,scoredEmailAddresses,jobTitle,department"},
	}
	if query = strings.TrimSpace(query); query != "" {
		q.Set("$search", `"`+strings.ReplaceAll(query, `"`, "")+`"`)
	}
	var page struct {
		Value []struct {
			DisplayName          string `json:"displayName"`
			JobTitle             string `json:"jobTitle"`
			Department           string `json:"department"`
			ScoredEmailAddresses []struct {
				Address string `json:"address"`
			} `json:"scoredEmailAddresses"`
		} `json:"value"`
	}
	if err := c.get(ctx, "/me/people?"+q.Encode(), &page); err != nil {
		return nil, err
	}
	var out []Person
	for _, p := range page.Value {
		if len(p.ScoredEmailAddresses) == 0 || p.ScoredEmailAddresses[0].Address == "" {
			continue
		}
		out = append(out, Person{Name: p.DisplayName, Addr: p.ScoredEmailAddresses[0].Address, JobTitle: p.JobTitle, Department: p.Department})
	}
	return out, nil
}

// Users finds people in the organization's directory whose name or
// address starts with query (User.ReadBasic.All).
func (c *Client) Users(ctx context.Context, query string, top int) ([]Person, error) {
	query = strings.ReplaceAll(strings.TrimSpace(query), "'", "''")
	if query == "" {
		return nil, nil
	}
	q := url.Values{
		"$top":    {strconv.Itoa(top)},
		"$select": {"displayName,mail,userPrincipalName,jobTitle,department"},
		"$filter": {"startswith(displayName,'" + query + "') or startswith(givenName,'" + query + "') or startswith(surname,'" + query + "') or startswith(mail,'" + query + "')"},
	}
	var page struct {
		Value []struct {
			DisplayName       string `json:"displayName"`
			Mail              string `json:"mail"`
			UserPrincipalName string `json:"userPrincipalName"`
			JobTitle          string `json:"jobTitle"`
			Department        string `json:"department"`
		} `json:"value"`
	}
	if err := c.get(ctx, "/users?"+q.Encode(), &page); err != nil {
		return nil, err
	}
	var out []Person
	for _, u := range page.Value {
		addr := u.Mail
		if addr == "" {
			continue // no mailbox: a room list, a service account
		}
		out = append(out, Person{Name: u.DisplayName, Addr: addr, JobTitle: u.JobTitle, Department: u.Department})
	}
	return out, nil
}

package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type User struct {
	DisplayName       string `json:"displayName"`
	Mail              string `json:"mail"`
	UserPrincipalName string `json:"userPrincipalName"`
}

type EmailAddress struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

type Recipient struct {
	EmailAddress EmailAddress `json:"emailAddress"`
}

type Flag struct {
	FlagStatus string `json:"flagStatus"` // notFlagged, flagged, complete
}

type Message struct {
	ID                string      `json:"id"`
	ConversationID    string      `json:"conversationId"`
	InternetMessageID string      `json:"internetMessageId"`
	Subject           string      `json:"subject"`
	From              *Recipient  `json:"from"` // null for some drafts and system messages
	ToRecipients      []Recipient `json:"toRecipients"`
	CcRecipients      []Recipient `json:"ccRecipients"`
	ReplyTo           []Recipient `json:"replyTo"`
	ReceivedDateTime  time.Time   `json:"receivedDateTime"`
	IsRead            bool        `json:"isRead"`
	HasAttachments    bool        `json:"hasAttachments"`
	Importance        string      `json:"importance"`
	Flag              *Flag       `json:"flag"`
	Categories        []string    `json:"categories"`
	BodyPreview       string      `json:"bodyPreview"`
	ParentFolderID    string      `json:"parentFolderId"`

	// Removed is set on delta items for messages that left the folder.
	Removed *struct {
		Reason string `json:"reason"`
	} `json:"@removed"`
}

// messageSelect is the $select list for message summaries.
const messageSelect = "id,conversationId,internetMessageId,subject,from,toRecipients,ccRecipients,replyTo," +
	"receivedDateTime,isRead,hasAttachments,importance,flag,categories,bodyPreview,parentFolderId"

type MailFolder struct {
	ID               string `json:"id"`
	DisplayName      string `json:"displayName"`
	ParentFolderID   string `json:"parentFolderId"`
	ChildFolderCount int    `json:"childFolderCount"`
	UnreadItemCount  int    `json:"unreadItemCount"`
	TotalItemCount   int    `json:"totalItemCount"`
}

// WellKnownFolders are the well-known folder names zorp resolves to IDs.
var WellKnownFolders = []string{"inbox", "sentitems", "drafts", "deleteditems", "archive", "junkemail", "outbox"}

// Me returns the signed-in user.
func (c *Client) Me(ctx context.Context) (*User, error) {
	var u User
	if err := c.get(ctx, "/me?$select=displayName,mail,userPrincipalName", &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// RecentInbox returns the newest n messages in the Inbox (summary fields only).
func (c *Client) RecentInbox(ctx context.Context, n int) ([]Message, error) {
	q := url.Values{}
	q.Set("$top", strconv.Itoa(n))
	q.Set("$select", "id,subject,from,receivedDateTime,isRead")
	q.Set("$orderby", "receivedDateTime desc")
	var page struct {
		Value []Message `json:"value"`
	}
	if err := c.get(ctx, "/me/mailFolders/inbox/messages?"+q.Encode(), &page); err != nil {
		return nil, err
	}
	return page.Value, nil
}

const folderSelect = "$top=100&$select=id,displayName,parentFolderId,childFolderCount,unreadItemCount,totalItemCount"

// MailFolders returns every mail folder in the mailbox, recursing into child folders.
func (c *Client) MailFolders(ctx context.Context) ([]MailFolder, error) {
	var all []MailFolder
	var walk func(path string) error
	walk = func(path string) error {
		for path != "" {
			var page struct {
				Value    []MailFolder `json:"value"`
				NextLink string       `json:"@odata.nextLink"`
			}
			if err := c.get(ctx, path, &page); err != nil {
				return err
			}
			for _, f := range page.Value {
				all = append(all, f)
				if f.ChildFolderCount > 0 {
					if err := walk("/me/mailFolders/" + url.PathEscape(f.ID) + "/childFolders?" + folderSelect); err != nil {
						return err
					}
				}
			}
			path = page.NextLink
		}
		return nil
	}
	if err := walk("/me/mailFolders?" + folderSelect); err != nil {
		return nil, err
	}
	return all, nil
}

// WellKnownFolder looks up a folder by well-known name ("inbox", "archive", ...).
// It returns (nil, nil) if the mailbox doesn't have that folder.
func (c *Client) WellKnownFolder(ctx context.Context, name string) (*MailFolder, error) {
	var f MailFolder
	err := c.get(ctx, "/me/mailFolders/"+name+"?$select=id,displayName", &f)
	if IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// DeltaPage is one page of a messages delta query.
type DeltaPage struct {
	Messages []Message // added or changed
	Removed  []string  // IDs that left the folder
	// Partial are IDs of messages that changed but came with only the
	// changed properties (e.g. isRead); fetch them in full (Messages).
	Partial []string
	// Exactly one of NextLink (more pages follow) or DeltaLink (pass complete,
	// save for next time) is set.
	NextLink  string
	DeltaLink string
}

// InitialDeltaURL starts a delta pass over a folder. A non-zero since limits
// it to messages received at or after that time.
func (c *Client) InitialDeltaURL(folderID string, since time.Time) string {
	q := url.Values{}
	q.Set("$select", messageSelect)
	if !since.IsZero() {
		q.Set("$filter", "receivedDateTime ge "+since.UTC().Format(time.RFC3339))
	}
	return c.baseURL + "/me/mailFolders/" + url.PathEscape(folderID) + "/messages/delta?" + q.Encode()
}

// MessagesDelta fetches one page of a delta pass from a URL returned by
// InitialDeltaURL or a previous page's NextLink/DeltaLink.
func (c *Client) MessagesDelta(ctx context.Context, link string) (*DeltaPage, error) {
	var raw struct {
		Value     []json.RawMessage `json:"value"`
		NextLink  string            `json:"@odata.nextLink"`
		DeltaLink string            `json:"@odata.deltaLink"`
	}
	if err := c.get(ctx, link, &raw, "odata.maxpagesize=100"); err != nil {
		return nil, err
	}
	page := &DeltaPage{NextLink: raw.NextLink, DeltaLink: raw.DeltaLink}
	for _, item := range raw.Value {
		var m Message
		// Every full item has receivedDateTime; an update can come with
		// only the properties that changed.
		var has struct {
			Received json.RawMessage `json:"receivedDateTime"`
		}
		if err := json.Unmarshal(item, &m); err != nil {
			return nil, fmt.Errorf("graph: delta item: %w", err)
		}
		json.Unmarshal(item, &has)
		switch {
		case m.Removed != nil:
			page.Removed = append(page.Removed, m.ID)
		case has.Received == nil:
			page.Partial = append(page.Partial, m.ID)
		default:
			page.Messages = append(page.Messages, m)
		}
	}
	if page.NextLink == "" && page.DeltaLink == "" {
		return nil, errors.New("graph: delta page has neither nextLink nor deltaLink")
	}
	return page, nil
}

// IsSyncStateExpired reports whether a delta link is no longer valid and the
// pass must restart from scratch.
func IsSyncStateExpired(err error) bool {
	var gerr *Error
	if !errors.As(err, &gerr) {
		return false
	}
	return gerr.Status == http.StatusGone || strings.Contains(strings.ToLower(gerr.Code), "syncstate")
}

// SearchMessages runs a server-side KQL search over the whole mailbox and
// returns up to limit message summaries. Graph orders results by date,
// newest first, and caps them at 1000.
func (c *Client) SearchMessages(ctx context.Context, kql string, limit int) ([]Message, error) {
	q := url.Values{}
	// The whole query is one quoted string; quotes inside are escaped.
	q.Set("$search", `"`+strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(kql)+`"`)
	q.Set("$select", messageSelect)
	q.Set("$top", strconv.Itoa(min(limit, 100)))
	path := "/me/messages?" + q.Encode()
	var out []Message
	for path != "" && len(out) < limit {
		var page struct {
			Value    []Message `json:"value"`
			NextLink string    `json:"@odata.nextLink"`
		}
		if err := c.get(ctx, path, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Value...)
		path = page.NextLink
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

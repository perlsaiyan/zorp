package graph

import (
	"context"
	"net/url"
	"strconv"
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

type Message struct {
	ID               string     `json:"id"`
	Subject          string     `json:"subject"`
	From             *Recipient `json:"from"` // null for some drafts and system messages
	ReceivedDateTime time.Time  `json:"receivedDateTime"`
	IsRead           bool       `json:"isRead"`
}

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

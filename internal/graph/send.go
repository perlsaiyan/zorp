package graph

import "context"

// OutgoingMessage is a plain-text message to send.
type OutgoingMessage struct {
	Subject string
	Body    string
	To      []string
	Cc      []string
}

func recipients(addrs []string) []Recipient {
	out := make([]Recipient, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, Recipient{EmailAddress: EmailAddress{Address: a}})
	}
	return out
}

// SendMail sends a message and saves it to Sent Items. It is never
// retried after an error that might mean it was sent.
func (c *Client) SendMail(ctx context.Context, m OutgoingMessage) error {
	body := map[string]any{
		"message": map[string]any{
			"subject":      m.Subject,
			"body":         map[string]string{"contentType": "text", "content": m.Body},
			"toRecipients": recipients(m.To),
			"ccRecipients": recipients(m.Cc),
		},
		"saveToSentItems": true,
	}
	return c.postOnce(ctx, "/me/sendMail", body, nil)
}

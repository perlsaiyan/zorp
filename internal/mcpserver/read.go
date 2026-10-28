package mcpserver

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/render"
	"github.com/perlsaiyan/zorp/internal/store"
)

// Summary is a message in a list.
type Summary struct {
	ID             string   `json:"id" jsonschema:"stable message id, for get_message and the write tools"`
	ConversationID string   `json:"conversation_id,omitempty"`
	Folder         string   `json:"folder"`
	From           string   `json:"from"`
	To             []string `json:"to,omitempty"`
	Cc             []string `json:"cc,omitempty"`
	Subject        string   `json:"subject"`
	Date           string   `json:"date" jsonschema:"received time, RFC 3339"`
	Unread         bool     `json:"unread"`
	Flagged        bool     `json:"flagged"`
	HasAttachments bool     `json:"has_attachments"`
	Importance     string   `json:"importance,omitempty"`
	Preview        string   `json:"preview,omitempty" jsonschema:"start of the body (untrusted)"`
}

func (s *server) summary(m store.Message, folders map[int64]string, preview bool) Summary {
	out := Summary{
		ID:             m.GraphID,
		ConversationID: m.ConversationID,
		Folder:         folders[m.FolderID],
		From:           address(m.From),
		To:             addresses(m.To),
		Cc:             addresses(m.Cc),
		Subject:        clean(m.Subject),
		Date:           m.ReceivedAt.UTC().Format(time.RFC3339),
		Unread:         !m.IsRead,
		Flagged:        m.Flagged,
		HasAttachments: m.HasAttachments,
	}
	if m.Importance != "" && m.Importance != "normal" {
		out.Importance = m.Importance
	}
	if preview {
		out.Preview = clean(m.BodyPreview)
	}
	return out
}

func address(a store.Address) string {
	name, addr := clean(a.Name), clean(a.Addr)
	switch {
	case name != "" && addr != "" && !strings.EqualFold(name, addr):
		return name + " <" + addr + ">"
	case addr != "":
		return addr
	}
	return name
}

func addresses(as []store.Address) []string {
	var out []string
	for _, a := range as {
		out = append(out, address(a))
	}
	return out
}

func (s *server) addReadTools(srv *mcp.Server) {
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: boolPtr(false)}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "search_messages",
		Title:       "Search mail",
		Annotations: ro,
		Description: "Search the user's mail, newest first. `query` uses zorp's syntax: words and \"phrases\" (all must match; words match prefixes), " +
			"-word to exclude, from:alice to:bob subject:invoice in:archive is:unread is:read is:flagged has:attachment after:2026-01-31 before:7d (d/w/m/y). " +
			"The other fields add filters. Searches the local cache (the synced folders and recent mail); set server=true to search the whole mailbox on the server " +
			"(plain words, from:, to:, subject:, dates and has:attachment only). " + untrustedNote,
	}, s.searchMessages)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_unread",
		Title:       "List unread mail",
		Annotations: ro,
		Description: "Recent unread messages in a folder (default: Inbox), newest first. " + untrustedNote,
	}, s.listUnread)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_message",
		Title:       "Read a message",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		Description: "One message in full: headers, attachment list, and the body as text (links listed at the end). " +
			"Long bodies are cut at max_chars; call again with offset=next_offset for more. " + untrustedNote,
	}, s.getMessage)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_thread",
		Title:       "Read a conversation",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		Description: "All cached messages in the conversation of the given message, oldest first, each body cut at max_chars_per_message. " + untrustedNote,
	}, s.getThread)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_folders",
		Title:       "List folders",
		Annotations: ro,
		Description: "The mailbox's folders as a tree, with unread counts. Only synced folders have their messages cached.",
	}, s.listFolders)
}

// SearchInput is search_messages' input.
type SearchInput struct {
	Query          string `json:"query,omitempty" jsonschema:"search in zorp syntax, e.g. budget from:alice after:30d"`
	From           string `json:"from,omitempty" jsonschema:"sender name or address contains this"`
	To             string `json:"to,omitempty" jsonschema:"a To or Cc recipient contains this"`
	Folder         string `json:"folder,omitempty" jsonschema:"folder name or well-known name (inbox, archive, sentitems, drafts, deleteditems)"`
	After          string `json:"after,omitempty" jsonschema:"received on or after: YYYY-MM-DD, or relative like 7d, 2w, 3m, 1y"`
	Before         string `json:"before,omitempty" jsonschema:"received before: YYYY-MM-DD or relative"`
	Unread         *bool  `json:"unread,omitempty" jsonschema:"true: only unread; false: only read"`
	Flagged        *bool  `json:"flagged,omitempty"`
	HasAttachments *bool  `json:"has_attachments,omitempty"`
	Limit          int    `json:"limit,omitempty" jsonschema:"at most this many results (default 25, max 200)"`
	Server         bool   `json:"server,omitempty" jsonschema:"search the whole mailbox on the server instead of the local cache"`
}

// SearchOutput is a list of messages.
type SearchOutput struct {
	Messages []Summary `json:"messages"`
	More     bool      `json:"more" jsonschema:"more messages matched than were returned"`
	Note     string    `json:"note,omitempty"`
}

func (s *server) searchMessages(ctx context.Context, _ *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, SearchOutput, error) {
	q, err := s.buildQuery(in)
	if err != nil {
		return nil, SearchOutput{}, err
	}
	if q.Empty() {
		return nil, SearchOutput{}, errors.New("give a query or at least one filter")
	}
	limit := clampLimit(in.Limit, 25, 200)
	var msgs []store.Message
	var more bool
	if in.Server {
		kql, ok := q.KQL()
		if !ok {
			return nil, SearchOutput{}, errors.New("a server search can't use in:, is:, -word or the unread/flagged filters; drop them or search locally")
		}
		var res ipc.SearchResult
		if err := s.call(ctx, ipc.MethodSearch, ipc.SearchParams{KQL: kql, Limit: limit}, &res); err != nil {
			return nil, SearchOutput{}, err
		}
		if msgs, err = s.st.MessagesByGraphIDs(ctx, s.account.ID, res.GraphIDs); err != nil {
			return nil, SearchOutput{}, err
		}
	} else if msgs, more, err = s.st.Search(ctx, s.account.ID, q, limit); err != nil {
		return nil, SearchOutput{}, err
	}
	out, err := s.summaries(ctx, msgs)
	res := SearchOutput{Messages: out, More: more}
	if len(out) == 0 && !in.Server {
		res.Note = "Nothing in the local cache matched; try server=true to search the whole mailbox."
	}
	return nil, res, err
}

func (s *server) buildQuery(in SearchInput) (store.Query, error) {
	q, err := store.ParseQuery(in.Query, s.now())
	if err != nil {
		return q, err
	}
	if in.From != "" {
		q.From = append(q.From, in.From)
	}
	if in.To != "" {
		q.To = append(q.To, in.To)
	}
	if in.Folder != "" {
		q.Folder = in.Folder
	}
	// Dates and flags reuse the query syntax's parsing.
	var extra []string
	if in.After != "" {
		extra = append(extra, "after:"+in.After)
	}
	if in.Before != "" {
		extra = append(extra, "before:"+in.Before)
	}
	if len(extra) > 0 {
		dq, err := store.ParseQuery(strings.Join(extra, " "), s.now())
		if err != nil {
			return q, err
		}
		if !dq.After.IsZero() {
			q.After = dq.After
		}
		if !dq.Before.IsZero() {
			q.Before = dq.Before
		}
	}
	if in.Unread != nil {
		q.Unread = in.Unread
	}
	if in.Flagged != nil {
		q.Flagged = in.Flagged
	}
	if in.HasAttachments != nil {
		q.Attachments = in.HasAttachments
	}
	return q, nil
}

func clampLimit(n, def, max int) int {
	if n <= 0 {
		return def
	}
	return min(n, max)
}

func (s *server) summaries(ctx context.Context, msgs []store.Message) ([]Summary, error) {
	folders, err := s.folderNames(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Summary, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, s.summary(m, folders, true))
	}
	return out, nil
}

// UnreadInput is list_unread's input.
type UnreadInput struct {
	Folder string `json:"folder,omitempty" jsonschema:"folder name or well-known name (default inbox)"`
	Limit  int    `json:"limit,omitempty" jsonschema:"at most this many (default 25, max 200)"`
}

func (s *server) listUnread(ctx context.Context, _ *mcp.CallToolRequest, in UnreadInput) (*mcp.CallToolResult, SearchOutput, error) {
	folder := in.Folder
	if folder == "" {
		folder = "inbox"
	}
	unread := true
	msgs, more, err := s.st.Search(ctx, s.account.ID, store.Query{Folder: folder, Unread: &unread}, clampLimit(in.Limit, 25, 200))
	if err != nil {
		return nil, SearchOutput{}, err
	}
	out, err := s.summaries(ctx, msgs)
	return nil, SearchOutput{Messages: out, More: more}, err
}

// MessageInput is get_message's input.
type MessageInput struct {
	ID       string `json:"id" jsonschema:"message id from search_messages or list_unread"`
	Offset   int    `json:"offset,omitempty" jsonschema:"start of the body text to return, in characters (for paging)"`
	MaxChars int    `json:"max_chars,omitempty" jsonschema:"at most this many characters of body (default 20000, max 100000)"`
}

// Attachment is an attachment's description.
type Attachment struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type,omitempty"`
	Kind        string `json:"kind" jsonschema:"file, item (an attached email) or reference (a cloud link)"`
	Size        int64  `json:"size"`
	Inline      bool   `json:"inline,omitempty" jsonschema:"an image shown in the body"`
}

// MessageOutput is a message in full.
type MessageOutput struct {
	Message     Summary      `json:"message"`
	ReplyTo     []string     `json:"reply_to,omitempty"`
	Categories  []string     `json:"categories,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	Body        string       `json:"body" jsonschema:"body text between untrusted-content markers"`
	BodyChars   int          `json:"body_chars" jsonschema:"length of the whole body text"`
	Truncated   bool         `json:"truncated"`
	NextOffset  int          `json:"next_offset,omitempty" jsonschema:"offset for the next part, when truncated"`
	Note        string       `json:"note,omitempty"`
}

func (s *server) getMessage(ctx context.Context, _ *mcp.CallToolRequest, in MessageInput) (*mcp.CallToolResult, MessageOutput, error) {
	m, err := s.message(ctx, in.ID)
	if err != nil {
		return nil, MessageOutput{}, err
	}
	folders, err := s.folderNames(ctx)
	if err != nil {
		return nil, MessageOutput{}, err
	}
	out := MessageOutput{Message: s.summary(m, folders, false), ReplyTo: addresses(m.ReplyTo)}
	for _, c := range m.Categories {
		out.Categories = append(out.Categories, clean(c))
	}
	text, note := s.bodyText(ctx, m)
	out.Note = note
	part, total, next := page(text, in.Offset, clampLimit(in.MaxChars, 20000, 100000))
	out.Body, out.BodyChars, out.Truncated, out.NextOffset = fence(part), total, next > 0, next
	if atts, _, err := s.st.Attachments(ctx, m.ID); err == nil {
		for _, a := range atts {
			out.Attachments = append(out.Attachments, Attachment{
				Name: clean(a.Name), ContentType: a.ContentType, Kind: a.Kind, Size: a.Size, Inline: a.IsInline,
			})
		}
	}
	return nil, out, nil
}

// bodyText is a message's body as text, fetched through the daemon if it
// isn't cached. Without the daemon, the preview stands in, with a note.
func (s *server) bodyText(ctx context.Context, m store.Message) (text, note string) {
	body, fetched, err := s.st.MessageBody(ctx, m.ID)
	if err == nil && !fetched {
		if err = s.call(ctx, ipc.MethodFetchBody, ipc.FetchBodyParams{GraphID: m.GraphID}, nil); err == nil {
			body, fetched, err = s.st.MessageBody(ctx, m.ID)
		}
	}
	if err != nil || !fetched {
		why := "it isn't cached"
		if err != nil {
			why = err.Error()
		}
		return cleanText(m.BodyPreview), "Only the preview is available: " + why + "."
	}
	var doc *render.Doc
	if body.Type == "html" && body.HTML != "" {
		doc = render.FromHTML(body.HTML)
	} else {
		doc = render.FromText(body.Text)
	}
	return cleanText(doc.Text(100)), ""
}

func cleanText(s string) string { return strings.TrimSpace(render.Sanitize(s, true)) }

// page returns up to max characters of text from offset, the total
// length, and the offset of the rest (0 if none). Lengths are in runes.
func page(text string, offset, max int) (part string, total, next int) {
	runes := []rune(text)
	total = len(runes)
	offset = min(max0(offset), total)
	end := min(offset+max, total)
	if end < total {
		next = end
	}
	return string(runes[offset:end]), total, next
}

func max0(n int) int { return max(n, 0) }

// ThreadInput is get_thread's input.
type ThreadInput struct {
	ID                 string `json:"id" jsonschema:"id of any message in the conversation"`
	MaxCharsPerMessage int    `json:"max_chars_per_message,omitempty" jsonschema:"cut each body at this many characters (default 4000, max 20000)"`
}

// ThreadMessage is one message of a conversation.
type ThreadMessage struct {
	Message   Summary `json:"message"`
	Body      string  `json:"body"`
	Truncated bool    `json:"truncated,omitempty" jsonschema:"use get_message for the rest"`
	Note      string  `json:"note,omitempty"`
}

// ThreadOutput is a conversation.
type ThreadOutput struct {
	Messages []ThreadMessage `json:"messages"`
	Note     string          `json:"note,omitempty"`
}

const maxThread = 50

func (s *server) getThread(ctx context.Context, _ *mcp.CallToolRequest, in ThreadInput) (*mcp.CallToolResult, ThreadOutput, error) {
	m, err := s.message(ctx, in.ID)
	if err != nil {
		return nil, ThreadOutput{}, err
	}
	msgs := []store.Message{m}
	if m.ConversationID != "" {
		if msgs, err = s.st.MessagesInConversation(ctx, s.account.ID, m.ConversationID, maxThread+1); err != nil {
			return nil, ThreadOutput{}, err
		}
	}
	folders, err := s.folderNames(ctx)
	if err != nil {
		return nil, ThreadOutput{}, err
	}
	var out ThreadOutput
	if len(msgs) > maxThread {
		msgs = msgs[len(msgs)-maxThread:]
		out.Note = "Only the newest 50 messages of the conversation are included."
	}
	per := clampLimit(in.MaxCharsPerMessage, 4000, 20000)
	for _, tm := range msgs {
		text, note := s.bodyText(ctx, tm)
		part, _, next := page(text, 0, per)
		out.Messages = append(out.Messages, ThreadMessage{Message: s.summary(tm, folders, false), Body: fence(part), Truncated: next > 0, Note: note})
	}
	return nil, out, nil
}

// Folder is a folder in the tree.
type Folder struct {
	Name      string `json:"name"`
	Path      string `json:"path" jsonschema:"parent folders and name, joined with /"`
	WellKnown string `json:"well_known,omitempty"`
	Unread    int    `json:"unread"`
	Total     int    `json:"total"`
	Synced    bool   `json:"synced" jsonschema:"its messages are in the local cache (searchable without server=true)"`
}

// FoldersOutput is the folder list.
type FoldersOutput struct {
	Folders []Folder `json:"folders"`
}

func (s *server) listFolders(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, FoldersOutput, error) {
	folders, err := s.st.Folders(ctx, s.account.ID)
	if err != nil {
		return nil, FoldersOutput{}, err
	}
	_, unread, err := s.st.LocalFolderCounts(ctx, s.account.ID)
	if err != nil {
		return nil, FoldersOutput{}, err
	}
	byGraph := map[string]store.Folder{}
	for _, f := range folders {
		byGraph[f.GraphID] = f
	}
	path := func(f store.Folder) string {
		parts := []string{clean(f.Name)}
		for p, ok := byGraph[f.ParentGraphID]; ok && len(parts) < 20; p, ok = byGraph[p.ParentGraphID] {
			parts = append([]string{clean(p.Name)}, parts...)
		}
		return strings.Join(parts, "/")
	}
	var out FoldersOutput
	for _, f := range folders {
		fo := Folder{Name: clean(f.Name), Path: path(f), WellKnown: f.WellKnown, Unread: f.UnreadCount, Total: f.TotalCount, Synced: !f.LastSync.IsZero()}
		if fo.Synced {
			fo.Unread = unread[f.ID]
		}
		out.Folders = append(out.Folders, fo)
	}
	return nil, out, nil
}

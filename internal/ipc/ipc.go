// Package ipc is the daemon's Unix-socket protocol: newline-delimited JSON.
//
// A client sends {"id":N,"method":"...","params":{...}} and gets back
// {"id":N,"result":...} or {"id":N,"error":"..."}. After a "subscribe"
// request the connection also receives {"event":"...","data":...} lines
// until it closes.
package ipc

import (
	"encoding/json"
)

// Methods understood by the daemon.
const (
	MethodStatus     = "status"
	MethodSync       = "sync"
	MethodReloadAuth = "reload-auth"
	MethodFetchBody  = "fetch-body"
	MethodMutate     = "mutate"
	MethodExportHTML = "export-html"
	MethodSetSync    = "set-sync"
	MethodSearch     = "search"
	MethodCompose    = "compose"
	MethodGetDraft   = "get-draft"
	MethodAttachList = "attachments"
	MethodSaveAttach = "save-attachment"
	MethodSubscribe  = "subscribe"
	MethodShutdown   = "shutdown"
)

// Events pushed to subscribers.
const (
	EventStatus  = "status"  // data: Status
	EventChanged = "changed" // data: Changed
	EventError   = "error"   // data: ErrorEvent
)

type message struct {
	ID     int64           `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Event  string          `json:"event,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}

// Event is a pushed notification.
type Event struct {
	Name string
	Data json.RawMessage
}

// Daemon states reported in Status.State.
const (
	StateStarting   = "starting"
	StateIdle       = "idle"
	StateSyncing    = "syncing"
	StateOffline    = "offline"
	StateNeedsLogin = "needs_login"
)

type Status struct {
	State     string `json:"state"`
	Account   string `json:"account,omitempty"`
	LastSync  int64  `json:"last_sync,omitempty"` // unix seconds
	LastError string `json:"last_error,omitempty"`
	PID       int    `json:"pid"`
	Pending   int    `json:"pending"` // queued changes not yet sent to Graph
	// Guards lists the guardrails in force: "send", "move", "delete"
	// (see config.Safety).
	Guards []string `json:"guards,omitempty"`
}

type SyncParams struct {
	Full bool `json:"full,omitempty"`
}

type FetchBodyParams struct {
	GraphID string `json:"graph_id"`
}

// Mutation ops for MutateParams.Op.
const (
	OpRead    = "read"
	OpUnread  = "unread"
	OpFlag    = "flag"
	OpUnflag  = "unflag"
	OpMove    = "move"    // to FolderID
	OpArchive = "archive" // to the Archive folder
	OpDelete  = "delete"  // to Deleted Items
)

type MutateParams struct {
	Op       string   `json:"op"`
	GraphIDs []string `json:"graph_ids"`
	FolderID int64    `json:"folder_id,omitempty"` // destination for OpMove
}

type MutateResult struct {
	Queued int `json:"queued"`
}

type ExportHTMLParams struct {
	GraphID string `json:"graph_id"`
}

type ExportHTMLResult struct {
	Path string `json:"path"` // standalone HTML file, safe to open in a browser
}

// SetSyncParams turns syncing of a folder's messages on or off.
type SetSyncParams struct {
	FolderID int64 `json:"folder_id"`
	Enabled  bool  `json:"enabled"`
}

// SearchParams asks for a server-side search of the whole mailbox.
type SearchParams struct {
	KQL   string `json:"kql"`             // Graph $search (KQL) query
	Limit int    `json:"limit,omitempty"` // default 100, at most 250
}

// SearchResult lists the matches, which the daemon has cached so clients
// can read them from the store.
type SearchResult struct {
	GraphIDs []string `json:"graph_ids"`
}

type Address struct {
	Name string `json:"name,omitempty"`
	Addr string `json:"addr"`
}

// ComposeParams sends a message or saves it as a draft.
type ComposeParams struct {
	Kind    string    `json:"kind"`          // new, reply, reply-all, forward, draft
	Ref     string    `json:"ref,omitempty"` // original message, or the draft being edited
	To      []Address `json:"to,omitempty"`
	Cc      []Address `json:"cc,omitempty"`
	Bcc     []Address `json:"bcc,omitempty"`
	Subject string    `json:"subject"`
	Body    string    `json:"body"`
	Attach  []string  `json:"attach,omitempty"` // absolute paths
	Keep    []string  `json:"keep,omitempty"`   // existing attachments to keep, by name
	Send    bool      `json:"send"`             // false: save to Drafts
}

// ComposeResult reports the draft used. When it was created but sending
// failed, DraftID is set together with SendError: the message is in Drafts.
type ComposeResult struct {
	DraftID   string `json:"draft_id,omitempty"`
	SendError string `json:"send_error,omitempty"`
}

type MessageRef struct {
	GraphID string `json:"graph_id"`
}

type AttachmentInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	IsInline    bool   `json:"is_inline"`
}

// DraftInfo is a draft's content, for editing as text.
type DraftInfo struct {
	Subject     string           `json:"subject"`
	To          []Address        `json:"to"`
	Cc          []Address        `json:"cc"`
	Bcc         []Address        `json:"bcc"`
	Body        string           `json:"body"`
	WasHTML     bool             `json:"was_html"` // saving will make it plain text
	Attachments []AttachmentInfo `json:"attachments"`
}

type SaveAttachmentParams struct {
	GraphID      string `json:"graph_id"`
	AttachmentID string `json:"attachment_id"`
}

type SaveAttachmentResult struct {
	Path string `json:"path"`
}

// ErrorEvent reports a failure the user should see, e.g. a change Graph
// rejected and that was reverted.
type ErrorEvent struct {
	Message string `json:"message"`
}

// Changed tells clients which folders' cached contents changed.
type Changed struct {
	FolderIDs []int64 `json:"folder_ids"`
}

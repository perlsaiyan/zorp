// Package ipc is the daemon's Unix-socket protocol: newline-delimited JSON.
//
// A client sends {"id":N,"method":"...","params":{...}} and gets back
// {"id":N,"result":...} or {"id":N,"error":"..."}. After a "subscribe"
// request the connection also receives {"event":"...","data":...} lines
// until it closes.
package ipc

import (
	"encoding/json"
	"time"
)

// Methods understood by the daemon.
const (
	MethodStatus      = "status"
	MethodSync        = "sync"
	MethodReloadAuth  = "reload-auth"
	MethodFetchBody   = "fetch-body"
	MethodMutate      = "mutate"
	MethodExportHTML  = "export-html"
	MethodSetSync     = "set-sync"
	MethodSearch      = "search"
	MethodCompose     = "compose"
	MethodGetDraft    = "get-draft"
	MethodAttachList  = "attachments"
	MethodSaveAttach  = "save-attachment"
	MethodImages      = "images"
	MethodOpenAttach  = "open-attachment"
	MethodNotifyTest  = "notify-test"  // result: NotifyTestResult
	MethodMeeting     = "meeting"      // params: MessageRef; result: MeetingInfo
	MethodRespond     = "respond"      // params: RespondParams; result: RespondResult
	MethodEventGet    = "event-get"    // params: EventRef; result: EventDetail
	MethodEventSave   = "event-save"   // params: EventSaveParams; result: EventSaveResult
	MethodEventCancel = "event-cancel" // params: EventCancelParams; result: EventCancelResult
	MethodFreeBusy    = "free-busy"    // params: FreeBusyParams; result: []FreeBusy
	MethodPeople      = "people"       // params: PeopleParams; result: []Person
	MethodSubscribe   = "subscribe"
	MethodShutdown    = "shutdown"
)

// Events pushed to subscribers.
const (
	EventStatus   = "status"   // data: Status
	EventChanged  = "changed"  // data: Changed
	EventError    = "error"    // data: ErrorEvent
	EventNotify   = "notify"   // data: Notify; show it through the terminal
	EventOpen     = "open"     // data: MessageRef; a notification was clicked
	EventCalendar = "calendar" // data: {}; the cached calendar changed
)

// NotifyTestResult says how a test notification was sent.
type NotifyTestResult struct {
	Via string `json:"via"` // "desktop", "terminal" or "none"
}

// Notify is a new-mail notification for a client to show in its terminal
// (when the daemon can't show it on a desktop).
type Notify struct {
	Title   string `json:"title"`
	Body    string `json:"body"`
	GraphID string `json:"graph_id,omitempty"`
}

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

// MeetingInfo is the calendar's side of a meeting message.
type MeetingInfo struct {
	Found     bool   `json:"found"` // the meeting is on the calendar
	EventID   string `json:"event_id,omitempty"`
	Response  string `json:"response,omitempty"`  // the user's answer: none, accepted, tentativelyAccepted, declined, organizer, notResponded
	Cancelled bool   `json:"cancelled,omitempty"` // the organizer cancelled it
	Organizer bool   `json:"organizer,omitempty"` // the user organized it
	JoinURL   string `json:"join_url,omitempty"`
}

// Answers for RespondParams.
const (
	AnswerAccept    = "accept"
	AnswerTentative = "tentative"
	AnswerDecline   = "decline"
	AnswerRemove    = "remove" // take a cancelled meeting off the calendar
)

type RespondParams struct {
	GraphID string `json:"graph_id,omitempty"` // the meeting message, or:
	EventID string `json:"event_id,omitempty"` // the event, answered from the calendar (nothing is archived)
	Answer  string `json:"answer"`
	Comment string `json:"comment,omitempty"`
	Send    bool   `json:"send,omitempty"` // tell the organizer
}

type RespondResult struct {
	// After says what happened to the message afterwards (calendar.after_respond):
	// "archive", "delete" or "" (kept).
	After string `json:"after,omitempty"`
	// AfterError is why that failed, when it did; the answer itself went through.
	AfterError string `json:"after_error,omitempty"`
}

type EventRef struct {
	ID string `json:"id"`
}

// EventDetail is a calendar event as the meeting editor needs it.
type EventDetail struct {
	ID             string    `json:"id"`
	Type           string    `json:"type"` // singleInstance, occurrence, exception, seriesMaster
	SeriesMasterID string    `json:"series_master_id,omitempty"`
	Subject        string    `json:"subject"`
	Start          time.Time `json:"start"`
	End            time.Time `json:"end"`
	AllDay         bool      `json:"all_day,omitempty"`
	Location       string    `json:"location,omitempty"`
	Required       []Address `json:"required,omitempty"`
	Optional       []Address `json:"optional,omitempty"`
	Online         bool      `json:"online,omitempty"`
	JoinURL        string    `json:"join_url,omitempty"`
	BodyType       string    `json:"body_type,omitempty"` // text or html
	Body           string    `json:"body,omitempty"`
	IsOrganizer    bool      `json:"is_organizer,omitempty"`
	Cancelled      bool      `json:"cancelled,omitempty"`
}

// Attendees is who's invited to a meeting.
type Attendees struct {
	Required []Address `json:"required,omitempty"`
	Optional []Address `json:"optional,omitempty"`
}

// EventSaveParams creates a meeting (no ID) or changes one the user
// organized. Nil fields are left as they are; a new meeting needs a
// subject and times. Everyone invited gets the invite or the update.
type EventSaveParams struct {
	ID        string     `json:"id,omitempty"`
	Subject   *string    `json:"subject,omitempty"`
	Start     *time.Time `json:"start,omitempty"`
	End       *time.Time `json:"end,omitempty"`
	AllDay    *bool      `json:"all_day,omitempty"`
	Location  *string    `json:"location,omitempty"`
	Attendees *Attendees `json:"attendees,omitempty"`
	Online    *bool      `json:"online,omitempty"` // add a Teams meeting
	Body      *string    `json:"body,omitempty"`
	Format    string     `json:"format,omitempty"` // the body's: markdown (sent as HTML) or text
}

type EventSaveResult struct {
	ID       string `json:"id"`
	JoinURL  string `json:"join_url,omitempty"`
	Notified int    `json:"notified"` // people sent the invite or update
}

// EventCancelParams cancels a meeting the user organized.
type EventCancelParams struct {
	ID      string `json:"id"`
	Comment string `json:"comment,omitempty"` // to the attendees
}

type EventCancelResult struct {
	Notified int `json:"notified"` // 0: nobody else was invited, so it was just deleted
}

// FreeBusyParams asks when people are busy between Start and End.
type FreeBusyParams struct {
	Addrs []string  `json:"addrs"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// FreeBusy is one person's busy times, as far as their organization
// shares them.
type FreeBusy struct {
	Addr  string `json:"addr"`
	Busy  []Busy `json:"busy,omitempty"`
	Error string `json:"error,omitempty"` // e.g. not shared
}

type Busy struct {
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	Status string    `json:"status"` // tentative, busy, oof, workingElsewhere
}

// PeopleParams looks people up in the directory.
type PeopleParams struct {
	Query string `json:"query"`
	Top   int    `json:"top,omitempty"`
}

// Person is someone found in the directory.
type Person struct {
	Name  string `json:"name,omitempty"`
	Addr  string `json:"addr"`
	Title string `json:"title,omitempty"` // job title, department
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
	Format  string    `json:"format,omitempty"` // "markdown" (sent as HTML) or "text"
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
	Kind        string `json:"kind,omitempty"` // "file", "item" (an attached email) or "reference"
}

// DraftInfo is a draft's content, for editing as text.
type DraftInfo struct {
	Subject     string           `json:"subject"`
	To          []Address        `json:"to"`
	Cc          []Address        `json:"cc"`
	Bcc         []Address        `json:"bcc"`
	Body        string           `json:"body"`
	Format      string           `json:"format,omitempty"` // set when Body is the source zorp saved
	WasHTML     bool             `json:"was_html"`         // converted from HTML; formatting was lost
	Attachments []AttachmentInfo `json:"attachments"`
}

// ImageInfo is a message image cached on disk for display.
type ImageInfo struct {
	ContentID string `json:"content_id,omitempty"` // inline images: the HTML's cid: reference
	Name      string `json:"name"`
	Inline    bool   `json:"inline"`
	Path      string `json:"path"`
}

type SaveAttachmentParams struct {
	GraphID      string `json:"graph_id"`
	AttachmentID string `json:"attachment_id"`
}

type SaveAttachmentResult struct {
	Path string `json:"path"`
}

// OpenAttachmentResult is an attachment fetched for viewing (params:
// SaveAttachmentParams).
type OpenAttachmentResult struct {
	Name        string           `json:"name"`
	ContentType string           `json:"content_type"`
	Kind        string           `json:"kind"`
	Size        int              `json:"size"`
	Path        string           `json:"path,omitempty"`    // file attachments: a private copy
	Message     *AttachedMessage `json:"message,omitempty"` // attached emails
}

// AttachedMessage is an email attached to another.
type AttachedMessage struct {
	Subject  string    `json:"subject"`
	From     Address   `json:"from"`
	To       []Address `json:"to,omitempty"`
	Cc       []Address `json:"cc,omitempty"`
	Date     string    `json:"date,omitempty"` // RFC 3339
	BodyType string    `json:"body_type"`      // "html" or "text"
	Body     string    `json:"body"`
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

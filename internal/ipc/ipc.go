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
	Pending   int    `json:"pending"`   // queued changes not yet sent to Graph
	SafeMode  bool   `json:"safe_mode"` // deletes refused, moves only to the scratch folder
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

// ErrorEvent reports a failure the user should see, e.g. a change Graph
// rejected and that was reverted.
type ErrorEvent struct {
	Message string `json:"message"`
}

// Changed tells clients which folders' cached contents changed.
type Changed struct {
	FolderIDs []int64 `json:"folder_ids"`
}

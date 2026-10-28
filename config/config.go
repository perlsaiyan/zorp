// Package config loads zorp's TOML config and resolves its on-disk paths.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const appName = "zorp"

// Config is the contents of ~/.config/zorp/config.toml.
type Config struct {
	Auth    Auth    `toml:"auth"`
	Sync    Sync    `toml:"sync"`
	Safety  Safety  `toml:"safety"`
	Keys    Keys    `toml:"keys"`
	UI      UI      `toml:"ui"`
	Compose Compose `toml:"compose"`
	MCP     MCP     `toml:"mcp"`
	Notify  Notify  `toml:"notify"`
	// Theme overrides colours by role (see README: Colours), e.g.
	// accent = "#89b4fa" or selected = "magenta".
	Theme map[string]string `toml:"theme"`

	// Paths are resolved at load time, not read from the file.
	Paths Paths `toml:"-"`
}

// Notify configures new-mail notifications (sent by the daemon).
type Notify struct {
	Enabled bool `toml:"enabled"`
	// Method is "auto" (desktop notifications where there's a desktop,
	// else through the terminal zorp runs in), "desktop", "terminal" or
	// "none".
	Method string `toml:"method"`
	// Folders to notify about, by name or well-known name.
	Folders []string `toml:"folders"`
	// From, if set, limits notifications to these senders: addresses or
	// @domains.
	From []string `toml:"from"`
	// QuietHours is a daily "HH:MM-HH:MM" window (local time) without
	// notifications, e.g. "22:00-07:00".
	QuietHours string `toml:"quiet_hours"`
	// ShowContent puts senders and subjects in notifications; false says
	// only how many messages arrived.
	ShowContent bool `toml:"show_content"`
	// GroupAfter: more new messages than this at once make one summary.
	GroupAfter int `toml:"group_after"`
	// ClickCommand runs when a desktop notification is clicked, e.g. a
	// script that focuses or opens zorp.
	ClickCommand string `toml:"click_command"`
}

// QuietWindow parses QuietHours into minutes after midnight. ok is false
// when there are no quiet hours.
func (n Notify) QuietWindow() (from, to int, ok bool, err error) {
	if strings.TrimSpace(n.QuietHours) == "" {
		return 0, 0, false, nil
	}
	a, b, found := strings.Cut(n.QuietHours, "-")
	if !found {
		return 0, 0, false, fmt.Errorf("notify.quiet_hours %q isn't HH:MM-HH:MM", n.QuietHours)
	}
	parse := func(s string) (int, error) {
		t, err := time.Parse("15:04", strings.TrimSpace(s))
		if err != nil {
			return 0, fmt.Errorf("notify.quiet_hours %q isn't HH:MM-HH:MM", n.QuietHours)
		}
		return t.Hour()*60 + t.Minute(), nil
	}
	if from, err = parse(a); err != nil {
		return 0, 0, false, err
	}
	if to, err = parse(b); err != nil {
		return 0, 0, false, err
	}
	return from, to, true, nil
}

// Quiet reports whether t falls in the quiet hours.
func (n Notify) Quiet(t time.Time) bool {
	from, to, ok, err := n.QuietWindow()
	if !ok || err != nil {
		return false
	}
	m := t.Hour()*60 + t.Minute()
	if from <= to {
		return m >= from && m < to
	}
	return m >= from || m < to // across midnight
}

// MCP configures `zorp mcp`, the server that gives AI tools access to the
// local cache.
type MCP struct {
	// AllowWrites enables the tools that change things: create_draft,
	// mark_read, flag_message, move_message. Nothing can ever send mail.
	AllowWrites bool `toml:"allow_writes"`
}

type Auth struct {
	// ClientID is the Application (client) ID of the zorp app registration.
	ClientID string `toml:"client_id"`
	// Tenant is "organizations" (any work/school account), "common", or a tenant ID.
	Tenant string `toml:"tenant"`
}

type Sync struct {
	// Interval between delta polls.
	Interval time.Duration `toml:"interval"`
	// InitialDays limits the first sync of a folder to mail received in the
	// last N days. 0 means no limit.
	InitialDays int `toml:"initial_days"`
	// Folders whose messages are synced, by well-known name ("inbox",
	// "sentitems", "drafts", "archive", ...) or display name. The folder
	// list itself is always synced in full.
	Folders []string `toml:"folders"`
	// PrefetchBodies is how many of the newest messages get their bodies
	// downloaded in the background; older ones are fetched when opened.
	PrefetchBodies int `toml:"prefetch_bodies"`
}

// Safety limits what zorp may change while it's being developed against a
// real mailbox. The daemon enforces it, so it covers every client.
//
// SafeMode sets the default for each guardrail; the Allow* settings
// override it one at a time. Read/unread and flag are always allowed.
type Safety struct {
	// SafeMode turns on every guardrail that isn't set explicitly.
	SafeMode bool `toml:"safe_mode"`
	// AllowDelete permits deleting (moving to Deleted Items).
	AllowDelete *bool `toml:"allow_delete"`
	// AllowMove permits moves (including archive) to any folder; when
	// false, only ScratchFolder is allowed.
	AllowMove     *bool  `toml:"allow_move"`
	ScratchFolder string `toml:"scratch_folder"`
	// AllowSend permits sending to anyone; when false, only to yourself and
	// to SendAllow entries (addresses, or domains like "example.com").
	AllowSend *bool    `toml:"allow_send"`
	SendAllow []string `toml:"send_allow"`
}

func (s Safety) allowed(v *bool) bool {
	if v != nil {
		return *v
	}
	return !s.SafeMode
}

// Guard names, as reported by Guards.
const (
	GuardDelete = "delete"
	GuardMove   = "move"
	GuardSend   = "send"
)

func (s Safety) DeleteAllowed() bool { return s.allowed(s.AllowDelete) }
func (s Safety) MoveAllowed() bool   { return s.allowed(s.AllowMove) }
func (s Safety) SendAllowed() bool   { return s.allowed(s.AllowSend) }

// Guards lists the guardrails in force.
func (s Safety) Guards() []string {
	var g []string
	if !s.SendAllowed() {
		g = append(g, GuardSend)
	}
	if !s.MoveAllowed() {
		g = append(g, GuardMove)
	}
	if !s.DeleteAllowed() {
		g = append(g, GuardDelete)
	}
	return g
}

// MayMoveTo reports whether a move into the named folder is allowed.
func (s Safety) MayMoveTo(folder string) bool {
	return s.MoveAllowed() || strings.EqualFold(folder, s.ScratchFolder)
}

// MaySendTo reports whether mail may be sent to addr. self is the
// signed-in user's address.
func (s Safety) MaySendTo(addr, self string) bool {
	if s.SendAllowed() {
		return true
	}
	addr = strings.ToLower(strings.TrimSpace(addr))
	if addr == "" {
		return false
	}
	if strings.EqualFold(addr, self) {
		return true
	}
	_, domain, _ := strings.Cut(addr, "@")
	for _, a := range s.SendAllow {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == addr || strings.TrimPrefix(a, "@") == domain {
			return true
		}
	}
	return false
}

// Keys adjusts what keys do in the TUI.
type Keys struct {
	// DeleteArchives makes "d" archive instead of delete, for people who
	// never delete mail. "X" still deletes, after confirmation.
	DeleteArchives bool `toml:"delete_archives"`
}

type UI struct {
	// Mouse enables clicking, scrolling and dragging pane dividers. While
	// it's on, most terminals need Shift+drag to select text.
	Mouse bool `toml:"mouse"`
	// SaveDir is where attachments are saved ("~" is expanded).
	SaveDir string `toml:"save_dir"`
	// Open is how links and messages are opened: "browser" (xdg-open),
	// "copy" (to the terminal's clipboard, for SSH), or "auto" (copy when
	// there's no local display or this is an SSH session).
	Open string `toml:"open"`
	// Graphics shows inline images: "auto" (detect), "kitty" (force the
	// Kitty graphics protocol) or "none".
	Graphics string `toml:"graphics"`
	// Color is how much colour to use: "auto" (the terminal's ANSI
	// colours, plus soft tints in a 24-bit terminal that reports its
	// palette), "tint" (tints even without 24-bit detection), "ansi" (no
	// tints) or "none" (no colour at all).
	Color string `toml:"color"`
}

type Compose struct {
	// Editor overrides $VISUAL / $EDITOR for writing messages.
	Editor string `toml:"editor"`
	// Signature is added to new messages, replies and forwards.
	Signature string `toml:"signature"`
	// Format is how messages are written by default: "markdown" (sent as
	// HTML) or "text". A message's own "Format:" line overrides it.
	Format string `toml:"format"`
}

type Paths struct {
	ConfigFile string
	DataDir    string
	TokenCache string
	DB         string
	Socket     string
	DaemonLog  string
	ViewDir    string // messages exported for viewing in a browser
	UIState    string // remembered pane sizes and layout
	ComposeDir string // messages being written, kept until sent or discarded
}

func defaults() Config {
	return Config{
		Auth: Auth{Tenant: "organizations"},
		Sync: Sync{
			Interval:       60 * time.Second,
			InitialDays:    90,
			Folders:        []string{"inbox", "sentitems", "drafts", "archive"},
			PrefetchBodies: 500,
		},
		Safety:  Safety{ScratchFolder: "zorp-test"},
		Keys:    Keys{DeleteArchives: true},
		UI:      UI{Mouse: true, SaveDir: "~/Downloads", Open: "auto", Graphics: "auto", Color: "auto"},
		Compose: Compose{Format: "markdown"},
		Notify:  Notify{Enabled: true, Method: "auto", Folders: []string{"inbox"}, ShowContent: true, GroupAfter: 3},
	}
}

// Load reads the config file if it exists and applies defaults.
func Load() (*Config, error) {
	paths, err := resolvePaths()
	if err != nil {
		return nil, err
	}
	cfg := defaults()
	cfg.Paths = paths

	if _, err := toml.DecodeFile(paths.ConfigFile, &cfg); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("reading %s: %w", paths.ConfigFile, err)
	}
	return &cfg, nil
}

// Validate reports settings that must be present before talking to Microsoft.
func (c *Config) Validate() error {
	if c.Auth.ClientID == "" {
		return fmt.Errorf("auth.client_id is not set in %s (see README: App registration)", c.Paths.ConfigFile)
	}
	if c.Auth.Tenant == "" {
		return fmt.Errorf("auth.tenant is empty in %s", c.Paths.ConfigFile)
	}
	if f := c.Compose.Format; f != "markdown" && f != "text" {
		return fmt.Errorf("compose.format must be \"markdown\" or \"text\" in %s", c.Paths.ConfigFile)
	}
	switch c.UI.Color {
	case "", "auto", "tint", "ansi", "none":
	default:
		return fmt.Errorf("ui.color must be \"auto\", \"tint\", \"ansi\" or \"none\" in %s", c.Paths.ConfigFile)
	}
	switch c.Notify.Method {
	case "auto", "desktop", "terminal", "none":
	default:
		return fmt.Errorf("notify.method must be \"auto\", \"desktop\", \"terminal\" or \"none\" in %s", c.Paths.ConfigFile)
	}
	if _, _, _, err := c.Notify.QuietWindow(); err != nil {
		return fmt.Errorf("%w in %s", err, c.Paths.ConfigFile)
	}
	if c.Sync.Interval < 10*time.Second {
		return fmt.Errorf("sync.interval must be at least 10s in %s", c.Paths.ConfigFile)
	}
	return nil
}

// Authority is the Entra ID authority URL for the configured tenant.
func (c *Config) Authority() string {
	return "https://login.microsoftonline.com/" + c.Auth.Tenant
}

// EnsureDataDir creates the data directory with owner-only permissions.
func (c *Config) EnsureDataDir() error {
	return os.MkdirAll(c.Paths.DataDir, 0o700)
}

func resolvePaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	configHome := envOr("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	dataHome := envOr("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	dataDir := filepath.Join(dataHome, appName)
	// The socket belongs in the per-user runtime dir; fall back to the data
	// dir (which is 0700) when it isn't set.
	socketDir := envOr("XDG_RUNTIME_DIR", dataDir)
	return Paths{
		ConfigFile: filepath.Join(configHome, appName, "config.toml"),
		DataDir:    dataDir,
		TokenCache: filepath.Join(dataDir, "token_cache.json"),
		DB:         filepath.Join(dataDir, "zorp.db"),
		Socket:     filepath.Join(socketDir, appName+".sock"),
		DaemonLog:  filepath.Join(dataDir, "daemon.log"),
		ViewDir:    filepath.Join(socketDir, appName+"-view"),
		UIState:    filepath.Join(dataDir, "ui-state.json"),
		ComposeDir: filepath.Join(dataDir, "compose"),
	}, nil
}

// ExpandHome replaces a leading "~" with the home directory.
func ExpandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

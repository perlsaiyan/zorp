// Package config loads zorp's TOML config and resolves its on-disk paths.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

const appName = "zorp"

// Config is the contents of ~/.config/zorp/config.toml.
type Config struct {
	Auth   Auth   `toml:"auth"`
	Sync   Sync   `toml:"sync"`
	Safety Safety `toml:"safety"`
	Keys   Keys   `toml:"keys"`
	UI     UI     `toml:"ui"`

	// Paths are resolved at load time, not read from the file.
	Paths Paths `toml:"-"`
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
type Safety struct {
	// SafeMode refuses deletes and only allows moves (including archive)
	// into ScratchFolder. Read/unread and flag are always allowed.
	SafeMode      bool   `toml:"safe_mode"`
	ScratchFolder string `toml:"scratch_folder"`
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
		Safety: Safety{SafeMode: true, ScratchFolder: "zorp-test"},
		Keys:   Keys{DeleteArchives: true},
		UI:     UI{Mouse: true},
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
	}, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

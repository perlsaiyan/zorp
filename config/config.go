// Package config loads zorp's TOML config and resolves its on-disk paths.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

const appName = "zorp"

// Config is the contents of ~/.config/zorp/config.toml.
type Config struct {
	Auth Auth `toml:"auth"`

	// Paths are resolved at load time, not read from the file.
	Paths Paths `toml:"-"`
}

type Auth struct {
	// ClientID is the Application (client) ID of the zorp app registration.
	ClientID string `toml:"client_id"`
	// Tenant is "organizations" (any work/school account), "common", or a tenant ID.
	Tenant string `toml:"tenant"`
}

type Paths struct {
	ConfigFile string
	DataDir    string
	TokenCache string
}

func defaults() Config {
	return Config{
		Auth: Auth{Tenant: "organizations"},
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
	return Paths{
		ConfigFile: filepath.Join(configHome, appName, "config.toml"),
		DataDir:    dataDir,
		TokenCache: filepath.Join(dataDir, "token_cache.json"),
	}, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

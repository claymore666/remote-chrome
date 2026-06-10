// Package config owns browserd's on-disk state directory and TOML config.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/BurntSushi/toml"
)

type Config struct {
	// Port for the extension WebSocket. 0 means: pick a free ephemeral port
	// on first run and persist it (the extension options need a stable port).
	Port int `toml:"port"`
	// Token shared with the extensions. Generated on first run.
	Token string `toml:"token"`
	// PinnedOrigins restricts which chrome-extension://<id> origins may
	// connect. Empty = any extension origin presenting the right token.
	PinnedOrigins []string `toml:"pinned_origins"`
	// DefaultProfile is targeted when a tool omits `profile`. Empty = the
	// sole connected profile (error if several are connected).
	DefaultProfile string `toml:"default_profile"`
	// DenyNavigation lists eTLD+1 domains navigation is always refused for,
	// regardless of grants.
	DenyNavigation []string `toml:"deny_navigation"`
	// Approval selects the dialog mechanism: "auto" (elicitation when the
	// client supports it, else native dialog), "elicit", "dialog".
	Approval string `toml:"approval"`
	// PermissionSet names the project permission set that "save to project
	// set" approvals are written to.
	PermissionSet string `toml:"permission_set"`
}

func defaults() *Config {
	return &Config{Approval: "auto", PermissionSet: "default"}
}

// Dir returns the state directory (~/.browserd or %LOCALAPPDATA%\browserd),
// honoring BROWSERD_DIR for tests.
func Dir() (string, error) {
	if d := os.Getenv("BROWSERD_DIR"); d != "" {
		return d, nil
	}
	if runtime.GOOS == "windows" {
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			return "", fmt.Errorf("%%LOCALAPPDATA%% not set")
		}
		return filepath.Join(base, "browserd"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".browserd"), nil
}

func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.toml"), nil
}

// Load reads the config, creating it (with a fresh token) on first run.
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	cfg := defaults()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if cfg.Token, err = NewToken(); err != nil {
			return nil, err
		}
		if err := cfg.Save(); err != nil {
			return nil, err
		}
		return cfg, nil
	}
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Token == "" {
		if cfg.Token, err = NewToken(); err != nil {
			return nil, err
		}
		if err := cfg.Save(); err != nil {
			return nil, err
		}
	}
	if cfg.Approval == "" {
		cfg.Approval = "auto"
	}
	if cfg.PermissionSet == "" {
		cfg.PermissionSet = "default"
	}
	return cfg, nil
}

// Save writes the config with owner-only permissions (it contains the token).
func (c *Config) Save() error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(c)
}

// NewToken returns a 256-bit hex token.
func NewToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

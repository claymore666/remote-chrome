package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFirstRunCreatesConfigWithToken(t *testing.T) {
	t.Setenv("REMOTE_CHROME_DIR", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Token) != 64 {
		t.Fatalf("expected 64-char hex token, got %q", cfg.Token)
	}
	if cfg.Approval != "auto" || cfg.PermissionSet != "default" {
		t.Fatalf("bad defaults: %+v", cfg)
	}
	path, _ := Path()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("config perms = %v, want 0600 (contains the token)", info.Mode().Perm())
	}
}

func TestLoadRoundtrip(t *testing.T) {
	t.Setenv("REMOTE_CHROME_DIR", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Port = 12345
	cfg.DefaultProfile = "work"
	cfg.DenyNavigation = []string{"evil.com"}
	cfg.PinnedOrigins = []string{"chrome-extension://abc"}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Port != 12345 || got.DefaultProfile != "work" ||
		len(got.DenyNavigation) != 1 || got.DenyNavigation[0] != "evil.com" ||
		len(got.PinnedOrigins) != 1 || got.Token != cfg.Token {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
}

func TestTokensAreUnique(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewToken()
	if a == b {
		t.Fatal("two tokens must differ")
	}
}

func TestDirEnvOverride(t *testing.T) {
	want := filepath.Join(t.TempDir(), "custom")
	t.Setenv("REMOTE_CHROME_DIR", want)
	got, err := Dir()
	if err != nil || got != want {
		t.Fatalf("Dir() = %q, %v", got, err)
	}
}

func TestMalformedConfigIsAnError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("REMOTE_CHROME_DIR", dir)
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte("port = \"not a number"), 0o600)
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

package hubclient

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPinnedConfigIgnoresEnvironmentAndPinsTokenLocation(t *testing.T) {
	t.Setenv("IRIS_HUB_URL", "https://evil.example")
	t.Setenv("IRIS_TOKEN_FILE", "/tmp/evil.token")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/evil-xdg")
	t.Setenv("HOME", "/tmp/evil-home")
	t.Setenv("USERPROFILE", `C:\evil-profile`)
	t.Setenv("APPDATA", `C:\evil-appdata`)

	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "hub.token")
	if err := os.WriteFile(tokenPath, []byte("real-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteConfig(filepath.Join(dir, "client.json"), FileConfig{Hub: "https://pinned.example", TokenFile: "hub.token"}); err != nil {
		t.Fatal(err)
	}

	r, gotDir, err := (PinnedConfig{ConfigDir: func() (string, error) { return dir, nil }}).Load()
	if err != nil || gotDir != dir {
		t.Fatalf("Load() = %v, %q, %v", r, gotDir, err)
	}
	if r.HubURL() != "https://pinned.example" {
		t.Fatalf("HubURL = %q", r.HubURL())
	}
	tok, err := r.Token()
	if err != nil || tok != "real-token" {
		t.Fatalf("Token = %q, %v", tok, err)
	}

	// Redirecting token_file outside <dir>/hub.token inside client.json fails closed.
	if err := WriteConfig(filepath.Join(dir, "client.json"), FileConfig{Hub: "https://pinned.example", TokenFile: "other.token"}); err != nil {
		t.Fatal(err)
	}
	r, _, err = (PinnedConfig{ConfigDir: func() (string, error) { return dir, nil }}).Load()
	if err == nil || !strings.Contains(err.Error(), "token_file must stay at") || r.HubURL() != "" {
		t.Fatalf("redirected token_file = %+v, %v", r, err)
	}

	// ConfigDir resolution failure fails closed.
	wantErr := errors.New("account lookup failed")
	r, gotDir, err = (PinnedConfig{ConfigDir: func() (string, error) { return "", wantErr }}).Load()
	if !errors.Is(err, wantErr) || gotDir != "" || r.HubURL() != "" || !errors.Is(r.ConfigErr(), wantErr) {
		t.Fatalf("ConfigDir error = %+v, %q, %v", r, gotDir, err)
	}
}

func TestNewPinnedConfigAndPinnedRuntimeEnvUseOSAccount(t *testing.T) {
	bogus := filepath.Join(t.TempDir(), "bogus-env")
	t.Setenv("HOME", bogus)
	t.Setenv("USERPROFILE", bogus)
	t.Setenv("XDG_CONFIG_HOME", bogus)
	t.Setenv("APPDATA", bogus)
	t.Setenv("IRIS_HUB_URL", "https://evil.example")
	t.Setenv("IRIS_TOKEN_FILE", filepath.Join(bogus, "hub.token"))

	dir, err := PinnedConfigDir()
	if err != nil || !filepath.IsAbs(dir) || strings.HasPrefix(dir, bogus) {
		t.Fatalf("PinnedConfigDir = %q, %v", dir, err)
	}
	env, err := PinnedRuntimeEnv()
	if err != nil || len(env) != 1 || strings.Contains(env[0], bogus) {
		t.Fatalf("PinnedRuntimeEnv = %v, %v", env, err)
	}
	// NewPinnedConfig exercises the default OS-account path without reading any caller env.
	r, gotDir, _ := NewPinnedConfig()
	if gotDir != dir || r.Source == "environment (IRIS_HUB_URL)" {
		t.Fatalf("NewPinnedConfig = %+v, %q", r, gotDir)
	}
}

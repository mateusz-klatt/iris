package hubclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigPrecedence(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "hub.token")
	env := map[string]string{}
	reads := 0
	e := EnvConfig{DefaultTokenFile: token, Getenv: func(k string) string { return env[k] }, ReadFile: func(p string) ([]byte, error) { reads++; return os.ReadFile(p) }}
	r := e.Load()
	if r.HubURL() != DefaultHubURL || r.Source != "default" {
		t.Fatal(r)
	}
	if err := WriteConfig(filepath.Join(dir, "client.json"), FileConfig{Hub: "https://file.example", TokenFile: "other.token"}); err != nil {
		t.Fatal(err)
	}
	reads = 0
	r = e.Load()
	if r.HubURL() != "https://file.example" || !strings.HasPrefix(r.Source, "file (") || r.TokenFile != filepath.Join(dir, "other.token") {
		t.Fatal(r)
	}
	r.HubURL()
	_, _ = r.TokenPath()
	r.HubURL()
	if reads != 1 {
		t.Fatal("configuration reread", reads)
	}
	env["IRIS_HUB_URL"] = "https://env.example"
	r = e.Load()
	if r.HubURL() != "https://env.example" || r.TokenFile != filepath.Join(dir, "other.token") {
		t.Fatal("URL override changed file token", r)
	}
	env["IRIS_TOKEN_FILE"] = token
	r = e.Load()
	if r.HubURL() != "https://env.example" || r.Source != "environment (IRIS_HUB_URL)" || r.TokenFile != token {
		t.Fatal(r)
	}
	if err := os.WriteFile(filepath.Join(dir, "client.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	loaded := e.Load()
	path, err := loaded.TokenPath()
	if err != nil || path != token {
		t.Fatalf("failure log lost its token path: %q %v", path, err)
	}
	if _, err = loaded.Token(); err == nil || loaded.HubURL() != "" {
		t.Fatal("malformed config silently ignored")
	}
}

func TestConfigErrReportsWhatTokenPathTolerates(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "hub.token")
	if err := os.WriteFile(filepath.Join(dir, "client.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := (EnvConfig{Getenv: func(string) string { return "" }, DefaultTokenFile: token}).Load()
	if got, err := r.TokenPath(); err != nil || got != token {
		t.Fatal(got, err)
	}
	if r.ConfigErr() == nil {
		t.Fatal("a malformed client.json was not reported")
	}
}

func TestBaseAndEnvTokenPathSkipClientConfig(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "hub.token")
	explicit := filepath.Join(dir, "explicit.token")
	noRead := func(path string) ([]byte, error) {
		t.Fatalf("unexpected read of %s", path)
		return nil, nil
	}
	withEnv := EnvConfig{
		DefaultTokenFile: token,
		Getenv: func(k string) string {
			if k == "IRIS_TOKEN_FILE" {
				return explicit
			}
			return ""
		},
		ReadFile: noRead,
	}
	if got, fromEnv, err := withEnv.BaseTokenPath(); err != nil || !fromEnv || got != explicit {
		t.Fatalf("BaseTokenPath() = (%q, %v, %v), want (%q, true, nil)", got, fromEnv, err, explicit)
	}
	if got, err := withEnv.TokenPath(); err != nil || got != explicit {
		t.Fatalf("TokenPath() = (%q, %v), want (%q, nil)", got, err, explicit)
	}
	withoutEnv := EnvConfig{
		DefaultTokenFile: token,
		Getenv:           func(string) string { return "" },
		ReadFile:         noRead,
	}
	if got, fromEnv, err := withoutEnv.BaseTokenPath(); err != nil || fromEnv || got != token {
		t.Fatalf("BaseTokenPath() = (%q, %v, %v), want (%q, false, nil)", got, fromEnv, err, token)
	}
	if err := WriteConfig(filepath.Join(dir, "client.json"), FileConfig{Hub: "https://file.example", TokenFile: "other.token"}); err != nil {
		t.Fatal(err)
	}
	withFile := EnvConfig{
		DefaultTokenFile: token,
		Getenv:           func(string) string { return "" },
		ReadFile:         os.ReadFile,
	}
	if got, err := withFile.TokenPath(); err != nil || got != filepath.Join(dir, "other.token") {
		t.Fatalf("TokenPath() with client.json = (%q, %v)", got, err)
	}
	if got, err := withEnv.BoundedTokenPath(15*time.Millisecond, nil); err != nil || got != explicit {
		t.Fatalf("BoundedTokenPath(env) = (%q, %v), want (%q, nil)", got, err, explicit)
	}
	if got, err := withFile.BoundedTokenPath(time.Second, nil); err != nil || got != filepath.Join(dir, "other.token") {
		t.Fatalf("BoundedTokenPath(file) = (%q, %v)", got, err)
	}
	entered := make(chan struct{})
	unblock := make(chan struct{})
	done := make(chan struct{})
	defer func() {
		close(unblock)
		<-done
	}()
	stalled := EnvConfig{
		DefaultTokenFile: token,
		Getenv:           func(string) string { return "" },
		ReadFile: func(string) ([]byte, error) {
			defer close(done)
			close(entered)
			<-unblock
			return nil, os.ErrNotExist
		},
	}
	got, err := stalled.BoundedTokenPath(15*time.Millisecond, func(time.Duration) <-chan time.Time {
		c := make(chan time.Time)
		go func() {
			<-entered
			close(c)
		}()
		return c
	})
	if err != nil || got != token {
		t.Fatalf("BoundedTokenPath(stalled) = (%q, %v), want (%q, nil)", got, err, token)
	}
}

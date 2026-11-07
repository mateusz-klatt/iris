package hubclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

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
	r.TokenPath()
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
	if _, err := e.Load().TokenPath(); err == nil {
		t.Fatal("malformed config silently ignored")
	}
}

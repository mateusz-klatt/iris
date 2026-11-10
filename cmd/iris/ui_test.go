package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/hubclient"
)

func TestConsoleURLAcceptsOnlyOrdinaryConfiguredOrigins(t *testing.T) {
	for input, want := range map[string]string{
		"https://iris.example":       "https://iris.example/",
		"https://iris.example/":      "https://iris.example/",
		"http://127.0.0.1:7878":      "http://127.0.0.1:7878/",
		"http://[::1]:7878/":         "http://[::1]:7878/",
		"https://iris.example:8443/": "https://iris.example:8443/",
	} {
		got, err := consoleURL(hubclient.Resolved{URL: input})
		if err != nil || got != want {
			t.Fatalf("console URL %q = %q, want %q: %v", input, got, want, err)
		}
	}
	for _, input := range []string{
		"%", "", "https://:443", "https:opaque", "http://localhost:7878",
		"http://public.example", "ftp://iris.example", "https://private:secret@iris.example",
		"https://iris.example/other", "https://iris.example/%2F", "https://iris.example/?secret",
		"https://iris.example/?", "https://iris.example/#secret", "https://iris.example/#",
	} {
		got, err := consoleURL(hubclient.Resolved{URL: input})
		if got != "" || err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid or secret-bearing console origin was exposed: %q %v", got, err)
		}
	}
}

func TestConsoleURLReadsConfigurationWithoutReadingMachineCredentials(t *testing.T) {
	const configPath = "/configuration/client.json"
	var reads int
	cfg := (hubclient.EnvConfig{
		DefaultTokenFile: "/configuration/hub.token",
		Getenv:           func(string) string { return "" },
		ReadFile: func(path string) ([]byte, error) {
			reads++
			if path != filepath.FromSlash(configPath) {
				t.Fatal("console URL attempted to read a credential", path)
			}
			return []byte(`{"hub_url":"https://iris.example","token_file":"private-token"}`), nil
		},
	}).Load()
	got, err := consoleURL(cfg)
	if err != nil || got != "https://iris.example/" || reads != 1 {
		t.Fatal("console URL did not resolve the config without a token", err)
	}
}

func TestUIReportsConfigurationAndOriginErrorsBeforeOpeningBrowser(t *testing.T) {
	token := filepath.Join(t.TempDir(), "hub.token")
	t.Setenv("IRIS_TOKEN_FILE", token)
	t.Setenv("IRIS_HUB_URL", "https://iris.example")
	t.Setenv("PATH", t.TempDir())
	configPath := filepath.Join(filepath.Dir(token), "client.json")
	if err := os.WriteFile(configPath, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runUI(nil); err == nil || !strings.Contains(err.Error(), "client.json") {
		t.Fatal("malformed client config was ignored", err)
	}
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	t.Setenv("IRIS_HUB_URL", "https://private:secret@iris.example/?secret")
	if err := runUI(nil); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("invalid origin reached the browser or exposed credentials", err)
	}

	privateError := errors.New("configuration unreadable")
	cfg := (hubclient.EnvConfig{
		DefaultTokenFile: token,
		Getenv:           func(string) string { return "" },
		ReadFile:         func(string) ([]byte, error) { return nil, privateError },
	}).Load()
	if got, err := consoleURL(cfg); got != "" || !errors.Is(err, privateError) {
		t.Fatal("configuration failure was replaced by a default console URL", err)
	}
}

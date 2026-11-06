// Package hubclient is the HTTP client for iris serve.
// hub_url and the token file are read through Config so internal/config can
// replace the environment fallback without changing callers.
package hubclient

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// DefaultHubURL is the loopback hub from D1.
const DefaultHubURL = "http://127.0.0.1:7878"

// Config supplies the hub address and bearer token.
type Config interface {
	HubURL() string
	Token() (string, error)
}

// EnvConfig reads IRIS_HUB_URL and IRIS_TOKEN_FILE.
// An empty token file means the process has no credential yet.
type EnvConfig struct {
	Getenv   func(string) string
	ReadFile func(string) ([]byte, error)
}

func (e EnvConfig) getenv(key string) string {
	if e.Getenv != nil {
		return e.Getenv(key)
	}
	return os.Getenv(key)
}

func (e EnvConfig) readFile(path string) ([]byte, error) {
	if e.ReadFile != nil {
		return e.ReadFile(path)
	}
	return os.ReadFile(path)
}

func (e EnvConfig) HubURL() string {
	if v := strings.TrimSpace(e.getenv("IRIS_HUB_URL")); v != "" {
		return v
	}
	return DefaultHubURL
}

func (e EnvConfig) Token() (string, error) {
	path := strings.TrimSpace(e.getenv("IRIS_TOKEN_FILE"))
	if path == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		raw, err := e.readFile(filepath.Join(dir, "iris", "hub.token"))
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(raw)), nil
	}
	b, err := e.readFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

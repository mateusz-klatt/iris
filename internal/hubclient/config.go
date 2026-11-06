// Package hubclient is the HTTP client for iris serve.
package hubclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const DefaultHubURL = "http://127.0.0.1:7878"

type Config interface {
	HubURL() string
	Token() (string, error)
}

// FileConfig is stored in client.json beside the discovery token location.
// Relative token paths are resolved relative to client.json.
type FileConfig struct {
	Hub       string `json:"hub_url"`
	TokenFile string `json:"token_file"`
}

type EnvConfig struct {
	Getenv           func(string) string
	ReadFile         func(string) ([]byte, error)
	DefaultTokenFile string // optional platform/layout override
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
func (e EnvConfig) defaultTokenPath() (string, error) {
	if e.DefaultTokenFile != "" {
		return e.DefaultTokenFile, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "iris", "hub.token"), nil
}

// Resolved captures configuration once per process; hooks read one small config file.
type Resolved struct {
	URL, TokenFile, Source, ConfigFile string
	err                                error
	readFile                           func(string) ([]byte, error)
	explicitToken                      bool
}

func NewConfig() Resolved { return (EnvConfig{}).Load() }
func (e EnvConfig) Load() Resolved {
	r := Resolved{URL: DefaultHubURL, Source: "default", readFile: e.readFile}
	r.TokenFile, r.err = e.defaultTokenPath()
	if r.err != nil {
		return r
	}
	envToken := strings.TrimSpace(e.getenv("IRIS_TOKEN_FILE"))
	if envToken != "" {
		r.TokenFile = envToken
	}
	r.ConfigFile = filepath.Join(filepath.Dir(r.TokenFile), "client.json")
	raw, err := e.readFile(r.ConfigFile)
	if err == nil {
		var f FileConfig
		if len(raw) > 64<<10 {
			err = errors.New("client config exceeds 64 KiB")
		} else {
			err = json.Unmarshal(raw, &f)
		}
		if err != nil {
			r.err = fmt.Errorf("read %s: %w", r.ConfigFile, err)
			return r
		}
		if v := strings.TrimSpace(f.Hub); v != "" {
			r.URL = v
			r.Source = "file (" + r.ConfigFile + ")"
		}
		if v := strings.TrimSpace(f.TokenFile); v != "" {
			if !filepath.IsAbs(v) {
				v = filepath.Join(filepath.Dir(r.ConfigFile), v)
			}
			r.TokenFile = v
			r.explicitToken = true
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		r.err = err
		return r
	}
	if v := strings.TrimSpace(e.getenv("IRIS_HUB_URL")); v != "" {
		r.URL = v
		r.Source = "environment (IRIS_HUB_URL)"
	}
	if envToken != "" {
		r.TokenFile = envToken
		r.explicitToken = true
	}
	return r
}
func (r Resolved) HubURL() string {
	if r.err != nil {
		return ""
	}
	return r.URL
}
func (r Resolved) TokenPath() (string, error) {
	// A malformed client.json must not hide the credential path. Failure logs
	// are written beside it, and a failing hook still has to leave a trace.
	// HubURL and Token keep the parse error, so the hub itself stays fail-closed.
	if r.TokenFile != "" {
		return r.TokenFile, nil
	}
	return "", r.err
}
func (r Resolved) Token() (string, error) {
	if r.err != nil {
		return "", r.err
	}
	raw, err := r.readFile(r.TokenFile)
	if errors.Is(err, os.ErrNotExist) && !r.explicitToken {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}
func (e EnvConfig) HubURL() string             { return e.Load().HubURL() }
func (e EnvConfig) TokenPath() (string, error) { return e.Load().TokenPath() }
func (e EnvConfig) Token() (string, error)     { return e.Load().Token() }

// Hooks a test uses to force a write or close failure on the temp file.
var (
	prepareConfigWrite = func(*os.File) {}
	prepareConfigClose = func(*os.File) {}
)

// WriteConfig persists client settings without fetching or creating a credential.
func WriteConfig(path string, f FileConfig) error {
	if err := ValidateURL(f.Hub); err != nil {
		return err
	}
	// FileConfig is only strings, so the encoding cannot fail.
	raw, _ := json.MarshalIndent(f, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".client-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	prepareConfigWrite(temp)
	if _, err = temp.Write(append(raw, '\n')); err != nil {
		temp.Close()
		return err
	}
	prepareConfigClose(temp)
	if err = temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

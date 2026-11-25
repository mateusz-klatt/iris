// Package hubclient is the HTTP client for iris serve.
package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	DefaultHubURL          = "http://127.0.0.1:7878"
	maxConfigBytes         = 64 << 10
	configSuccessionBudget = 25 * time.Millisecond
	configCASAttempts      = 3
)

// ErrConfigChanged reports that client.json changed concurrently on every CAS attempt.
var ErrConfigChanged = errors.New("client config changed concurrently")

type Config interface {
	HubURL() string
	Token() (string, error)
}

// FileConfig is stored in client.json beside the discovery token location.
// Relative token paths are resolved relative to client.json.
type FileConfig struct {
	Hub               string `json:"hub_url"`
	TokenFile         string `json:"token_file"`
	SessionSuccession *bool  `json:"session_succession,omitempty"`
}

type fileStrings struct {
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
	configReadFile                     func(string) ([]byte, error)
	explicitToken                      bool
}

func NewConfig() Resolved { return (EnvConfig{}).Load() }
func (e EnvConfig) Load() Resolved {
	r := Resolved{URL: DefaultHubURL, Source: "default", readFile: e.readFile, configReadFile: e.ReadFile}
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
		var fs fileStrings
		if len(raw) > maxConfigBytes {
			err = errors.New("client config exceeds 64 KiB")
		} else {
			err = json.Unmarshal(raw, &fs)
		}
		if err != nil {
			r.err = fmt.Errorf("read %s: %w", r.ConfigFile, err)
			return r
		}
		if v := strings.TrimSpace(fs.Hub); v != "" {
			r.URL = v
			r.Source = "file (" + r.ConfigFile + ")"
		}
		if v := strings.TrimSpace(fs.TokenFile); v != "" {
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

// ConfigErr reports what TokenPath deliberately tolerates: a setting that could
// not be read. Commands that act on the configuration must check it.
func (r Resolved) ConfigErr() error { return r.err }

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
func (e EnvConfig) HubURL() string { return e.Load().HubURL() }

// BaseTokenPath returns the token path from IRIS_TOKEN_FILE or the platform
// configuration directory without reading client.json. When fromEnv is true,
// IRIS_TOKEN_FILE set the path explicitly and client.json cannot override it.
func (e EnvConfig) BaseTokenPath() (path string, fromEnv bool, err error) {
	if v := strings.TrimSpace(e.getenv("IRIS_TOKEN_FILE")); v != "" {
		return v, true, nil
	}
	path, err = e.defaultTokenPath()
	return path, false, err
}

func (e EnvConfig) TokenPath() (string, error) {
	if path, fromEnv, _ := e.BaseTokenPath(); fromEnv {
		return path, nil
	}
	return e.Load().TokenPath()
}

// BoundedTokenPath resolves the token path without blocking indefinitely on client.json.
// When IRIS_TOKEN_FILE is set, client.json is not read. Otherwise client.json is read
// within budget; if that stalls, the default token path is returned.
func (e EnvConfig) BoundedTokenPath(budget time.Duration, after func(time.Duration) <-chan time.Time) (string, error) {
	fallback, fromEnv, err := e.BaseTokenPath()
	if err != nil || fromEnv {
		return fallback, err
	}
	ch := make(chan string, 1)
	go func() {
		ch <- (EnvConfig{
			Getenv:           func(string) string { return "" },
			ReadFile:         e.ReadFile,
			DefaultTokenFile: fallback,
		}).Load().TokenFile
	}()
	if after == nil {
		after = time.After
	}
	select {
	case path := <-ch:
		return path, nil
	case <-after(budget):
		return fallback, nil
	}
}

func (e EnvConfig) Token() (string, error) { return e.Load().Token() }

// SuccessionEnabled reloads client.json within a bounded deadline and returns whether
// session_succession is explicitly enabled, failing OFF on missing, bad, or stalled configs.
func (r Resolved) SuccessionEnabled() bool {
	return r.SuccessionEnabledContext(context.Background())
}

// SuccessionEnabledContext reloads client.json bounded by ctx (or default budget if ctx has
// no deadline), failing OFF without altering HubURL or Token settings.
func (r Resolved) SuccessionEnabledContext(ctx context.Context) bool {
	return readSessionSuccession(ctx, r.ConfigFile, r.configReadFile)
}

// SuccessionEnabled reloads client.json relative to BaseTokenPath without parsing HubURL/Token,
// defaulting and failing OFF on missing, malformed, non-regular, or stalled configs.
func (e EnvConfig) SuccessionEnabled() bool {
	return e.SuccessionEnabledContext(context.Background())
}

// SuccessionEnabledContext reloads client.json relative to BaseTokenPath within ctx.
func (e EnvConfig) SuccessionEnabledContext(ctx context.Context) bool {
	base, _, err := e.BaseTokenPath()
	if err != nil {
		return false
	}
	return readSessionSuccession(ctx, filepath.Join(filepath.Dir(base), "client.json"), e.ReadFile)
}

// Hooks tests use to force temp-file errors or simulate concurrent config writes cleanly.
var (
	prepareConfigWrite = func(*os.File) {}
	prepareConfigClose = func(*os.File) {}
	createConfigTemp   = os.CreateTemp
	beforeConfigRename = func(string) {}
	afterConfigCompare = func(string) {}
	configStat         = statRegularConfig
	configOpen         = openRegularConfig
	configFileLock     = lockConfigFile
	afterConfigStat    = func() {}
	afterConfigRead    = func(string) {}
)

func boolPtr(v bool) *bool { return &v }

func readBoundedRegularConfig(path string) ([]byte, error) {
	raw, _, err := readBoundedRegularConfigWithID(path)
	return raw, err
}

func readBoundedRegularConfigWithID(path string) ([]byte, configFileID, error) {
	info, expected, err := configStat(path)
	if err != nil {
		return nil, configFileID{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxConfigBytes {
		return nil, configFileID{}, errors.New("client config is not a bounded regular file")
	}
	f, actual, err := configOpen(path, expected)
	if err != nil {
		return nil, configFileID{}, err
	}
	defer func() { _ = f.Close() }()
	finfo, err := f.Stat()
	if err != nil {
		return nil, configFileID{}, err
	}
	if !finfo.Mode().IsRegular() || finfo.Size() > maxConfigBytes {
		return nil, configFileID{}, errors.New("client config is not a bounded regular file")
	}
	afterConfigStat()
	raw, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return nil, configFileID{}, err
	}
	if len(raw) > maxConfigBytes {
		return nil, configFileID{}, errors.New("client config exceeds 64 KiB")
	}
	return raw, actual, nil
}

func parseSessionSuccession(raw []byte) (*bool, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, false
	}
	v, ok := fields["session_succession"]
	if !ok {
		return nil, true
	}
	trimmed := bytes.TrimSpace(v)
	switch {
	case bytes.Equal(trimmed, []byte("true")):
		return boolPtr(true), true
	case bytes.Equal(trimmed, []byte("false")):
		return boolPtr(false), true
	default:
		return nil, false
	}
}

type configSnapshot struct {
	absent bool
	valid  bool
	id     configFileID
	raw    []byte
	cur    *bool
}

func readConfigSnapshot(path string) configSnapshot {
	raw, id, err := readBoundedRegularConfigWithID(path)
	if errors.Is(err, os.ErrNotExist) {
		return configSnapshot{absent: true}
	}
	if err != nil {
		return configSnapshot{}
	}
	val, _ := parseSessionSuccession(raw)
	return configSnapshot{valid: true, id: id, raw: raw, cur: val}
}

func sameConfigSnapshot(a, b configSnapshot) bool {
	if a.absent != b.absent {
		return false
	}
	if a.absent {
		return true
	}
	if a.valid != b.valid || !bytes.Equal(a.raw, b.raw) {
		return false
	}
	return !a.valid || sameConfigFileID(a.id, b.id)
}

func lockConfig(path string) (func(), error) {
	f, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := configFileLock(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		unlockConfigFile(f)
		_ = f.Close()
	}, nil
}

func readSessionSuccession(ctx context.Context, path string, customRead func(string) ([]byte, error)) bool {
	if path == "" || ctx == nil || ctx.Err() != nil {
		return false
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, configSuccessionBudget)
		defer cancel()
	}
	ch := make(chan bool, 1)
	go func() {
		var raw []byte
		var err error
		if customRead != nil {
			raw, err = customRead(path)
		} else {
			raw, err = readBoundedRegularConfig(path)
		}
		if err != nil || len(raw) > maxConfigBytes {
			ch <- false
			return
		}
		val, ok := parseSessionSuccession(raw)
		ch <- ok && val != nil && *val
	}()
	select {
	case enabled := <-ch:
		return enabled
	case <-ctx.Done():
		return false
	}
}

func stageConfigTemp(path string, f FileConfig) (string, error) {
	// FileConfig contains only strings and a *bool, so JSON encoding cannot fail.
	raw, _ := json.MarshalIndent(f, "", "  ")
	temp, err := createConfigTemp(filepath.Dir(path), ".client-*")
	if err != nil {
		return "", err
	}
	name := temp.Name()
	prepareConfigWrite(temp)
	if _, err = temp.Write(append(raw, '\n')); err != nil {
		_ = temp.Close()
		_ = os.Remove(name)
		return "", err
	}
	prepareConfigClose(temp)
	if err = temp.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

func renameConfigTemp(name, path string) error {
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// WriteConfig persists client settings without fetching or creating a credential.
func WriteConfig(path string, f FileConfig) error {
	if err := ValidateURL(f.Hub); err != nil {
		return err
	}
	if err := privateDir(filepath.Dir(path)); err != nil {
		return err
	}
	unlock, err := lockConfig(path)
	if err != nil {
		return err
	}
	defer unlock()
	name, err := stageConfigTemp(path, f)
	if err != nil {
		return err
	}
	return renameConfigTemp(name, path)
}

// UpdateConfig performs a lock-serialized compare-and-swap update of Hub and TokenFile while
// preserving whatever valid session_succession is on disk immediately before rename. When
// SessionSuccession is nil, setup takes it from the on-disk re-read rather than setting it. If the
// on-disk config changes before rename (e.g., an external atomic replacement with false), it
// retries against the new disk state or aborts with ErrConfigChanged without writing stale true.
func UpdateConfig(path string, f FileConfig) error {
	if err := ValidateURL(f.Hub); err != nil {
		return err
	}
	if err := privateDir(filepath.Dir(path)); err != nil {
		return err
	}
	unlock, err := lockConfig(path)
	if err != nil {
		return err
	}
	defer unlock()
	for range configCASAttempts {
		before := readConfigSnapshot(path)
		afterConfigRead(path)
		out := f
		if out.SessionSuccession == nil {
			out.SessionSuccession = before.cur
		}
		name, err := stageConfigTemp(path, out)
		if err != nil {
			return err
		}
		beforeConfigRename(path)
		after := readConfigSnapshot(path)
		if !sameConfigSnapshot(before, after) {
			_ = os.Remove(name)
			continue
		}
		afterConfigCompare(path)
		latest := readConfigSnapshot(path)
		if !sameConfigSnapshot(after, latest) {
			_ = os.Remove(name)
			continue
		}
		return renameConfigTemp(name, path)
	}
	return ErrConfigChanged
}

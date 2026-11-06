// Package integrate writes user-level client configuration for Iris.
// It never writes project files and never grants trust.
package integrate

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	Claude      = "claude"
	Codex       = "codex"
	Copilot     = "copilot"
	Cursor      = "cursor"
	Antigravity = "antigravity"
	Grok        = "grok"
	Kimi        = "kimi"
)

// Names is the stable client order.
var Names = []string{Claude, Codex, Copilot, Cursor, Antigravity, Grok, Kimi}

// ErrBroken means an existing file cannot be edited safely.
var ErrBroken = errors.New("refusing to overwrite a broken file")

// ErrNotConfigured means Iris has not written this client's files.
var ErrNotConfigured = errors.New("configuration is absent")

// Plugins installs the Claude plugin. Tests supply a fake.
type Plugins interface {
	MarketplaceAdd(dir string) error
	Install(id string) error
	Uninstall(id string) error
	MarketplaceRemove(name string) error
}

// Client is one agent integration.
type Client interface {
	Name() string
	Files(Layout) ([]string, error)
	Install(Layout, Plugins) error
	Remove(Layout, Plugins) error
	Canary(Layout) (Invocation, error)
	Human() string
}

// Layout is the home, state directory and command lookup for one run.
// A nil Getenv or LookPath uses the process environment. Tests set both
// so a run cannot touch the real home.
type Layout struct {
	Home     string
	StateDir string
	Getenv   func(string) string
	LookPath func(string) (string, error)
	GOOS     string
}

// Invocation is the hook command read back from a client file.
type Invocation struct {
	Argv  []string
	Shell string
}

var registry []Client

func register(c Client) { registry = append(registry, c) }

// All returns clients in Names order.
func All() []Client {
	by := map[string]Client{}
	for _, c := range registry {
		by[c.Name()] = c
	}
	out := make([]Client, 0, len(Names))
	for _, name := range Names {
		if c, ok := by[name]; ok {
			out = append(out, c)
		}
	}
	return out
}

// ByName accepts the canonical name and the aliases the hook already accepts.
func ByName(name string) (Client, bool) {
	want := Canonical(name)
	if want == "" {
		return nil, false
	}
	for _, c := range All() {
		if c.Name() == want {
			return c, true
		}
	}
	return nil, false
}

// Canonical maps aliases onto the seven client names.
func Canonical(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "claude", "claude-code":
		return Claude
	case "codex":
		return Codex
	case "copilot":
		return Copilot
	case "cursor", "cursor-agent":
		return Cursor
	case "antigravity", "agy":
		return Antigravity
	case "grok":
		return Grok
	case "kimi", "kimi-code":
		return Kimi
	default:
		return ""
	}
}

// Binaries is the executable tried for each client, first match wins.
var Binaries = map[string][]string{
	Claude:      {"claude"},
	Codex:       {"codex"},
	Copilot:     {"copilot"},
	Cursor:      {"cursor-agent", "agent"},
	Antigravity: {"agy"},
	Grok:        {"grok"},
	Kimi:        {"kimi"},
}

// Detect returns clients whose binary LookPath can see.
func Detect(look func(string) (string, error)) []string {
	if look == nil {
		look = exec.LookPath
	}
	var found []string
	for _, name := range Names {
		for _, bin := range Binaries[name] {
			if p, err := look(bin); err == nil && p != "" {
				found = append(found, name)
				break
			}
		}
	}
	return found
}

// Binary resolves one client executable.
func (l Layout) Binary(client string) (string, error) {
	look := l.LookPath
	if look == nil {
		look = exec.LookPath
	}
	var last error
	for _, bin := range Binaries[client] {
		p, err := look(bin)
		if err == nil && p != "" {
			return p, nil
		}
		last = err
	}
	if last == nil {
		last = errors.New("not found")
	}
	return "", last
}

func (l Layout) getenv(key string) string {
	if l.Getenv != nil {
		return l.Getenv(key)
	}
	return os.Getenv(key)
}

// GetenvOr reads one variable. An empty string means unset.
func (l Layout) GetenvOr(key string) string { return l.getenv(key) }

// OS is the target operating system, defaulting to the process.
func (l Layout) OS() string { return l.goos() }

// HomeDir is the home directory used for user-level files.
func (l Layout) HomeDir() (string, error) { return l.home() }

func (l Layout) goos() string {
	if l.GOOS != "" {
		return l.GOOS
	}
	return runtime.GOOS
}

func (l Layout) home() (string, error) {
	if l.Home != "" {
		return l.Home, nil
	}
	if h := l.getenv("HOME"); h != "" {
		return h, nil
	}
	if h := l.getenv("USERPROFILE"); h != "" {
		return h, nil
	}
	return os.UserHomeDir()
}

func (l Layout) state() (string, error) {
	if l.StateDir != "" {
		return l.StateDir, nil
	}
	if v := l.getenv("IRIS_STATE_DIR"); v != "" {
		return v, nil
	}
	if v := l.getenv("XDG_STATE_HOME"); v != "" && l.goos() != "windows" {
		return filepath.Join(v, "iris"), nil
	}
	home, err := l.home()
	if err != nil {
		return "", err
	}
	if l.goos() == "windows" {
		if v := l.getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "iris"), nil
		}
		return filepath.Join(home, "AppData", "Local", "iris"), nil
	}
	return filepath.Join(home, ".local", "state", "iris"), nil
}

func (l Layout) underHome(elem ...string) (string, error) {
	home, err := l.home()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{home}, elem...)...), nil
}

func (l Layout) envDir(key, fallback string) (string, error) {
	if v := l.getenv(key); v != "" {
		return v, nil
	}
	return l.underHome(fallback)
}

// Iris is the stable path from LookPath. Symlinks are not resolved, so a
// versioned target behind a PATH entry is not what clients are told to run.
func (l Layout) Iris() string {
	look := l.LookPath
	if look == nil {
		look = exec.LookPath
	}
	p, err := look("iris")
	if err != nil || strings.TrimSpace(p) == "" {
		if l.goos() == "windows" {
			return "iris.exe"
		}
		return "iris"
	}
	return p
}

func hookShell(iris, client, event string) string {
	return shellQuote(iris) + " hook --client " + client + " --event " + event
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n\"'\\$`!;|&<>(){}[]*?#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

type manifest struct {
	Files map[string]manifestFile `json:"files"`
}

type manifestFile struct {
	Client  string `json:"client"`
	Created bool   `json:"created"`
	Backup  string `json:"backup,omitempty"`
}

func (l Layout) manifestPath() (string, error) {
	dir, err := l.state()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "integrate", "manifest.json"), nil
}

func (l Layout) loadManifest() (manifest, error) {
	path, err := l.manifestPath()
	if err != nil {
		return manifest{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return manifest{Files: map[string]manifestFile{}}, nil
	}
	if err != nil {
		return manifest{}, err
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return manifest{}, fmt.Errorf("iris integrate manifest is unreadable: %w", err)
	}
	if m.Files == nil {
		m.Files = map[string]manifestFile{}
	}
	return m, nil
}

func (l Layout) saveManifest(m manifest) error {
	if m.Files == nil {
		m.Files = map[string]manifestFile{}
	}
	path, err := l.manifestPath()
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return atomicWrite(path, raw)
}

// update validates, backs up the original once, then writes next.
// render must refuse a broken existing file by returning ErrBroken.
func (l Layout) update(client, path string, render func(existing []byte) ([]byte, error)) error {
	if err := refuseSpecial(path); err != nil {
		return err
	}
	existing, err := os.ReadFile(path)
	absent := errors.Is(err, os.ErrNotExist)
	if err != nil && !absent {
		return err
	}
	if absent {
		existing = nil
	}
	next, err := render(existing)
	if err != nil {
		return err
	}
	if !absent && string(existing) == string(next) {
		return nil
	}
	if err := l.remember(client, path, existing, absent); err != nil {
		return err
	}
	return atomicWrite(path, next)
}

func (l Layout) remember(client, path string, existing []byte, absent bool) error {
	m, err := l.loadManifest()
	if err != nil {
		return err
	}
	if _, ok := m.Files[path]; ok {
		return nil
	}
	rec := manifestFile{Client: client, Created: absent}
	if !absent {
		dir, err := l.state()
		if err != nil {
			return err
		}
		name := base64.RawURLEncoding.EncodeToString([]byte(path))
		rel := filepath.Join("integrate", "backup", name)
		if err := atomicWrite(filepath.Join(dir, rel), existing); err != nil {
			return err
		}
		rec.Backup = rel
	}
	m.Files[path] = rec
	return l.saveManifest(m)
}

func (l Layout) restore(client string) error {
	m, err := l.loadManifest()
	if err != nil {
		return err
	}
	dir, err := l.state()
	if err != nil {
		return err
	}
	for path, rec := range m.Files {
		if rec.Client != client {
			continue
		}
		if rec.Created {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		} else if rec.Backup != "" {
			data, err := os.ReadFile(filepath.Join(dir, rec.Backup))
			if err != nil {
				return err
			}
			if err := atomicWrite(path, data); err != nil {
				return err
			}
			_ = os.Remove(filepath.Join(dir, rec.Backup))
		}
		delete(m.Files, path)
	}
	return l.saveManifest(m)
}

func refuseSpecial(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrBroken, path)
	}
	return nil
}

func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil && info.Mode().Perm() != 0 {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".iris-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		if runtime.GOOS == "windows" && os.Remove(path) == nil {
			if err = os.Rename(tmpName, path); err == nil {
				cleanup = false
				return nil
			}
		}
		return err
	}
	cleanup = false
	return nil
}

func marshalJSON(v any) ([]byte, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func readObject(existing []byte) (map[string]any, error) {
	if len(bytesTrim(existing)) == 0 {
		return map[string]any{}, nil
	}
	if !json.Valid(existing) {
		return nil, ErrBroken
	}
	var doc map[string]any
	if err := json.Unmarshal(existing, &doc); err != nil || doc == nil {
		return nil, ErrBroken
	}
	return doc, nil
}

func bytesTrim(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

func timeoutFor(event string) int {
	switch strings.ToLower(event) {
	case "sessionend", "session_end", "session-end":
		return 1
	default:
		return 3
	}
}

// sessionEndCapped reports clients whose session-end budget is under 3s.
func sessionEndCapped(client, event string) bool {
	if timeoutFor(event) != 1 {
		return false
	}
	switch client {
	case Claude, Codex, Grok:
		return true
	default:
		return false
	}
}

func eventTimeout(client, event string) int {
	if sessionEndCapped(client, event) {
		return 1
	}
	return 3
}

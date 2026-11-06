// Package doctor reports whether each client is actually hooked up.
package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/integrate"
)

const (
	StatusOK            = "ok"
	StatusNotConfigured = "not_configured"
	StatusNeedsHuman    = "needs_human"
	StatusSilent        = "silent"
)

// Report is the body stored with PUT /v1/ui/doctor.
type Report struct {
	CheckedAt time.Time      `json:"checked_at"`
	Clients   []ClientReport `json:"clients"`
}

// ClientReport is one client's doctor row.
type ClientReport struct {
	Client     string `json:"client"`
	Version    string `json:"version,omitempty"`
	Status     string `json:"status"`
	Detail     string `json:"detail,omitempty"`
	LastHookAt string `json:"last_hook_at,omitempty"`
}

// Result is the human-readable outcome plus the stored report.
type Result struct {
	HubURL    string
	HubSource string
	Report    Report
	HubUp     bool
	TokenOK   bool
	// TokenUnchecked means the hub never judged the token, as opposed to rejecting it.
	TokenUnchecked bool
	HubDetail      string
	Stored         bool
	StoreError     string
}

// Options configures one doctor run. Live is opt-in because it calls a model.
type Options struct {
	Layout    integrate.Layout
	HubURL    string
	TokenFile string
	HTTP      *http.Client
	Live      bool
	LiveRun   func(ctx context.Context, name string, args []string) error
	Now       func() time.Time
}

// Run checks the hub and every client. It does not grant trust.
func Run(ctx context.Context, opt Options) (Result, error) {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.HTTP == nil {
		opt.HTTP = http.DefaultClient
	}
	resolved := (hubclient.EnvConfig{Getenv: opt.Layout.GetenvOr, DefaultTokenFile: defaultTokenFile(opt.Layout)}).Load()
	hubURL, source := resolved.HubURL(), resolved.Source
	if opt.HubURL != "" {
		hubURL = opt.HubURL
		source = "command line"
	}
	tokenFile, configErr := resolved.TokenPath()
	if configErr != nil {
		return Result{}, configErr
	}
	if opt.TokenFile != "" {
		tokenFile = opt.TokenFile
	}
	token, tokenReadErr := readToken(tokenFile)
	probe := probeHub(ctx, hubURL, token, tokenReadErr, opt.HTTP)
	res := Result{HubURL: hubURL, HubSource: source, HubUp: probe.up, TokenOK: probe.tokenOK, TokenUnchecked: probe.unchecked, HubDetail: probe.detail}
	res.Report.CheckedAt = opt.Now().UTC()
	for _, c := range integrate.All() {
		res.Report.Clients = append(res.Report.Clients, checkClient(ctx, opt, c))
	}
	if err := applyFailures(&res, tokenFile, opt.Now()); err != nil && res.HubDetail == "" {
		res.HubDetail = "failure log: " + err.Error()
	}
	for i := range res.Report.Clients {
		applyVersion(&res.Report.Clients[i])
	}
	if probe.up && probe.tokenOK {
		if err := putReport(ctx, hubURL, token, res.Report, opt.HTTP); err != nil {
			res.StoreError = err.Error()
		} else {
			res.Stored = true
		}
	}
	return res, nil
}

// ExitCode is 1 when the hub or token is unusable, or any client is silent.
func (r Result) ExitCode() int {
	if !r.HubUp || !r.TokenOK {
		return 1
	}
	for _, c := range r.Report.Clients {
		if c.Status == StatusSilent {
			return 1
		}
	}
	return 0
}

// Format is the text a person reads.
func (r Result) Format() string {
	var b strings.Builder
	if r.HubURL != "" {
		fmt.Fprintf(&b, "hub URL: %s (source: %s)\n", r.HubURL, r.HubSource)
	}
	state := "unreachable"
	if r.HubUp {
		state = "ok"
	}
	fmt.Fprintf(&b, "hub: %s", state)
	if r.HubDetail != "" {
		fmt.Fprintf(&b, " (%s)", r.HubDetail)
	}
	b.WriteByte('\n')
	if !r.HubUp || r.TokenUnchecked {
		b.WriteString("token: not checked\n")
	} else if r.TokenOK {
		b.WriteString("token: ok\n")
	} else {
		b.WriteString("token: not valid\n")
	}
	for _, c := range r.Report.Clients {
		fmt.Fprintf(&b, "%s\t%s\t%s", c.Client, c.Status, c.Version)
		if c.Detail != "" {
			fmt.Fprintf(&b, "\t%s", c.Detail)
		}
		b.WriteByte('\n')
	}
	if r.Stored {
		b.WriteString("stored: yes\n")
	} else if r.StoreError != "" {
		fmt.Fprintf(&b, "stored: no (%s)\n", r.StoreError)
	}
	return b.String()
}

func checkClient(ctx context.Context, opt Options, c integrate.Client) ClientReport {
	row := ClientReport{Client: c.Name(), Status: StatusNotConfigured, Detail: "configuration is absent"}
	if bin, err := opt.Layout.Binary(c.Name()); err == nil {
		if ver, err := commandVersion(ctx, bin); err == nil {
			row.Version = ver
		}
	}
	// A legacy Kimi binary never reads the Kimi Code integration directory.
	if legacyKimi(row) {
		applyVersion(&row)
		return row
	}
	in, err := c.Canary(opt.Layout)
	if err != nil {
		if errors.Is(err, integrate.ErrNotConfigured) {
			if _, berr := opt.Layout.Binary(c.Name()); berr != nil {
				row.Detail = "client binary not found"
			}
		} else {
			row.Status = StatusSilent
			row.Detail = err.Error()
		}
		return row
	}
	dir, dirErr := os.MkdirTemp("", "iris-canary-")
	if dirErr != nil {
		dir = string(os.PathSeparator)
	} else {
		defer os.RemoveAll(dir)
	}
	payload := canaryPayload(c.Name(), dir)
	stdout, exit, runErr := in.Run(ctx, payload)
	if runErr != nil {
		row.Status = StatusSilent
		row.Detail = "hook command failed: " + runErr.Error()
		return row
	}
	if err := validHook(c.Name(), stdout, exit); err != nil {
		row.Status = StatusSilent
		row.Detail = err.Error()
		return row
	}
	if opt.Live {
		if err := runLive(ctx, opt, c.Name()); err != nil {
			row.Status = StatusSilent
			row.Detail = "live run failed: " + err.Error()
			return row
		}
	}
	if step := c.Human(); step != "" {
		row.Status = StatusNeedsHuman
		row.Detail = step
		return row
	}
	if row.Version == "" {
		row.Status = StatusNeedsHuman
		row.Detail = "client binary not found"
		return row
	}
	row.Status = StatusOK
	row.Detail = ""
	return row
}

// failureWindow is how long a failed hook counts against a client. An old failure
// says nothing about whether the client is protected now.
const failureWindow = 24 * time.Hour

// applyFailures marks a client silent when hook-failures.log, beside the token
// file, has a recent entry for it. A hook that failed open did not protect the turn.
// An entry proves that a hook ran, so it counts even when doctor found no configuration.
func applyFailures(res *Result, tokenFile string, now time.Time) error {
	if strings.TrimSpace(tokenFile) == "" {
		return nil
	}
	entries, err := hook.ReadRecentFailures(tokenFile, 20)
	if err != nil {
		return err
	}
	by := map[string][]string{}
	for _, entry := range entries {
		name := integrate.Canonical(entry.Client)
		if name == "" || now.Sub(entry.Time) > failureWindow {
			continue
		}
		by[name] = append(by[name], entry.Event+" "+entry.Stage+": "+entry.Error)
	}
	for i := range res.Report.Clients {
		parts := by[res.Report.Clients[i].Client]
		if len(parts) == 0 {
			continue
		}
		res.Report.Clients[i].Status = StatusSilent
		res.Report.Clients[i].Detail = "recent hook failure: " + strings.Join(parts, "; ")
	}
	return nil
}

func validHook(client string, stdout []byte, exit int) error {
	if exit != 0 {
		return fmt.Errorf("hook command exited %d", exit)
	}
	body := bytes.TrimSpace(stdout)
	if client == integrate.Kimi && len(body) == 0 {
		return nil
	}
	if !json.Valid(body) {
		return errors.New("hook output is not valid JSON")
	}
	return nil
}

func commandVersion(ctx context.Context, bin string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, "--version")
	cmd.Stdin = closedStdin()
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	return line, nil
}

func canaryPayload(client, cwd string) []byte {
	quoted, _ := json.Marshal(cwd)
	switch client {
	case integrate.Copilot:
		return []byte(fmt.Sprintf(`{"sessionId":"iris-canary","cwd":%s}`, quoted))
	case integrate.Cursor:
		return []byte(fmt.Sprintf(`{"conversation_id":"iris-canary","cwd":%s,"hook_event_name":"sessionStart"}`, quoted))
	case integrate.Antigravity:
		return []byte(fmt.Sprintf(`{"conversationId":"iris-canary","workspacePaths":[%s]}`, quoted))
	default:
		return []byte(fmt.Sprintf(`{"session_id":"iris-canary","cwd":%s,"hook_event_name":"SessionStart"}`, quoted))
	}
}

// hubProbe is what one run learned about the hub and the token.
type hubProbe struct {
	up, tokenOK, unchecked bool
	detail                 string
}

// hubRequest sends one call. An empty token sends no credential.
func hubRequest(ctx context.Context, method, target, token string, body io.Reader, hc *http.Client) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return hc.Do(req)
}

func probeHub(ctx context.Context, base, token string, tokenErr error, hc *http.Client) hubProbe {
	if err := hubclient.ValidateURL(base); err != nil {
		return hubProbe{detail: err.Error()}
	}
	root := strings.TrimRight(base, "/")
	res, err := hubRequest(ctx, http.MethodGet, root+"/v1/status", "", nil, hc)
	if err != nil {
		return hubProbe{detail: "hub unreachable"}
	}
	res.Body.Close()
	if res.StatusCode >= 500 {
		return hubProbe{detail: "hub status " + res.Status}
	}
	if tokenErr != nil {
		return hubProbe{up: true, detail: tokenErr.Error()}
	}
	res, err = hubRequest(ctx, http.MethodGet, root+"/v1/agents", token, nil, hc)
	if err != nil {
		return hubProbe{up: true, unchecked: true, detail: "token check failed"}
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	switch {
	case res.StatusCode == http.StatusUnauthorized:
		return hubProbe{up: true, detail: "token was rejected"}
	case res.StatusCode >= 500:
		return hubProbe{up: true, unchecked: true, detail: "token check failed: hub status " + res.Status}
	}
	return hubProbe{up: true, tokenOK: true}
}

func putReport(ctx context.Context, base, token string, report Report, hc *http.Client) error {
	raw, err := json.Marshal(report)
	if err != nil {
		return err
	}
	res, err := hubRequest(ctx, http.MethodPut, strings.TrimRight(base, "/")+"/v1/ui/doctor", token, bytes.NewReader(raw), hc)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("PUT /v1/ui/doctor: %s", res.Status)
	}
	return nil
}

func readToken(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("token file is not set")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errors.New("token file is missing or empty")
		}
		return "", err
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", errors.New("token file is missing or empty")
	}
	return token, nil
}

func defaultTokenFile(l integrate.Layout) string {
	if v := l.GetenvOr("IRIS_TOKEN_FILE"); v != "" {
		return v
	}
	if v := l.GetenvOr("XDG_CONFIG_HOME"); v != "" && l.OS() != "windows" {
		return filepath.Join(v, "iris", "hub.token")
	}
	home, err := l.HomeDir()
	if err != nil {
		return ""
	}
	switch l.OS() {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "iris", "hub.token")
	case "windows":
		if v := l.GetenvOr("AppData"); v != "" {
			return filepath.Join(v, "iris", "hub.token")
		}
		return filepath.Join(home, "AppData", "Roaming", "iris", "hub.token")
	default:
		return filepath.Join(home, ".config", "iris", "hub.token")
	}
}

func closedStdin() io.Reader { return bytes.NewReader(nil) }

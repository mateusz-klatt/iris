// Package doctor reports whether each client is actually hooked up.
package doctor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/guard"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/integrate"
)

const (
	StatusOK            = "ok"
	StatusNotConfigured = "not_configured"
	StatusNeedsHuman    = "needs_human"
	StatusSilent        = "silent"
	// StatusSlow is graded here only. Until the hub knows it, the report sends it as silent
	// with a "slow: " detail; see published.
	StatusSlow = "slow"
)

// Report is the body stored with PUT /v1/ui/doctor.
type Report struct {
	// Host is this machine's name as its hooks give it, so the hub keeps this report beside
	// other hosts' instead of over them (ADR 007).
	Host      string         `json:"host"`
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
	// TokenUnavailable means no token could be read, so the hub never saw one.
	TokenUnavailable bool
	TokenDetail      string
	HubDetail        string
	// DatabaseBytes is the hub database's size, when the hub says it.
	DatabaseBytes int64
	Stored        bool
	StoreError    string
	// Guard is the pre-commit guard line for the repository doctor ran in.
	Guard string
	// Case warns when the repository doctor ran in is case-sensitive at the hub while this
	// checkout folds case. Empty means there is nothing to warn about, or nothing known.
	Case string
	// LineEndings warns when the repository doctor ran in holds CRLF files that this git reads
	// as modified, which keeps the shell reconcile past its budget. Empty means nothing to warn
	// about, or nothing known.
	LineEndings string
	// tokenFile is where the hook logs beside the token are, for a Bundle. Never shown.
	tokenFile string
}

// Options configures one offline doctor run. RunLive is separate because it calls a model.
type Options struct {
	Layout    integrate.Layout
	HubURL    string
	TokenFile string
	HTTP      *http.Client
	Now       func() time.Time
	// VersionTimeout limits one client --version call. Zero means the default.
	VersionTimeout time.Duration
	// Dir is where the guard is looked for. Empty means the working directory.
	Dir string
	// Self is this doctor's own binary. Empty means the running executable.
	Self string
	// Host is this machine's name as its hooks give it to the hub. Empty means os.Hostname.
	Host string
}

func (o Options) self() string {
	if o.Self != "" {
		return o.Self
	}
	exe, _ := os.Executable() // an error leaves "", which is never the same file as the hooks'
	return exe
}

// sameFile reports whether two paths name one file, through links.
func sameFile(a, b string) bool {
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}

func (o Options) host() string {
	if o.Host != "" {
		return o.Host
	}
	host, _ := os.Hostname() // what iris hook sends; an error leaves "", and the hub then says nothing
	return host
}

// versionTimeout is long enough for a normal client and short enough to keep doctor usable.
// A first start under antivirus can still pass it, which says nothing about the client.
const versionTimeout = 3 * time.Second

// Run checks the hub and every client. It does not grant trust.
func Run(ctx context.Context, opt Options) (Result, error) {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.HTTP == nil {
		opt.HTTP = http.DefaultClient
	}
	if opt.VersionTimeout == 0 {
		opt.VersionTimeout = versionTimeout
	}
	resolved := (hubclient.EnvConfig{Getenv: opt.Layout.GetenvOr, DefaultTokenFile: defaultTokenFile(opt.Layout)}).Load()
	hubURL, source := resolved.HubURL(), resolved.Source
	if opt.HubURL != "" {
		hubURL = opt.HubURL
		source = "command line"
	}
	if err := resolved.ConfigErr(); err != nil {
		return Result{}, err
	}
	tokenFile := resolved.TokenFile
	if opt.TokenFile != "" {
		tokenFile = opt.TokenFile
	}
	token, tokenReadErr := readToken(tokenFile)
	probe := probeHub(ctx, hubURL, token, tokenReadErr, opt.HTTP)
	res := Result{tokenFile: tokenFile, HubURL: hubURL, HubSource: source, HubUp: probe.up, TokenOK: probe.tokenOK, TokenUnchecked: probe.unchecked, TokenUnavailable: probe.unavailable, TokenDetail: probe.tokenDetail, HubDetail: probe.detail, DatabaseBytes: probe.databaseBytes}
	res.Report.CheckedAt = opt.Now().UTC()
	res.Report.Host = opt.host()
	for _, c := range integrate.All() {
		res.Report.Clients = append(res.Report.Clients, checkClient(ctx, opt, c))
	}
	if err := applyFailures(&res, tokenFile, opt.Now()); err != nil && res.HubDetail == "" {
		res.HubDetail = "failure log: " + err.Error()
	}
	for i := range res.Report.Clients {
		applyVersion(&res.Report.Clients[i])
	}
	res.Guard = guardLine(ctx, opt.Dir)
	res.LineEndings = lineEndingsLine(ctx, opt.Dir)
	if probe.up && probe.tokenOK {
		res.Case = caseLine(ctx, hubURL, token, opt.host(), opt.Dir, opt.HTTP)
		seen := lastHooks(ctx, hubURL, token, opt.host(), opt.HTTP)
		for i := range res.Report.Clients {
			res.Report.Clients[i].LastHookAt = seen[res.Report.Clients[i].Client]
		}
		if err := storeReport(ctx, hubURL, token, res.Report, opt.HTTP); err != nil {
			res.StoreError = err.Error()
		} else {
			res.Stored = true
		}
	}
	return res, nil
}

// guardLine reports the pre-commit guard of the repository containing dir.
// A missing guard does not fail doctor: it is cooperative and per repository.
func guardLine(ctx context.Context, dir string) string {
	if dir == "" {
		dir = "."
	}
	state, path, err := guard.Status(ctx, dir)
	switch {
	case errors.Is(err, gitinfo.ErrNotRepo):
		return "not in a git repository"
	case err != nil:
		return "unknown (" + err.Error() + ")"
	case state == guard.StateInstalled:
		return state + " (" + path + ")"
	}
	return state + " (" + path + "); run `iris guard install`"
}

// ExitCode is 1 when the hub or token is unusable, or any client is silent or slow.
func (r Result) ExitCode() int {
	if !r.HubUp || !r.TokenOK {
		return 1
	}
	for _, c := range r.Report.Clients {
		if c.Status == StatusSilent || c.Status == StatusSlow {
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
	if r.DatabaseBytes > 0 {
		fmt.Fprintf(&b, " (database %.1f MiB)", float64(r.DatabaseBytes)/(1<<20))
	}
	if r.HubDetail != "" {
		fmt.Fprintf(&b, " (%s)", r.HubDetail)
	}
	b.WriteByte('\n')
	state = "not valid"
	switch {
	case !r.HubUp || r.TokenUnchecked:
		state = "not checked"
	case r.TokenOK:
		state = "ok"
	case r.TokenUnavailable:
		state = "not available"
	}
	fmt.Fprintf(&b, "token: %s", state)
	if r.TokenDetail != "" {
		fmt.Fprintf(&b, " (%s)", r.TokenDetail)
	}
	b.WriteByte('\n')
	for _, c := range r.Report.Clients {
		fmt.Fprintf(&b, "%s\t%s\t%s", c.Client, c.Status, c.Version)
		if c.Detail != "" {
			fmt.Fprintf(&b, "\t%s", c.Detail)
		}
		if c.LastHookAt != "" {
			fmt.Fprintf(&b, "\tlast hook %s", c.LastHookAt)
		}
		b.WriteByte('\n')
	}
	if r.Case != "" {
		fmt.Fprintf(&b, "case policy: %s\n", r.Case)
	}
	if r.Guard != "" {
		fmt.Fprintf(&b, "pre-commit guard: %s\n", r.Guard)
	}
	if r.LineEndings != "" {
		fmt.Fprintf(&b, "line endings: %s\n", r.LineEndings)
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
	bin, binErr := opt.Layout.Binary(c.Name())
	var versionErr error
	if binErr == nil {
		row.Version, versionErr = commandVersion(ctx, bin, opt.VersionTimeout)
	}
	// Iris is measured on Kimi Code 2. An older kimi, whether an early Kimi Code or the
	// separate kimi-cli, says so here instead of reporting missing configuration.
	if oldKimi(row) {
		applyVersion(&row)
		return row
	}
	in, err := c.Canary(opt.Layout)
	if err != nil {
		if errors.Is(err, integrate.ErrNotConfigured) {
			if binErr != nil {
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
		defer func() { _ = os.RemoveAll(dir) }()
	}
	payload := canaryPayload(c.Name(), dir)
	nonce := canaryNonce()
	ran, runErr := in.Run(ctx, payload, hook.CanaryNonce+"="+nonce)
	if runErr != nil {
		row.Status = StatusSilent
		row.Detail = "hook command failed: " + runErr.Error()
		return row
	}
	if err := validHook(c.Name(), ran.Stdout, ran.Exit); err != nil {
		row.Status = StatusSilent
		row.Detail = err.Error()
		return row
	}
	// The canary ran one hook. The rest of the configuration, every other hook event and the
	// MCP entry, is checked against what iris integrate would write (gap review C3).
	drift, err := opt.Layout.Drift(c)
	if err != nil {
		row.Status = StatusSilent
		row.Detail = err.Error()
		return row
	}
	if len(drift) > 0 {
		row.Status = StatusNeedsHuman
		row.Detail = "configuration differs from what iris integrate installs (" + strings.Join(homeRelative(opt.Layout, drift), ", ") + "): run iris integrate " + c.Name()
		return row
	}
	// Exit 0 and valid JSON are what a stand-in prints too. Only iris hook answers the nonce,
	// so its line proves this configured command reaches Iris (gap review C3). It cannot prove
	// that the client loads the command and fires it; doctor --live does that. With no drift,
	// the command runs the iris that integrate would name. When that is another file than this
	// doctor, it may simply be an older iris, from before the receipt, so doctor says that.
	if !bytes.Contains(ran.Stderr, []byte("iris: canary "+nonce+"\n")) {
		if hooks := opt.Layout.Iris(); !sameFile(opt.self(), hooks) {
			row.Status = StatusNeedsHuman
			row.Detail = "the hooks run another iris than this doctor, " + hooks + " (" + describeVersion(commandVersion(ctx, hooks, opt.VersionTimeout)) + "), and it gave no canary receipt, as an iris from before the receipt does not: update that iris, or run doctor from it"
			return row
		}
		row.Status = StatusSilent
		row.Detail = "the hook command answered, but not from Iris: no canary receipt on stderr"
		return row
	}
	step, note := c.Human(), ""
	if r, ok := c.(integrate.Reviewer); ok {
		step, note = r.Review(opt.Layout)
	}
	if step != "" {
		row.Status = StatusNeedsHuman
		row.Detail = step
		return row
	}
	row.Status = StatusOK
	row.Detail = note
	switch {
	case binErr != nil:
		row.Status = StatusNeedsHuman
		row.Detail = "client binary not found"
	case errors.Is(versionErr, context.DeadlineExceeded):
		// The hook has just answered, so a slow version call is a note, not a fault.
		row.Detail = "version check timed out"
	case versionErr != nil:
		row.Status = StatusNeedsHuman
		row.Detail = "version check failed: " + versionErr.Error()
	}
	return row
}

// failureWindow is how long a failed hook counts against a client. An old failure
// says nothing about whether the client is protected now.
const failureWindow = 24 * time.Hour

// slowLimit is how many over-budget shell reconciliations in failureWindow a client can have
// before doctor says so. One is a cold cache; many mean changes routinely go unclaimed.
const slowLimit = 10

// applyFailures grades each client by its entries in hook-failures.log, beside the token
// file. A failure that shows the hook is broken makes the client silent: that hook did not
// protect the turn. A shell reconcile that only ran past its budget is degraded: the row
// keeps its status with a note, unless there were more than slowLimit of them, which makes it
// slow. An entry proves that a hook ran, so it counts even when doctor found no configuration.
func applyFailures(res *Result, tokenFile string, now time.Time) error {
	if strings.TrimSpace(tokenFile) == "" {
		return nil
	}
	entries, err := hook.ReadRecentFailures(tokenFile, failureRead)
	if err != nil {
		return err
	}
	seen, err := hook.ReadRecentObservations(tokenFile, failureRead)
	if err != nil {
		return err
	}
	entries = append(entries, seen...)
	broken := map[string][]hook.Failure{}
	degraded := map[string]int{}
	observed := map[string]map[string]int{}
	for _, entry := range entries {
		name := integrate.Canonical(entry.Client)
		if name == "" || now.Sub(entry.Time) > failureWindow {
			continue
		}
		if entry.Degraded() {
			degraded[name]++
			continue
		}
		if entry.Observed() {
			// Not a failure: the hook worked, but saw something doctor should say (C4).
			if observed[name] == nil {
				observed[name] = map[string]int{}
			}
			observed[name][entry.Error]++
			continue
		}
		broken[name] = append(broken[name], entry)
	}
	for i := range res.Report.Clients {
		row := &res.Report.Clients[i]
		n := degraded[row.Client]
		switch {
		case len(broken[row.Client]) > 0:
			row.Status = StatusSilent
			row.Detail = "recent hook failure: " + brokenText(broken[row.Client])
		case n > slowLimit:
			row.Status = StatusSlow
			row.Detail = fmt.Sprintf("%d shell reconciliations ran past their budget (%s, or %s for a hub on this machine) in 24 h, so those commands' changes went unclaimed; git status or the hub is slow here, as on a big or cold worktree, a loaded machine or a slow network", n, hook.RemoteReconcileDeadline, hook.ReconcileDeadline)
		case n > 0:
			row.Detail = joinDetail(row.Detail, fmt.Sprintf("degraded: %d shell reconciliation(s) ran past their budget in 24 h", n))
		}
		if seen := observed[row.Client]; len(seen) > 0 {
			row.Detail = joinDetail(row.Detail, "observed in 24 h: "+observedText(seen))
		}
	}
	return nil
}

// failureGroups is how many kinds of failure one doctor line names before it says how many more.
const failureGroups = 5

// brokenText names each kind of failure once: its event, stage and text, how often it
// happened, and when it last did, newest first. A crash loop that logged one failure a
// hundred times reads as one entry with its count, not a hundred.
func brokenText(entries []hook.Failure) string {
	type group struct {
		text  string
		count int
		last  time.Time
	}
	by := map[string]*group{}
	var groups []*group
	for _, e := range entries {
		text := e.Event + " " + e.Stage + ": " + e.Error
		g := by[text]
		if g == nil {
			g = &group{text: text}
			by[text] = g
			groups = append(groups, g)
		}
		g.count++
		if e.Time.After(g.last) {
			g.last = e.Time
		}
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].last.After(groups[j].last) })
	parts := []string{}
	for i, g := range groups {
		if i == failureGroups {
			parts = append(parts, fmt.Sprintf("and %d more", len(groups)-i))
			break
		}
		at := g.last.UTC().Format("15:04") + " UTC"
		if g.count == 1 {
			parts = append(parts, g.text+", at "+at)
			continue
		}
		parts = append(parts, fmt.Sprintf("%s ×%d, last at %s", g.text, g.count, at))
	}
	return strings.Join(parts, "; ")
}

// failureRead is how many log entries doctor reads: enough to count past slowLimit for every
// client at once, and still within the 64 KiB the log keeps.
const failureRead = 500

func joinDetail(detail, note string) string {
	if detail == "" {
		return note
	}
	return detail + "; " + note
}

// published is the report as a hub from before "slow" accepts it: such a hub refuses the whole
// report, so a slow row goes as silent with its grade in the detail.
func published(r Report) Report {
	out := r
	out.Clients = make([]ClientReport, len(r.Clients))
	for i, c := range r.Clients {
		if c.Status == StatusSlow {
			c.Status = StatusSilent
			c.Detail = "slow: " + c.Detail
		}
		out.Clients[i] = c
	}
	return out
}

// storeReport sends the report as graded. A hub from before "slow" refuses the whole report,
// so it then gets slow as silent, with the grade in the detail.
func storeReport(ctx context.Context, base, token string, report Report, hc *http.Client) error {
	err := putReport(ctx, base, token, report, hc)
	if errors.Is(err, errReportRefused) && hasSlow(report) {
		err = putReport(ctx, base, token, published(report), hc)
	}
	return err
}

func hasSlow(r Report) bool {
	for _, c := range r.Clients {
		if c.Status == StatusSlow {
			return true
		}
	}
	return false
}

// errReportRefused is a hub's 400 for a report it does not accept.
var errReportRefused = errors.New("the hub refused the report")

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

func commandVersion(ctx context.Context, bin string, limit time.Duration) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, "--version")
	cmd.Stdin = closedStdin()
	out, err := cmd.Output()
	if cctx.Err() != nil {
		return "", cctx.Err()
	}
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
	up, tokenOK, unchecked, unavailable bool
	// databaseBytes is the hub database's size, which grows with mail (ADR 006).
	databaseBytes       int64
	detail, tokenDetail string
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
	// The token must not follow a redirect to another scheme, host or port.
	return hubclient.NoRedirects(hc).Do(req)
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
	var status struct {
		DatabaseBytes int64 `json:"database_bytes"`
	}
	_ = json.NewDecoder(res.Body).Decode(&status) // an older hub does not say, which leaves 0
	_ = res.Body.Close()
	if res.StatusCode >= 500 {
		return hubProbe{detail: "hub status " + res.Status}
	}
	if tokenErr != nil {
		return hubProbe{up: true, unavailable: true, tokenDetail: tokenErr.Error()}
	}
	res, err = hubRequest(ctx, http.MethodGet, root+"/v1/agents", token, nil, hc)
	if err != nil {
		return hubProbe{up: true, unchecked: true, tokenDetail: "token check failed"}
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	switch {
	case res.StatusCode == http.StatusUnauthorized:
		return hubProbe{up: true, tokenDetail: "token was rejected"}
	case res.StatusCode >= 500:
		return hubProbe{up: true, unchecked: true, tokenDetail: "token check failed: hub status " + res.Status}
	}
	return hubProbe{up: true, tokenOK: true, databaseBytes: status.DatabaseBytes}
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
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode == http.StatusBadRequest {
		return fmt.Errorf("PUT /v1/ui/doctor: %s: %w", res.Status, errReportRefused)
	}
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

// homeRelative shortens paths under home to ~/..., so a report on the hub does not carry the
// user's home directory.
func homeRelative(l integrate.Layout, paths []string) []string {
	home, _ := l.HomeDir() // without a home, Rel below fails and the paths stay whole
	out := make([]string, len(paths))
	for i, p := range paths {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			p = filepath.Join("~", rel)
		}
		out[i] = p
	}
	return out
}

// canaryNonce is a fresh random value for one canary, which only iris hook echoes back.
func canaryNonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read always fills b or crashes the program.
	return hex.EncodeToString(b[:])
}

// describeVersion says what a version check found, including that it failed or timed out.
func describeVersion(version string, err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "version check timed out"
	case err != nil:
		return "version check failed: " + err.Error()
	}
	return "version " + version
}

// lastHooks asks the hub when each client's hooks on host last reached it: a session a hook
// registered, an automatic claim or a shell reconcile, never a heartbeat (gap review C4). A
// hub that cannot say, being older, unreachable or refusing, leaves every client unknown.
func lastHooks(ctx context.Context, base, token, host string, hc *http.Client) map[string]string {
	res, err := hubRequest(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v1/hooks/seen?"+url.Values{"host": {host}}.Encode(), token, nil, hc)
	if err != nil {
		return nil
	}
	defer func() { _ = res.Body.Close() }()
	var out struct {
		Clients map[string]string `json:"clients"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&out) != nil {
		return nil
	}
	return out.Clients
}

// caseLine is ADR 003's warning. Claims fold case in every repository unless it opts out as
// sensitive. A checkout where git sets core.ignorecase, as on a default macOS or Windows
// volume, cannot hold two paths that differ only by case, so in a sensitive repository a claim
// on a.go would not stop an edit of A.GO from here. Asking the hub uses the guard's check,
// which, like a commit, may register a repository the hub has not seen.
func caseLine(ctx context.Context, base, token, host, dir string, hc *http.Client) string {
	if dir == "" {
		dir = "."
	}
	repo, err := gitinfo.Find(dir)
	if err != nil {
		return ""
	}
	if folds, _ := repo.ConfigValue("core", "ignorecase"); !strings.EqualFold(folds, "true") {
		return ""
	}
	body, _ := json.Marshal(hubclient.CheckRequest{CommonDir: repo.CommonDir, Host: host, Worktree: repo.Worktree, WorktreeID: repo.WorktreeID, RemoteURL: repo.RemoteURL, RootCommit: repo.RootCommit, Paths: []string{}})
	res, err := hubRequest(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/guard/check", token, bytes.NewReader(body), hc)
	if err != nil {
		return ""
	}
	defer func() { _ = res.Body.Close() }()
	var out hubclient.CheckResponse
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&out) != nil || out.CasePolicy != "sensitive" {
		return ""
	}
	return "this repository is case-sensitive at the hub, but this checkout folds case (core.ignorecase), so a claim on a.go does not stop an edit of A.GO here"
}

// lineEndingsLine names a worktree where this git reads many tracked files as modified only
// because they hold CRLF and git stores LF, as when a Windows checkout written with
// core.autocrlf=true is used from WSL. Every git status there re-reads those files, so the
// shell reconcile runs past its budget and fails open on every command. The fix is the
// person's to choose; Iris does not change how git reads someone's checkout.
func lineEndingsLine(ctx context.Context, dir string) string {
	if dir == "" {
		dir = "."
	}
	repo, err := gitinfo.Find(dir)
	if err != nil {
		return ""
	}
	crlf, text, err := gitinfo.LineEndings(ctx, repo.Worktree)
	if err != nil || crlf < crlfFloor || crlf*2 < text {
		return ""
	}
	return fmt.Sprintf("%d of %d tracked text files hold CRLF line endings that this git reads as changes (core.autocrlf is not true or input here). "+
		"Each already reads as modified, so a shell command's change to one goes unseen, and git status re-reads them all, which can keep the reconcile past its budget, where it detects nothing. "+
		"Advice, not something doctor changes: if this is a Windows checkout used from WSL, set `git config core.autocrlf true` in it, "+
		"or use it only from the git that wrote it", crlf, text)
}

// crlfFloor keeps a few CRLF files, which an editor can write in any repository, from being
// reported as a misconfigured checkout.
const crlfFloor = 20

// observedText lists observations with their counts, in a stable order.
func observedText(seen map[string]int) string {
	texts := make([]string, 0, len(seen))
	for text := range seen {
		texts = append(texts, text)
	}
	sort.Strings(texts)
	for i, text := range texts {
		texts[i] = fmt.Sprintf("%s (%d)", text, seen[text])
	}
	return strings.Join(texts, "; ")
}

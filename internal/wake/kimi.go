package wake

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

const kimiActive = "Kimi session is active"

type kimiInstance struct {
	ServerID    string `json:"server_id"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	PID         int    `json:"pid"`
	StartedMS   int64  `json:"started_at"`
	HostVersion string `json:"host_version"`
}

type kimiServer struct {
	base, version, token string
	client               *http.Client
	transport            *http.Transport
}

func (s *kimiServer) close() {
	if s.transport != nil {
		s.transport.CloseIdleConnections()
	}
}

type kimiCommands struct {
	home           string
	homeFailure    string
	http           *http.Client
	readFile       func(string) ([]byte, error)
	readDir        func(string) ([]os.DirEntry, error)
	now            func() time.Time
	versionTimeout time.Duration
	idleTimeout    time.Duration
	startTimeout   time.Duration
	queueTimeout   time.Duration
	server         *kimiServer
	lookupProcess  func(int) (proc.Snapshot, bool)
}

var kimiUserHomeDir = os.UserHomeDir

func newKimiCommands() *kimiCommands {
	c := &kimiCommands{
		http: http.DefaultClient, readFile: os.ReadFile, readDir: os.ReadDir, now: time.Now, lookupProcess: proc.Lookup,
		versionTimeout: 5 * time.Second, idleTimeout: 5 * time.Second, startTimeout: 2 * time.Second, queueTimeout: 30 * time.Second,
	}
	home := os.Getenv("KIMI_CODE_HOME")
	if home == "" {
		var err error
		home, err = kimiUserHomeDir()
		if err != nil {
			c.homeFailure = "Kimi home could not be determined"
			return c
		}
		home = filepath.Join(home, ".kimi-code")
	}
	c.home = filepath.Clean(home)
	return c
}

func pinKimiClient(client *http.Client) (*http.Client, *http.Transport) {
	if client == nil {
		client = http.DefaultClient
	}
	base, _ := client.Transport.(*http.Transport)
	if base == nil {
		base = http.DefaultTransport.(*http.Transport)
	}
	transport := base.Clone()
	dial := transport.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	var mu sync.Mutex
	dialed := false
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		if dialed {
			return nil, errors.New("kimi server attempted to replace its verified connection")
		}
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		dialed = true
		return conn, nil
	}
	transport.MaxConnsPerHost = 1
	transport.MaxIdleConnsPerHost = 1
	pinned := *client
	pinned.Transport = transport
	return &pinned, transport
}

func (c *kimiCommands) check(ctx context.Context, clientVersion string) (binaryInfo, string) {
	info := binaryInfo{ClientVersion: clientVersion, CheckedMS: c.now().UnixMilli()}
	if c.server != nil {
		c.server.close()
	}
	c.server = nil
	if c.homeFailure != "" {
		return info, c.homeFailure
	}
	if clientVersion == "" {
		return info, "Kimi initialization did not name its version"
	}
	token, err := c.readSecret(filepath.Join(c.home, "server.token"))
	if err != nil {
		return info, "Kimi server token could not be read"
	}
	entries, err := c.readDir(filepath.Join(c.home, "server", "instances"))
	if err != nil {
		return info, "Kimi server instances could not be read"
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
	versionMismatch := false
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		raw, err := c.readFile(filepath.Join(c.home, "server", "instances", entry.Name()))
		if err != nil || len(raw) > 4096 {
			continue
		}
		var instance kimiInstance
		if json.Unmarshal(raw, &instance) != nil || instance.ServerID == "" || entry.Name() != instance.ServerID+".json" || instance.HostVersion == "" ||
			(instance.Host != "127.0.0.1" && instance.Host != "::1") || instance.Port < 1 || instance.Port > 65535 || instance.PID <= 0 || instance.StartedMS <= 0 {
			continue
		}
		process, ok := c.lookupProcess(instance.PID)
		if !ok || process.StartMS <= 0 || process.StartMS > instance.StartedMS || !proc.SingleSession(proc.Ancestor(process), "kimi") {
			continue
		}
		candidate := &kimiServer{
			version: instance.HostVersion,
			base:    "http://" + net.JoinHostPort(instance.Host, strconv.Itoa(instance.Port)), // NOSONAR: go:S5332; Kimi uses HTTP on the literal loopback allowlisted above.
		}
		candidate.client, candidate.transport = pinKimiClient(c.http)
		var health struct {
			OK bool `json:"ok"`
		}
		var meta struct {
			ServerID      string `json:"server_id"`
			ServerVersion string `json:"server_version"`
		}
		probe, cancel := context.WithTimeout(ctx, c.versionTimeout)
		err = c.call(probe, candidate, http.MethodGet, "/api/v1/healthz", nil, &health)
		if err == nil && health.OK {
			candidate.token = token
			// Kimi 2.6 uses different ids in the registry and /meta. The registry id is
			// checked against its filename above; live metadata must have an id and its version.
			err = c.call(probe, candidate, http.MethodGet, "/api/v1/meta", nil, &meta)
		}
		cancel()
		if err != nil || !health.OK || meta.ServerID == "" || meta.ServerVersion != candidate.version {
			candidate.close()
			continue
		}
		info.Path, info.Version = candidate.base, candidate.version
		if candidate.version != clientVersion {
			versionMismatch = true
			candidate.close()
			continue
		}
		c.server = candidate
		return info, ""
	}
	if versionMismatch {
		return info, "Kimi executable and server versions differ"
	}
	return info, "Kimi web server is unavailable or incompatible"
}

func (c *kimiCommands) readSecret(path string) (string, error) {
	raw, err := c.readFile(path)
	if err != nil || len(raw) > 4096 {
		return "", errors.New("token unavailable")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" || len(token) > 512 || strings.IndexFunc(token, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 {
		return "", errors.New("token invalid")
	}
	return token, nil
}

type kimiAPIError struct{ code int }

func (e *kimiAPIError) Error() string { return fmt.Sprintf("Kimi API error %d", e.code) }

func newKimiRequest(ctx context.Context, server *kimiServer, method, path string, body any) (*http.Request, error) {
	var input io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		input = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, server.base+path, input)
	if err != nil {
		return nil, err
	}
	if server.token != "" {
		req.Header.Set("Authorization", "Bearer "+server.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *kimiCommands) httpClient(server *kimiServer) *http.Client {
	if server.client != nil {
		return server.client
	}
	return c.http
}

func (c *kimiCommands) startCall(ctx context.Context, server *kimiServer, method, path string, body any) (*http.Response, error) {
	req, err := newKimiRequest(ctx, server, method, path, body)
	if err != nil {
		return nil, err
	}
	return hubclient.NoRedirects(c.httpClient(server)).Do(req)
}

type kimiCallResult struct {
	res *http.Response
	err error
}

type kimiCall struct {
	done chan kimiCallResult
	stop context.CancelFunc
}

func (c *kimiCall) wait() (*http.Response, error) {
	result := <-c.done
	return result.res, result.err
}

func (c *kimiCommands) beginCall(ctx context.Context, server *kimiServer, method, path string, body any) (*kimiCall, error) {
	requestCtx, stopRequest := context.WithCancel(ctx)
	req, err := newKimiRequest(requestCtx, server, method, path, body)
	if err != nil {
		stopRequest()
		return nil, err
	}
	wrote := make(chan struct{}, 1)
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) {
			select {
			case wrote <- struct{}{}:
			default:
			}
		},
	}))
	call := &kimiCall{done: make(chan kimiCallResult, 1), stop: stopRequest}
	go func() {
		res, err := hubclient.NoRedirects(c.httpClient(server)).Do(req)
		call.done <- kimiCallResult{res: res, err: err}
	}()
	timer := time.NewTimer(c.startTimeout)
	select {
	case <-wrote:
		timer.Stop()
	case result := <-call.done:
		timer.Stop()
		call.done <- result
	case <-timer.C:
		stopRequest()
		result := <-call.done
		call.done <- result
	}
	return call, nil
}

func (c *kimiCommands) finishCall(res *http.Response, dest any) error {
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 65537))
	if err != nil || len(raw) > 65536 {
		return errors.New("invalid Kimi response")
	}
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Code == nil {
		return errors.New("invalid Kimi response")
	}
	if *envelope.Code != 0 {
		return &kimiAPIError{code: *envelope.Code}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("kimi HTTP status %d", res.StatusCode)
	}
	if dest == nil {
		return nil
	}
	if len(envelope.Data) == 0 || json.Unmarshal(envelope.Data, dest) != nil {
		return errors.New("invalid Kimi response data")
	}
	return nil
}

func (c *kimiCommands) call(ctx context.Context, server *kimiServer, method, path string, body, dest any) error {
	res, err := c.startCall(ctx, server, method, path, body)
	if err != nil {
		return err
	}
	return c.finishCall(res, dest)
}

func (c *kimiCommands) idle(ctx context.Context, session string) (bool, string) {
	if c.server == nil {
		return false, "Kimi web server has not passed its preflight"
	}
	ctx, cancel := context.WithTimeout(ctx, c.idleTimeout)
	defer cancel()
	var status struct {
		ID                 string `json:"id"`
		Busy               *bool  `json:"busy"`
		MainTurnActive     *bool  `json:"main_turn_active"`
		PendingInteraction string `json:"pending_interaction"`
		Archived           *bool  `json:"archived"`
	}
	err := c.call(ctx, c.server, http.MethodGet, "/api/v1/sessions/"+url.PathEscape(session), nil, &status)
	var api *kimiAPIError
	if errors.As(err, &api) && api.code == 40931 {
		return false, kimiActive
	}
	if err != nil || status.ID != session || status.Busy == nil || status.MainTurnActive == nil || status.Archived == nil {
		return false, "Kimi session status could not be verified"
	}
	if *status.Busy || *status.MainTurnActive || *status.Archived || status.PendingInteraction != "none" {
		return false, kimiActive
	}
	return true, ""
}

func kimiPromptID(message string) string {
	sum := sha256.Sum256([]byte(message))
	return "iris-" + hex.EncodeToString(sum[:])
}

func (c *kimiCommands) queue(ctx context.Context, session, message string, start func(func() error) (bool, error)) commandAnswer {
	if c.server == nil {
		return commandAnswer{answer: "failed", detail: "Kimi web server has not passed its preflight"}
	}
	ctx, cancel := context.WithTimeout(ctx, c.queueTimeout)
	defer cancel()
	id := kimiPromptID(message)
	var answer struct {
		PromptID string `json:"prompt_id"`
		Status   string `json:"status"`
		Content  []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	var call *kimiCall
	attempted := false
	_, admissionErr := start(func() error {
		attempted = true
		var err error
		call, err = c.beginCall(ctx, c.server, http.MethodPost, "/api/v1/sessions/"+url.PathEscape(session)+"/prompts", map[string]any{
			"prompt_id": id,
			"content":   []map[string]string{{"type": "text", "text": message}},
		})
		return err
	})
	if !attempted {
		if admissionErr != nil {
			return commandAnswer{answer: "failed", detail: "Kimi wake admission failed"}
		}
		return commandAnswer{answer: "aborted", detail: "Session binding changed before prompt submission"}
	}
	result := commandAnswer{started: true}
	if admissionErr != nil {
		result.answer, result.detail = "unknown", "Kimi prompt response was not a known outcome"
		return result
	}
	defer call.stop()
	res, err := call.wait()
	if err == nil {
		err = c.finishCall(res, &answer)
	}
	if err == nil && answer.PromptID == id && len(answer.Content) == 1 && answer.Content[0].Type == "text" && answer.Content[0].Text == message {
		switch answer.Status {
		case "running", "queued":
			result.answer = "submitted"
			return result
		case "blocked", "failed":
			result.answer, result.detail = "failed", "Kimi rejected the wake prompt"
			return result
		}
	}
	var api *kimiAPIError
	if errors.As(err, &api) && api.code == 40931 {
		return commandAnswer{answer: "aborted", detail: kimiActive}
	}
	if errors.As(err, &api) {
		switch api.code {
		case 40001, 40101, 40110, 40113, 40401, 40407, 40415, 40420, 40912, 40926, 40930, 40932:
			result.answer, result.detail = "failed", "Kimi refused the wake prompt"
			return result
		}
	}
	result.answer, result.detail = "unknown", "Kimi prompt response was not a known outcome"
	return result
}

package wake

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func init() {
	if os.Getenv("IRIS_WAKE_COMMAND_HELPER") != "" {
		return
	}
	if mode := os.Getenv("IRIS_WAKE_PROXY_HELPER"); mode != "" {
		os.Exit(runProxyHelper(mode))
	}
}

// The helper is an independent HTTP/WebSocket peer on stdio. No Iris transport
// code is called by the child, and no daemon or real Codex thread is touched.
type proxyHelperConn struct{ io.ReadWriteCloser }

func (proxyHelperConn) LocalAddr() net.Addr              { return &net.IPAddr{} }
func (proxyHelperConn) RemoteAddr() net.Addr             { return &net.IPAddr{} }
func (proxyHelperConn) SetDeadline(time.Time) error      { return nil }
func (proxyHelperConn) SetReadDeadline(time.Time) error  { return nil }
func (proxyHelperConn) SetWriteDeadline(time.Time) error { return nil }

type helperStdio struct{}

func (helperStdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (helperStdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (helperStdio) Close() error                { return nil }

type proxyListener struct{ accepted bool }

func (l *proxyListener) Accept() (net.Conn, error) {
	if l.accepted {
		return nil, io.EOF
	}
	l.accepted = true
	return proxyHelperConn{helperStdio{}}, nil
}
func (*proxyListener) Addr() net.Addr { return &net.IPAddr{} }
func (*proxyListener) Close() error   { return nil }

func runProxyHelper(mode string) int {
	cwd, _ := os.Getwd()
	record := struct {
		commandRecord
		Requests []json.RawMessage
		Closed   bool
	}{commandRecord: commandRecord{Args: os.Args[1:], CWD: cwd, Home: os.Getenv("HOME"), CodexHome: os.Getenv("CODEX_HOME")}}
	if mode == "exit" {
		return 21
	}
	if mode == "hang" {
		time.Sleep(time.Minute)
		return 22
	}
	if mode == "bad handshake" {
		_, _ = io.Copy(io.Discard, io.LimitReader(os.Stdin, 16))
		_, _ = io.WriteString(os.Stdout, "not HTTP\r\n\r\n")
		return 0
	}
	done := make(chan struct{})
	server := &http.Server{ErrorLog: log.New(io.Discard, "", 0), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx := context.Background()
		for range 3 {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				return
			}
			record.Requests = append(record.Requests, raw)
			var request struct {
				ID int `json:"id"`
			}
			_ = json.Unmarshal(raw, &request)
			if request.ID == 0 {
				continue
			}
			if mode == "stdout flood" {
				for range 24 {
					if conn.Write(ctx, websocket.MessageText, []byte(`{"method":"noise","params":"`+strings.Repeat("x", 64<<10)+`"}`)) != nil {
						return
					}
				}
			}
			result := `{"userAgent":"test"}`
			if request.ID == 2 {
				status := mode
				if mode == "stderr overflow" || mode == "exit after read" || mode == "hang after read" || mode == "keep pipes" {
					status = "idle"
				}
				result = fmt.Sprintf(`{"thread":{"id":%q,"status":{"type":%q},"turns":[]}}`, commandSession, status)
			}
			_ = conn.Write(ctx, websocket.MessageText, []byte(`{"method":"account/updated","params":{"secret":"`+commandSecret+`"}}`))
			_ = conn.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`{"id":%d,"result":%s}`, request.ID, result)))
		}
		if mode == "stderr overflow" {
			_, _ = io.WriteString(os.Stderr, strings.Repeat("x", (64<<10)+1))
		}
		_, _, err = conn.Read(ctx)
		record.Closed = err != nil
	})}
	_ = server.Serve(&proxyListener{})
	<-done
	raw, _ := json.Marshal(record)
	_ = os.WriteFile(os.Getenv("IRIS_WAKE_PROXY_RECORD"), raw, 0600)
	if mode == "hang after read" {
		time.Sleep(time.Minute)
	}
	if mode == "keep pipes" {
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "IRIS_WAKE_COMMAND_HELPER=pipe-keeper")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if child.Start() != nil {
			return 24
		}
		defer func() { _ = child.Process.Release() }()
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
			if _, err := os.Stat(os.Getenv("IRIS_WAKE_COMMAND_CONTROL") + ".keeper-ready"); err == nil {
				return 0
			}
			time.Sleep(5 * time.Millisecond)
		}
		return 25
	}
	if mode == "idle" || mode == "active" {
		return 0
	}
	return 23
}

func TestWakeNativeIdleProxy(t *testing.T) {
	binary := commandBinary(t)
	setup := func(t *testing.T, mode string) (*commands, string) {
		t.Helper()
		t.Setenv("PATH", filepath.Dir(binary))
		t.Setenv("IRIS_WAKE_COMMAND_HELPER", "")
		t.Setenv("IRIS_WAKE_PROXY_HELPER", mode)
		t.Setenv("GOCOVERDIR", t.TempDir())
		record := filepath.Join(t.TempDir(), "record.json")
		t.Setenv("IRIS_WAKE_PROXY_RECORD", record)
		return newCommands(), record
	}
	for _, mode := range []string{"idle", "active"} {
		t.Run(mode, func(t *testing.T) {
			c, recordPath := setup(t, mode)
			if c.idleTimeout != 5*time.Second {
				t.Fatalf("status deadline: %s", c.idleTimeout)
			}
			c.dir = t.TempDir()
			c.env = append(os.Environ(), "HOME=home sentinel", "CODEX_HOME=custom home sentinel")
			idle, detail := c.idle(context.Background(), commandSession)
			if idle != (mode == "idle") || (idle && detail != "") || (!idle && detail != "Codex thread is active") {
				t.Fatalf("status %s: %t %q", mode, idle, detail)
			}
			var record struct {
				commandRecord
				Requests []struct {
					ID     int            `json:"id"`
					Method string         `json:"method"`
					Params map[string]any `json:"params"`
				}
				Closed bool
			}
			raw, err := os.ReadFile(recordPath)
			if err != nil || json.Unmarshal(raw, &record) != nil {
				t.Fatalf("proxy did not exit and record protocol: %v", err)
			}
			if !reflect.DeepEqual(record.Args, []string{"app-server", "proxy"}) || record.CWD != c.dir || record.Home != "home sentinel" || record.CodexHome != "custom home sentinel" || !record.Closed {
				t.Fatalf("proxy argv/environment/closure: %+v", record)
			}
			if len(record.Requests) != 3 || record.Requests[0].Method != "initialize" || record.Requests[1].Method != "initialized" || record.Requests[2].Method != "thread/read" || !reflect.DeepEqual(record.Requests[2].Params, map[string]any{"threadId": commandSession, "includeTurns": false}) {
				t.Fatalf("unexpected mutation or thread selection: %+v", record.Requests)
			}
		})
	}
	for _, mode := range []string{"exit", "hang", "bad handshake", "notLoaded", "systemError", "futureState", "exit after read", "hang after read", "stderr overflow", "stdout flood"} {
		t.Run(mode, func(t *testing.T) {
			c, _ := setup(t, mode)
			c.idleTimeout, c.waitDelay = 600*time.Millisecond, 30*time.Millisecond
			before := time.Now()
			idle, detail := c.idle(context.Background(), commandSession)
			if idle || detail == "" || strings.Contains(detail, commandSecret) || time.Since(before) > 3*time.Second {
				t.Fatalf("unverified status admitted or leaked: %t %q, elapsed %s", idle, detail, time.Since(before))
			}
		})
	}
	t.Run("start failure", func(t *testing.T) {
		c, _ := setup(t, "idle")
		c.path = filepath.Join(t.TempDir(), "missing")
		if idle, detail := c.idle(context.Background(), commandSession); idle || detail != "Codex status proxy could not start" {
			t.Fatalf("failed start: %t %q", idle, detail)
		}
	})
	t.Run("inherited handles are bounded by WaitDelay", func(t *testing.T) {
		c, record := setup(t, "keep pipes")
		control := record + ".control"
		t.Setenv("IRIS_WAKE_COMMAND_CONTROL", control)
		c.waitDelay = 50 * time.Millisecond
		t.Cleanup(func() {
			_ = os.WriteFile(control+".release", nil, 0600)
			awaitCommandFile(t, control+".keeper-done")
		})
		before := time.Now()
		idle, detail := c.idle(context.Background(), commandSession)
		if idle || detail != "Codex status proxy exceeded its bound or failed" || time.Since(before) > 3*time.Second {
			t.Fatalf("inherited handles: %t %q %s", idle, detail, time.Since(before))
		}
		awaitCommandFile(t, control+".keeper-ready")
	})
}

func TestWakeIdleProtocolRejectsUncorrelatedAndUnavailableStatus(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		kind          websocket.MessageType
	}{
		{"failed initialize", `{"id":1,"error":{"code":-1}}`, websocket.MessageText},
		{"closed before initialize", `{}`, websocket.MessageText},
		{"malformed JSON", `private malformed`, websocket.MessageText},
		{"binary frame", `{}`, websocket.MessageBinary},
		{"RPC failure", `{"id":2,"error":{"code":-1,"message":"private"}}`, websocket.MessageText},
		{"missing result", `{"id":2}`, websocket.MessageText},
		{"bad result", `{"id":2,"result":[]}`, websocket.MessageText},
		{"wrong thread", `{"id":2,"result":{"thread":{"id":"other","status":{"type":"idle"}}}}`, websocket.MessageText},
		{"missing status", `{"id":2,"result":{"thread":{"id":"` + commandSession + `"}}}`, websocket.MessageText},
		{"unloaded", `{"id":2,"result":{"thread":{"id":"` + commandSession + `","status":{"type":"notLoaded"}}}}`, websocket.MessageText},
		{"too many unrelated frames", `{"id":99,"result":{}}`, websocket.MessageText},
		{"oversized frame", strings.Repeat("x", (256<<10)+1), websocket.MessageText},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = conn.CloseNow() }()
				ctx := r.Context()
				_, _, _ = conn.Read(ctx)
				if tc.name == "failed initialize" {
					_ = conn.Write(ctx, tc.kind, []byte(tc.payload))
					return
				}
				_ = conn.Write(ctx, websocket.MessageText, []byte(`{"id":1,"result":{}}`))
				_, _, _ = conn.Read(ctx)
				_, _, _ = conn.Read(ctx)
				count := 1
				if tc.name == "too many unrelated frames" {
					count = 32
				}
				for range count {
					_ = conn.Write(ctx, tc.kind, []byte(tc.payload))
				}
				_, _, _ = conn.Read(ctx)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.CloseNow() }()
			conn.SetReadLimit(256 << 10)
			if tc.name == "closed before initialize" {
				_ = conn.CloseNow()
			}
			if status, err := readIdle(ctx, conn, commandSession); err == nil || status != "" {
				t.Fatalf("unverified response authorized %q: %v", status, err)
			}
		})
	}
}

func TestProxyUpgradePreservesBufferedFramesAndReportsIOFailures(t *testing.T) {
	request, _ := http.NewRequest(http.MethodGet, "http://localhost/", nil)
	for _, tc := range []struct {
		name, input string
		writer      io.Writer
		wantError   bool
	}{
		{"buffered", "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\nframe", io.Discard, false},
		{"invalid response", "invalid HTTP", io.Discard, true},
		{"failed request write", "", closedWriter{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &proxyStream{Reader: bufio.NewReader(strings.NewReader(tc.input)), Writer: tc.writer}
			response, err := p.RoundTrip(request)
			if (err != nil) != tc.wantError {
				t.Fatalf("upgrade: %v", err)
			}
			if !tc.wantError {
				body, err := io.ReadAll(response.Body)
				if err != nil || string(body) != "frame" {
					t.Fatalf("buffered bytes lost: %q %v", body, err)
				}
			}
		})
	}
}

type closedWriter struct{}

func (closedWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

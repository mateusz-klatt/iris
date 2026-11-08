package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/store"
)

// TestShellReconcileLatency runs real shell hooks against a hub 80 ms away, which a hook
// reaches over TLS on a non-loopback address and so gives the 1 s remote budget, and against a
// loopback hub with no added delay, which keeps 300 ms. Every reconcile must finish inside its
// budget: one that ran out is recorded in hook-failures.log, and its changes went unclaimed
// (D3). A hook refuses cleartext to any host but loopback, so the remote hub needs a
// certificate the binary trusts, given through SSL_CERT_FILE, which Linux and macOS honour.
func TestShellReconcileLatency(t *testing.T) {
	if os.Getenv("IRIS_MEASURE_LATENCY") != "1" {
		t.Skip("set IRIS_MEASURE_LATENCY=1 to run real shell hooks against a delayed hub")
	}
	binary := filepath.Join(t.TempDir(), "iris")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	if out, err := exec.Command("go", "build", "-trimpath", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	t.Run("remote hub, 80 ms away", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows takes roots only from its store, so the binary cannot be given a test certificate; the budget choice itself is unit-tested")
		}
		shellHooks(t, binary, nonLoopback(t), 80*time.Millisecond)
	})
	t.Run("loopback hub", func(t *testing.T) { shellHooks(t, binary, "127.0.0.1", 0) })
}

// farListener and farConn delay every write by one round trip, as a distant hub's answers are.
type farListener struct {
	net.Listener
	rtt time.Duration
}

func (l farListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil || l.rtt == 0 {
		return c, err
	}
	return &farConn{Conn: c, rtt: l.rtt}, nil
}

type farConn struct {
	net.Conn
	rtt  time.Duration
	last time.Time
}

// Write waits a round trip, except within one burst: a TLS flight or a response written in
// several pieces arrives together. The first waits two, as the client's connect costs one
// before its TLS handshake starts.
func (c *farConn) Write(p []byte) (int, error) {
	switch {
	case c.last.IsZero():
		time.Sleep(2 * c.rtt)
	case time.Since(c.last) > time.Millisecond:
		time.Sleep(c.rtt)
	}
	n, err := c.Conn.Write(p)
	c.last = time.Now()
	return n, err
}

// certificateFor makes a self-signed certificate for ip, and writes it where SSL_CERT_FILE can
// point, so a hook trusts the test hub and nothing else.
func certificateFor(t *testing.T, ip net.IP, dir string) (tls.Certificate, string) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "iris test hub"}, IPAddresses: []net.IP{ip},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "hub.pem")
	if err = os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, file
}

// nonLoopback is an address of this machine that is not loopback, so a hook takes a hub there
// for one elsewhere.
func nonLoopback(t *testing.T) string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			return n.IP.String()
		}
	}
	t.Fatal("this machine has no non-loopback IPv4 address to stand for a remote hub")
	return ""
}

func shellHooks(t *testing.T, binary, host string, delay time.Duration) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New(s, "latency-token", []string{listener.Addr().String()})
	server := httptest.NewUnstartedServer(h)
	_ = server.Listener.Close()
	// Each answer the hub writes reaches the hook a round trip later: the TLS handshake of the
	// new connection every hook process opens, then each response.
	server.Listener = farListener{listener, delay}
	dir := t.TempDir()
	var trust []string
	if ip := net.ParseIP(host); ip.IsLoopback() {
		server.Start()
	} else {
		cert, file := certificateFor(t, ip, dir)
		server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
		server.StartTLS()
		trust = []string{"SSL_CERT_FILE=" + file}
	}
	defer server.Close()
	token := filepath.Join(dir, "token")
	if err = os.WriteFile(token, []byte("latency-token"), 0600); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, v := range os.Environ() {
		key, _, _ := strings.Cut(v, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "IRIS_") || upper == "SSL_CERT_FILE" || upper == "COPILOT_AGENT_SESSION_ID" || upper == "GROK_SESSION_ID" || upper == "CURSOR_CONVERSATION_ID" || upper == "ANTIGRAVITY_CONVERSATION_ID" {
			continue
		}
		env = append(env, v)
	}
	env = append(append(env, trust...), "IRIS_HUB_URL="+server.URL, "IRIS_TOKEN_FILE="+token)
	run := func(event string, i int) time.Duration {
		payload, err := json.Marshal(map[string]any{"session_id": "shell-latency", "cwd": repo, "hook_event_name": event, "tool_name": "Bash", "tool_use_id": fmt.Sprintf("shell-%d", i), "tool_input": map[string]string{"command": "touch"}, "tool_response": map[string]any{"stdout": ""}})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, "hook", "--client", "claude", "--event", event)
		cmd.Env, cmd.Dir, cmd.Stdin = env, repo, bytes.NewReader(payload)
		start := time.Now()
		out, err := cmd.CombinedOutput()
		if err != nil || !json.Valid(bytes.TrimSpace(out)) {
			t.Fatalf("%s %d: %v: %s", event, i, err, out)
		}
		return time.Since(start)
	}
	const calls = 10
	var slowest time.Duration
	for i := range calls {
		slowest = max(slowest, run("PreToolUse", i))
		if err = os.WriteFile(filepath.Join(repo, fmt.Sprintf("out%d.txt", i)), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		slowest = max(slowest, run("PostToolUse", i))
	}
	// A reconcile past its budget fails open and says so here. What this test is about is the
	// hub's share: a run out of budget while talking to the hub fails it. git status is this
	// machine's share, and a slow one, as on a busy Windows runner, is the degraded case doctor
	// already tolerates, so it is allowed here; every such call is one change that may go
	// unclaimed.
	gitSlow := 0
	if raw, err := os.ReadFile(filepath.Join(dir, "hook-failures.log")); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line == "" {
				continue
			}
			var entry struct{ Stage, Error string }
			if json.Unmarshal([]byte(line), &entry) != nil {
				t.Fatalf("an unreadable failure log:\n%s", raw)
			}
			// On a loopback hub every overrun is this machine's: git status runs first and can
			// use the budget up before the hub call, so the overrun shows at that call's stage.
			machine := entry.Stage == "shell status" || (delay == 0 && strings.HasPrefix(entry.Stage, "shell "))
			if !machine || entry.Error != "shell reconciliation passed its deadline" {
				t.Fatalf("a shell hook failed open:\n%s", raw)
			}
			gitSlow++
		}
	}
	// A few slow git calls are noise; more would leave too few claims to prove anything.
	if gitSlow > 2*calls/4 {
		t.Fatalf("git status ran out of the budget in %d of %d shell hooks", gitSlow, 2*calls)
	}
	// And the changes reached the hub as claims, rather than the hook answering without it.
	status, err := s.Call(context.Background(), "status", store.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if n := status["counts"].(map[string]int)["Claim"]; n < calls-gitSlow || n > calls {
		t.Fatalf("%d claims for %d changed files, %d slow git status", n, calls, gitSlow)
	}
	t.Logf("shell hooks os=%s hub=%s delay=%s n=%d slowest=%s slow git status=%d", runtime.GOOS, host, delay, 2*calls, slowest, gitSlow)
}

package hubclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoReportsEachFailureAndCheckPaths(t *testing.T) {
	if err := (*Client)(nil).Do(context.Background(), http.MethodGet, "/", nil, nil); err == nil {
		t.Fatal("nil client")
	}
	if (&Error{Code: "lost", Message: "gone"}).Error() != "lost: gone" {
		t.Fatal((&Error{Code: "lost", Message: "gone"}).Error())
	}
	broken := &Client{Config: staticConfig{url: "http://127.0.0.1:9", token: "t"}}
	broken.Config = tokenErr{}
	if err := broken.Do(context.Background(), http.MethodGet, "/v1/events", nil, nil); err == nil || !strings.Contains(err.Error(), "no token") {
		t.Fatal(err)
	}
	if err := (&Client{Config: staticConfig{url: "http://127.0.0.1:9"}}).Do(context.Background(), http.MethodPost, "/v1/sessions", make(chan int), nil); err == nil {
		t.Fatal("unmarshalable body")
	}
	if _, err := (&Client{Config: staticConfig{url: "http://127.0.0.1:9"}}).RegisterSession(context.Background(), SessionRequest{}); err == nil {
		t.Fatal("dial should fail")
	}
	if err := (&Client{}).EndExecution(context.Background(), "", 1); err == nil {
		t.Fatal("missing id")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sessions":
			_, _ = w.Write([]byte(`{"generation":1}`))
		case "/v1/claims/touch":
			_, _ = w.Write([]byte(`{}`))
		case "/v1/guard/check":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"message":"held","conflicts":[{"path":"a.go"}]}`))
		case "/v1/shell/reconcile":
			_, _ = w.Write([]byte(`{"message":""}`))
		case "/bad-json":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{`))
		case "/envelope":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"held"}}`))
		case "/plain":
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("nope"))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	c := &Client{Config: staticConfig{url: srv.URL, token: "tok"}}
	if _, err := c.RegisterSession(context.Background(), SessionRequest{SessionID: "s"}); err == nil || !strings.Contains(err.Error(), "execution id") {
		t.Fatal(err)
	}
	if _, err := c.Touch(context.Background(), TouchRequest{}); err == nil || !strings.Contains(err.Error(), "no action") {
		t.Fatal(err)
	}
	checked, err := c.CheckPaths(context.Background(), CheckRequest{Paths: []string{"a.go"}})
	if err != nil || checked.Message != "held" || len(checked.Conflicts) != 1 {
		t.Fatalf("%+v %v", checked, err)
	}
	reconciled, err := c.Reconcile(context.Background(), ReconcileRequest{Kind: "before"})
	if err != nil || reconciled.Message != "" {
		t.Fatal(err, reconciled)
	}
	if err = c.Do(context.Background(), http.MethodGet, "/bad-json", nil, &struct{}{}); err == nil {
		t.Fatal("bad json")
	}
	if err = c.Do(context.Background(), http.MethodGet, "/envelope", nil, nil); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatal(err)
	}
	if err = c.Do(context.Background(), http.MethodGet, "/plain", nil, nil); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatal(err)
	}
	if err = c.Do(context.Background(), http.MethodGet, "/empty", nil, nil); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	if err = c.Do(context.Background(), http.MethodGet, "/empty", nil, nil); err == nil {
		t.Fatal("closed server")
	}
}

type tokenErr struct{}

func (tokenErr) HubURL() string         { return "http://127.0.0.1:9" }
func (tokenErr) Token() (string, error) { return "", errors.New("no token") }

func TestValidateURLRejectsABlankHTTPSHost(t *testing.T) {
	if err := ValidateURL("https://"); err == nil {
		t.Fatal("https without host")
	}
	if err := ValidateURL("http://[::1"); err == nil {
		t.Fatal("broken url")
	}
	if err := ValidateURL("://nope"); err == nil {
		t.Fatal("scheme")
	}
}

func TestLoadAndWriteConfigFailures(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "hub.token")
	if err := os.WriteFile(filepath.Join(dir, "client.json"), bytesOf(65<<10), 0600); err != nil {
		t.Fatal(err)
	}
	e := EnvConfig{DefaultTokenFile: token, Getenv: func(string) string { return "" }, ReadFile: os.ReadFile}
	if loaded := e.Load(); loaded.err == nil || loaded.HubURL() != "" {
		t.Fatal(loaded.err)
	}
	e.ReadFile = func(string) ([]byte, error) { return nil, errors.New("unreadable") }
	if loaded := e.Load(); loaded.err == nil {
		t.Fatal("read error ignored")
	}
	if _, err := (EnvConfig{Getenv: func(string) string { return "" }}).TokenPath(); err != nil && os.Getenv("XDG_CONFIG_HOME") == "" && os.Getenv("HOME") == "" {
		// TokenPath fails only when no default path can be built.
	}
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	if _, err := (EnvConfig{}).TokenPath(); err == nil {
		t.Fatal("missing config dir")
	}
	if NewConfig().HubURL() != "" && os.Getenv("HOME") == "" {
		t.Fatal("new config hid the error")
	}

	parent := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parent, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := WriteConfig(filepath.Join(parent, "client.json"), FileConfig{Hub: "https://iris.example", TokenFile: "t"}); err == nil {
		t.Fatal("wrote through a file")
	}
	if err := WriteConfig(filepath.Join(dir, "client.json"), FileConfig{Hub: "http://example.com"}); err == nil {
		t.Fatal("cleartext accepted")
	}

	explicit := filepath.Join(dir, "missing-token")
	os.Remove(filepath.Join(dir, "client.json"))
	loaded := (EnvConfig{DefaultTokenFile: token, Getenv: func(k string) string {
		if k == "IRIS_TOKEN_FILE" {
			return explicit
		}
		return ""
	}, ReadFile: os.ReadFile}).Load()
	if _, err := loaded.Token(); err == nil {
		t.Fatal("missing explicit token")
	}
	if got := (EnvConfig{Getenv: os.Getenv, DefaultTokenFile: token, ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist }}).HubURL(); got != DefaultHubURL && os.Getenv("IRIS_HUB_URL") != "" {
		t.Fatal(got)
	}
}

func TestDoReadsABrokenBodyAndTouchReportsHubErrors(t *testing.T) {
	t.Setenv("IRIS_HUB_URL", "https://from-env.example")
	if got := (EnvConfig{}).HubURL(); got != "https://from-env.example" {
		t.Fatal(got)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusBadGateway)
	}))
	defer srv.Close()
	c := &Client{Config: staticConfig{url: srv.URL, token: "t"}}
	if _, err := c.Touch(context.Background(), TouchRequest{ExecutionID: "ex"}); err == nil {
		t.Fatal("touch ignored the hub")
	}
	if err := c.Do(context.Background(), "BAD METHOD", "/v1/events", map[string]int{}, nil); err == nil {
		t.Fatal("invalid method")
	}
	c.HTTP = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(boomReader{}), Header: make(http.Header), ContentLength: 1}, nil
	})}
	if err := c.Do(context.Background(), http.MethodGet, "/v1/events", nil, nil); err == nil || !strings.Contains(err.Error(), "cut") {
		t.Fatal(err)
	}

	locked := t.TempDir()
	if err := os.Chmod(locked, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0700) })
	if err := WriteConfig(filepath.Join(locked, "client.json"), FileConfig{Hub: "https://iris.example"}); err == nil {
		t.Fatal("created a config in a read-only directory")
	}

	origWrite, origClose := prepareConfigWrite, prepareConfigClose
	t.Cleanup(func() { prepareConfigWrite, prepareConfigClose = origWrite, origClose })
	dest := filepath.Join(t.TempDir(), "client.json")
	prepareConfigWrite = func(f *os.File) { f.Close() }
	if err := WriteConfig(dest, FileConfig{Hub: "https://iris.example", TokenFile: "t"}); err == nil {
		t.Fatal("write to a closed file succeeded")
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial config was renamed", err)
	}
	prepareConfigWrite = func(*os.File) {}
	prepareConfigClose = func(f *os.File) { f.Close() }
	if err := WriteConfig(dest, FileConfig{Hub: "https://iris.example", TokenFile: "t"}); err == nil {
		t.Fatal("closing twice succeeded")
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("config renamed after a failed close", err)
	}
}

type boomReader struct{}

func (boomReader) Read([]byte) (int, error) { return 0, errors.New("cut") }

type roundTrip func(*http.Request) (*http.Response, error)

func (r roundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return r(req) }

func bytesOf(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return b
}

package doctor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// trapHub is a server that fails the test if any request reaches it, and a
// client that delivers every dial to that server. A refused URL must not
// arrive here even though the client would otherwise carry it.
func trapHub(t *testing.T) *http.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("doctor contacted %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return &http.Client{Transport: &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) { return nil, nil },
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, srv.Listener.Addr().String())
		},
	}}
}

func runDoctor(t *testing.T, hubURL, dir string, hc *http.Client, getenv func(string) string) Result {
	t.Helper()
	layout := emptyLayout(t)
	if getenv != nil {
		layout.Getenv = getenv
	}
	token := filepath.Join(layout.Home, "hub.token")
	if err := os.WriteFile(token, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Options{
		Layout:    layout,
		HubURL:    hubURL,
		TokenFile: token,
		Dir:       dir,
		HTTP:      hc,
		Host:      "this-host",
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A non-loopback http URL, and anything else ValidateURL rejects, is reported
// and never contacted. The claim-scope line stays unknown for a checkout:
// doctor did not ask, and it does not assume repository or worktree.
func TestDoctorRefusedHubURLSendsNoRequest(t *testing.T) {
	repo := gitRepoFolding(t, "false")
	for _, tc := range []struct {
		name, hubURL, source string
		getenv               func(string) string
	}{
		{name: "command line cleartext", hubURL: "http://hub.example", source: "command line"},
		{
			name:   "environment cleartext",
			source: "environment (IRIS_HUB_URL)",
			getenv: func(key string) string {
				if key == "IRIS_HUB_URL" {
					return "http://192.0.2.10"
				}
				return ""
			},
		},
		{name: "localhost is not literal loopback", hubURL: "http://localhost:9", source: "command line"},
		{name: "unsupported scheme", hubURL: "ftp://hub.example", source: "command line"},
		{name: "https without a host", hubURL: "https://", source: "command line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.hubURL
			if raw == "" {
				raw = tc.getenv("IRIS_HUB_URL")
			}
			want := hubclient.ValidateURL(raw)
			if want == nil {
				t.Fatalf("fixture %s is a hub URL the probe accepts", raw)
			}
			res := runDoctor(t, tc.hubURL, repo, trapHub(t), tc.getenv)
			if res.HubUp || res.TokenOK || res.ExitCode() != 1 || res.HubSource != tc.source || res.HubDetail != want.Error() {
				t.Fatalf("source=%s detail=%s up=%v token=%v exit=%d", res.HubSource, res.HubDetail, res.HubUp, res.TokenOK, res.ExitCode())
			}
			if !strings.Contains(res.Format(), "hub: unreachable ("+want.Error()+")") {
				t.Fatal(res.Format())
			}
			if res.ClaimScope != claimScopeText("") || !strings.Contains(res.Format(), "claim auto scope: unknown.") {
				t.Fatalf("scope %q", res.ClaimScope)
			}
			if res.Stored || res.StoreError != "" {
				t.Fatalf("stored=%v %s", res.Stored, res.StoreError)
			}
		})
	}
}

// https and literal loopback http still reach an authenticated hub, and the
// scope the hub reports is shown.
func TestDoctorShowsClaimScopeForHTTPSAndLoopbackHTTP(t *testing.T) {
	repo := gitRepoFolding(t, "false")
	for _, tc := range []struct {
		name, scope string
		serve       func(http.Handler) *httptest.Server
	}{
		{name: "loopback http", scope: "repository", serve: httptest.NewServer},
		{name: "https", scope: "worktree", serve: httptest.NewTLSServer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var guardPosts int
			srv := tc.serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/status" && r.Header.Get("Authorization") != "Bearer secret" {
					t.Error("bearer mismatch")
				}
				switch r.URL.Path {
				case "/v1/status":
					if r.Header.Get("Authorization") != "" {
						t.Error("status probe carried a bearer")
					}
					w.WriteHeader(http.StatusNoContent)
				case "/v1/agents":
					w.WriteHeader(http.StatusNoContent)
				case "/v1/guard/check":
					if r.Method != http.MethodPost {
						t.Errorf("method %s", r.Method)
					}
					guardPosts++
					_, _ = fmt.Fprintf(w, `{"claim_auto_scope":%q}`, tc.scope)
				case "/v1/hooks/seen":
					_, _ = w.Write([]byte(`{"clients":{}}`))
				case "/v1/ui/doctor":
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				}
			}))
			defer srv.Close()
			parsed, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			switch tc.name {
			case "loopback http":
				if parsed.Scheme != "http" || (parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1") {
					t.Fatalf("not loopback http: %s", srv.URL)
				}
			case "https":
				if parsed.Scheme != "https" || parsed.Host == "" {
					t.Fatalf("not https: %s", srv.URL)
				}
			}
			if err := hubclient.ValidateURL(srv.URL); err != nil {
				t.Fatal(err)
			}
			res := runDoctor(t, srv.URL, repo, srv.Client(), nil)
			if !res.HubUp || !res.TokenOK || !res.Stored || res.ExitCode() != 0 {
				t.Fatalf("up=%v token=%v stored=%v exit=%d detail=%s tokenDetail=%s store=%s", res.HubUp, res.TokenOK, res.Stored, res.ExitCode(), res.HubDetail, res.TokenDetail, res.StoreError)
			}
			want := tc.scope + "."
			if tc.scope == "worktree" {
				want = "worktree claim scope is not enabled in this release"
			}
			if !strings.HasPrefix(res.ClaimScope, want) || !strings.Contains(res.Format(), "claim auto scope: "+want) {
				t.Fatalf("%q\n%s", res.ClaimScope, res.Format())
			}
			if tc.scope == "worktree" && !strings.Contains(res.Format(), "Automatic claims stay repository-wide.") {
				t.Fatal(res.Format())
			}
			if guardPosts != 1 {
				t.Fatalf("claim-scope lookups %d", guardPosts)
			}
		})
	}
}

// A probe that does not succeed does not look the scope up. The checkout is
// unknown, and a directory outside git still has no line.
func TestDoctorSkipsClaimScopeLookupWhenProbeFails(t *testing.T) {
	repo := gitRepoFolding(t, "false")
	t.Run("hub error", func(t *testing.T) {
		var paths []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			if r.URL.Path != "/v1/status" {
				t.Errorf("request beyond the probe: %s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		res := runDoctor(t, srv.URL, repo, srv.Client(), nil)
		if res.HubUp || res.TokenOK || res.ExitCode() != 1 || !strings.Contains(res.HubDetail, "hub status") {
			t.Fatalf("up=%v token=%v exit=%d detail=%s", res.HubUp, res.TokenOK, res.ExitCode(), res.HubDetail)
		}
		if res.ClaimScope != claimScopeText("") || len(paths) != 1 {
			t.Fatalf("scope %q paths %v", res.ClaimScope, paths)
		}
	})
	t.Run("rejected token", func(t *testing.T) {
		var paths []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			switch r.URL.Path {
			case "/v1/status":
				if r.Header.Get("Authorization") != "" {
					t.Error("status probe carried a bearer")
				}
				w.WriteHeader(http.StatusNoContent)
			case "/v1/agents":
				if r.Header.Get("Authorization") != "Bearer secret" {
					t.Error("bearer mismatch")
				}
				w.WriteHeader(http.StatusUnauthorized)
			default:
				t.Errorf("request beyond the probe: %s %s", r.Method, r.URL.Path)
			}
		}))
		defer srv.Close()
		res := runDoctor(t, srv.URL, repo, srv.Client(), nil)
		if !res.HubUp || res.TokenOK || res.ExitCode() != 1 || !strings.Contains(res.Format(), "token: not valid") {
			t.Fatalf("up=%v token=%v exit=%d\n%s", res.HubUp, res.TokenOK, res.ExitCode(), res.Format())
		}
		if res.ClaimScope != claimScopeText("") || len(paths) != 2 || paths[0] != "/v1/status" || paths[1] != "/v1/agents" {
			t.Fatalf("scope %q paths %v", res.ClaimScope, paths)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		var paths []string
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			paths = append(paths, r.URL.Path)
			if r.URL.Path != "/v1/status" {
				t.Errorf("request beyond the probe: %s", r.URL.Path)
			}
			return nil, errors.New("connection reset")
		})}
		res := runDoctor(t, "https://hub.example", repo, client, nil)
		if res.HubUp || res.TokenOK || res.ExitCode() != 1 || res.HubDetail != "hub unreachable" {
			t.Fatalf("up=%v token=%v exit=%d detail=%s", res.HubUp, res.TokenOK, res.ExitCode(), res.HubDetail)
		}
		if res.ClaimScope != claimScopeText("") || len(paths) != 1 {
			t.Fatalf("scope %q paths %v", res.ClaimScope, paths)
		}
	})
	t.Run("outside git", func(t *testing.T) {
		var paths []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			if r.URL.Path != "/v1/status" {
				t.Errorf("request beyond the probe: %s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		res := runDoctor(t, srv.URL, t.TempDir(), srv.Client(), nil)
		if res.ClaimScope != "" || strings.Contains(res.Format(), "claim auto scope") || len(paths) != 1 {
			t.Fatalf("scope %q paths %v\n%s", res.ClaimScope, paths, res.Format())
		}
	})
	t.Run("working directory", func(t *testing.T) {
		var paths []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			if r.URL.Path != "/v1/status" {
				t.Errorf("request beyond the probe: %s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		res := runDoctor(t, srv.URL, "", srv.Client(), nil)
		if _, err := gitinfo.Find("."); err != nil {
			t.Fatal(err)
		}
		if res.ClaimScope != claimScopeText("") || len(paths) != 1 {
			t.Fatalf("scope %q paths %v", res.ClaimScope, paths)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Every doctor call that carries a bearer goes through hubRequest, which
// refuses a URL the probe refuses before the client is used.
func TestBearerRequestsRefuseAnUnvalidatedURL(t *testing.T) {
	repo := gitRepoFolding(t, "false")
	folding := gitRepoFolding(t, "true")
	found, err := gitinfo.Find(folding)
	if err != nil {
		t.Fatal(err)
	}
	folds, ok := found.ConfigValue("core", "ignorecase")
	if !ok || !strings.EqualFold(folds, "true") {
		t.Fatalf("fixture ignorecase=%q present=%v", folds, ok)
	}
	for _, target := range []string{
		"http://hub.example/v1/guard/check",
		"http://localhost/v1/agents",
		"http://127.0.0.1.example/v1/ui/doctor",
		"http://[::ffff:127.0.0.1]/v1/agents",
		"https:///v1/hooks/seen",
		"ftp://hub.example/v1/agents",
	} {
		t.Run(target, func(t *testing.T) {
			_, err := hubRequest(context.Background(), http.MethodPost, target, "secret", strings.NewReader(`{}`), trapHub(t))
			want := hubclient.ValidateURL(target)
			if err == nil || want == nil || err.Error() != want.Error() {
				t.Fatalf("got %v, validate %v", err, want)
			}
		})
	}
	t.Run("claim scope", func(t *testing.T) {
		got := claimScopeLine(context.Background(), "http://hub.example", "secret", "h", repo, trapHub(t))
		if got != claimScopeText("") {
			t.Fatalf("%q", got)
		}
	})
	t.Run("case warning", func(t *testing.T) {
		if got := caseLine(context.Background(), "http://hub.example", "secret", "h", folding, trapHub(t)); got != "" {
			t.Fatalf("%q", got)
		}
	})
	t.Run("last hooks", func(t *testing.T) {
		if got := lastHooks(context.Background(), "http://hub.example", "secret", "h", trapHub(t)); got != nil {
			t.Fatalf("%v", got)
		}
	})
	t.Run("stored report", func(t *testing.T) {
		err := storeReport(context.Background(), "http://hub.example", "secret", Report{}, trapHub(t))
		want := hubclient.ValidateURL("http://hub.example/v1/ui/doctor")
		if err == nil || want == nil || err.Error() != want.Error() {
			t.Fatalf("got %v, validate %v", err, want)
		}
	})
	t.Run("agents probe", func(t *testing.T) {
		probe := probeHub(context.Background(), "http://hub.example", "secret", nil, trapHub(t))
		want := hubclient.ValidateURL("http://hub.example")
		if probe.up || probe.tokenOK || want == nil || probe.detail != want.Error() {
			t.Fatalf("%+v", probe)
		}
	})
	t.Run("loopback still sends", func(t *testing.T) {
		var hit bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/agents" {
				t.Errorf("path %s", r.URL.Path)
			}
			if r.Header.Get("Authorization") != "Bearer secret" {
				t.Error("bearer mismatch")
			}
			hit = true
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.Close()
		res, err := hubRequest(context.Background(), http.MethodGet, srv.URL+"/v1/agents", "secret", nil, srv.Client())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		if !hit || res.StatusCode != http.StatusNoContent {
			t.Fatalf("hit=%v status=%d", hit, res.StatusCode)
		}
	})
}

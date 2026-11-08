package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	oldSHA   = "1111111111111111111111111111111111111111"
	newSHA   = "2222222222222222222222222222222222222222"
	tagSHA   = "3333333333333333333333333333333333333333"
	oldImage = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	newImage = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// fakeRepo writes a small repository with every kind of pin refresh updates.
func fakeRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"Makefile": "GOLANGCI_LINT_VERSION ?= v2.15.0\nGOVULNCHECK_VERSION ?= v1.9.0\nGITLEAKS_VERSION ?= v8.30.1\n",
		".github/workflows/ci.yml": "steps:\n      - uses: actions/checkout@" + oldSHA + " # v4\n" +
			"      - uses: anchore/sbom-action/download-syft@" + oldSHA + " # v0.24.3\n" +
			"      - uses: actions/checkout@" + oldSHA + " # v4\n",
		"Dockerfile":            "FROM --platform=$BUILDPLATFORM busybox:1.38@" + oldImage + " AS data\nFROM gcr.io/distroless/static-debian12:nonroot@" + oldImage + "\n",
		"go.mod":                "module example\n\ngo 1.27\n\nrequire (\n\tgithub.com/coder/websocket v1.8.13\n\tgolang.org/x/sys v0.49.0 // indirect\n)\n",
		"web/package-lock.json": `{"packages":{"":{},"node_modules/react":{"version":"19.0.0"},"node_modules/vite":{"version":"7.0.0"},"node_modules/a/node_modules/b":{"version":"1.0.0"}}}`,
	}
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// fakeSources serves a module proxy, GitHub's API and two registries, one of which wants a token.
func fakeSources(t *testing.T) Sources {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	latest := map[string]string{"github.com/golangci/golangci-lint/v2": "v2.16.0", "golang.org/x/vuln": "v1.9.0", "github.com/zricethezav/gitleaks/v8": "v8.31.0"}
	mux.HandleFunc("/proxy/", func(w http.ResponseWriter, r *http.Request) {
		module := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/proxy/"), "/@latest")
		_, _ = w.Write([]byte(`{"Version":"` + latest[module] + `"}`))
	})
	mux.HandleFunc("/gh/repos/actions/checkout/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gh-token" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"tag_name":"v7.0.1"}`))
	})
	mux.HandleFunc("/gh/repos/actions/checkout/git/ref/tags/v7.0.1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":{"type":"tag","sha":"` + tagSHA + `"}}`))
	})
	mux.HandleFunc("/gh/repos/actions/checkout/git/tags/"+tagSHA, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":{"type":"commit","sha":"` + newSHA + `"}}`))
	})
	mux.HandleFunc("/gh/repos/anchore/sbom-action/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v0.24.3"}`))
	})
	mux.HandleFunc("/gh/repos/anchore/sbom-action/git/ref/tags/v0.24.3", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":{"type":"commit","sha":"` + oldSHA + `"}}`))
	})
	// Docker Hub's shape: a 401 challenge, then a token, then the digest.
	mux.HandleFunc("/hub/v2/library/busybox/manifests/1.38", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer anon" {
			w.Header().Set("Www-Authenticate", `Bearer realm="`+srv.URL+`/token",service="registry.docker.io",scope="repository:library/busybox:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Docker-Content-Digest", newImage)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("scope") != "repository:library/busybox:pull" {
			http.Error(w, "scope", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"token":"anon"}`))
	})
	mux.HandleFunc("/gcr/v2/distroless/static-debian12/manifests/nonroot", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", oldImage)
	})
	return Sources{
		GitHub:  srv.URL + "/gh",
		GoProxy: srv.URL + "/proxy",
		Registry: func(host string) string {
			if host == "gcr.io" {
				return srv.URL + "/gcr"
			}
			return srv.URL + "/hub"
		},
		Token: "gh-token",
		HTTP:  srv.Client(),
	}
}

// fakeRunner stands in for go, npm and make: go and npm move one version each, make passes.
func fakeRunner(t *testing.T, root string, fail string) (Runner, *[]string) {
	t.Helper()
	var calls []string
	return func(dir, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		if call == fail {
			return "line 1\nline 2\n", errors.New("exit status 1")
		}
		switch call {
		case "go get -u ./...":
			p := filepath.Join(root, "go.mod")
			raw, _ := os.ReadFile(p)
			raw = bytes.Replace(raw, []byte("websocket v1.8.13"), []byte("websocket v1.8.14"), 1)
			_ = os.WriteFile(p, raw, 0o644)
		case "npm update":
			_ = os.WriteFile(filepath.Join(dir, "package-lock.json"), []byte(`{"packages":{"node_modules/react":{"version":"19.1.0"},"node_modules/zod":{"version":"4.0.0"}}}`), 0o644)
		}
		return "", nil
	}, &calls
}

func read(t *testing.T, root, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A refresh bumps every kind of pin to what the sources say is newest, keeps what is current,
// runs the checks and commits nothing.
func TestRefreshUpdatesEveryPinAndRunsTheChecks(t *testing.T) {
	root := fakeRepo(t)
	runner, calls := fakeRunner(t, root, "")
	report := Refresh(root, fakeSources(t), runner)
	if len(report.Problems) != 0 {
		t.Fatalf("problems: %v", report.Problems)
	}
	if mk := read(t, root, "Makefile"); !strings.Contains(mk, "GOLANGCI_LINT_VERSION ?= v2.16.0") || !strings.Contains(mk, "GOVULNCHECK_VERSION ?= v1.9.0") || !strings.Contains(mk, "GITLEAKS_VERSION ?= v8.31.0") {
		t.Fatalf("Makefile:\n%s", mk)
	}
	ci := read(t, root, ".github/workflows/ci.yml")
	if strings.Count(ci, "actions/checkout@"+newSHA+" # v7.0.1") != 2 || !strings.Contains(ci, "anchore/sbom-action/download-syft@"+oldSHA+" # v0.24.3") {
		t.Fatalf("workflow:\n%s", ci)
	}
	if df := read(t, root, "Dockerfile"); !strings.Contains(df, "FROM --platform=$BUILDPLATFORM busybox:1.38@"+newImage+" AS data") || !strings.Contains(df, "static-debian12:nonroot@"+oldImage) {
		t.Fatalf("Dockerfile:\n%s", df)
	}
	want := []string{"go get -u ./...", "go mod tidy", "npm update", "make test", "make check-all"}
	if strings.Join(*calls, "|") != strings.Join(want, "|") {
		t.Fatalf("ran %v", *calls)
	}
	summary := strings.Join(report.Changes, "\n")
	for _, line := range []string{
		"Makefile GOLANGCI_LINT_VERSION v2.15.0 -> v2.16.0",
		"ci.yml actions/checkout -> v7.0.1 (222222222222)",
		"Dockerfile busybox:1.38 sha256:aaaaaaaaaaaa -> sha256:bbbbbbbbbbbb",
		"go.mod github.com/coder/websocket v1.8.13 -> v1.8.14",
		"web react 19.0.0 -> 19.1.0",
		"web vite 7.0.0 removed",
		"web zod added at 4.0.0",
	} {
		if !strings.Contains(summary, line) {
			t.Errorf("summary lacks %q:\n%s", line, summary)
		}
	}
	if strings.Count(summary, "actions/checkout") != 1 {
		t.Errorf("one workflow file reports one change per action:\n%s", summary)
	}
	var out bytes.Buffer
	if code := Print(&out, report); code != 0 || !strings.Contains(out.String(), "Nothing was committed") || !strings.Contains(out.String(), "make check-all: ok") {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
}

// A source that cannot be reached is reported and fails the run; it is never read as "nothing
// newer". The pins it could not check stay as they were.
func TestUnreachableSourcesAreReportedNotSwallowed(t *testing.T) {
	root := fakeRepo(t)
	runner, _ := fakeRunner(t, root, "")
	src := Sources{GitHub: "http://127.0.0.1:9", GoProxy: "http://127.0.0.1:9", Registry: func(string) string { return "http://127.0.0.1:9" }, HTTP: http.DefaultClient}
	report := Refresh(root, src, runner)
	problems := strings.Join(report.Problems, "\n")
	for _, who := range []string{"GOLANGCI_LINT_VERSION", "actions/checkout", "anchore/sbom-action", "busybox:1.38", "gcr.io/distroless/static-debian12:nonroot"} {
		if !strings.Contains(problems, who) {
			t.Errorf("no problem reported for %s:\n%s", who, problems)
		}
	}
	if strings.Count(problems, "actions/checkout: ") != 1 {
		t.Errorf("a repository is looked up and reported once:\n%s", problems)
	}
	if !strings.Contains(read(t, root, "Makefile"), "GOLANGCI_LINT_VERSION ?= v2.15.0") {
		t.Fatal("an unchecked pin changed")
	}
	var out bytes.Buffer
	if code := Print(&out, report); code != 1 {
		t.Fatalf("exit %d", code)
	}
}

// A failing step is reported with the end of its output, and a failed module update stops
// before go mod tidy runs on a half-updated go.mod.
func TestAFailingStepIsReportedWithItsOutput(t *testing.T) {
	for _, step := range []string{"go get -u ./...", "npm update", "make check-all"} {
		root := fakeRepo(t)
		runner, calls := fakeRunner(t, root, step)
		report := Refresh(root, fakeSources(t), runner)
		problems := strings.Join(report.Problems, "\n")
		if !strings.Contains(problems, step+" failed: exit status 1\nline 1\nline 2") {
			t.Errorf("%s: %v", step, report.Problems)
		}
		if step == "go get -u ./..." && strings.Contains(strings.Join(*calls, "|"), "go mod tidy") {
			t.Error("go mod tidy ran after go get failed")
		}
	}
}

// Registry, GitHub and token edge cases each name what went wrong.
func TestLookupFailuresSayWhy(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/gh/repos/o/notag/releases/latest", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"tag_name":"v1"}`)) })
	mux.HandleFunc("/gh/repos/o/notag/git/ref/tags/v1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":{"type":"commit","sha":"short"}}`))
	})
	mux.HandleFunc("/gh/repos/o/badtag/releases/latest", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"tag_name":"v1"}`)) })
	mux.HandleFunc("/gh/repos/o/badtag/git/ref/tags/v1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":{"type":"tag","sha":"` + tagSHA + `"}}`))
	})
	mux.HandleFunc("/gh/repos/o/noref/releases/latest", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"tag_name":"v1"}`)) })
	// A tag object that points at itself.
	mux.HandleFunc("/gh/repos/o/loop/releases/latest", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"tag_name":"v1"}`)) })
	mux.HandleFunc("/gh/repos/o/loop/git/ref/tags/v1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":{"type":"tag","sha":"` + tagSHA + `"}}`))
	})
	mux.HandleFunc("/gh/repos/o/loop/git/tags/"+tagSHA, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":{"type":"tag","sha":"` + tagSHA + `"}}`))
	})
	mux.HandleFunc("/r/v2/nochallenge/manifests/t", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	mux.HandleFunc("/r/v2/notoken/manifests/t", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Www-Authenticate", `Bearer realm="`+srv.URL+`/empty",service="s",scope="x"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/r/v2/deadtoken/manifests/t", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Www-Authenticate", `Bearer realm="http://127.0.0.1:9/token"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/empty", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) })
	mux.HandleFunc("/access", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"access_token":"a"}`)) })
	// A realm that already has a query keeps it, and service and scope are added to it.
	mux.HandleFunc("/realm", func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); q.Get("v") != "2" || q.Get("service") != "s" || q.Get("scope") != "repo:x pull" {
			http.Error(w, r.URL.RawQuery, http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"token":"q"}`))
	})
	mux.HandleFunc("/r/v2/query/manifests/t", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer q" {
			w.Header().Set("Docker-Content-Digest", newImage)
			return
		}
		w.Header().Set("Www-Authenticate", `Bearer realm="`+srv.URL+`/realm?v=2",service="s",scope="repo:x pull"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/r/v2/badrealm/manifests/t", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Www-Authenticate", `Bearer realm="://bad"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/r/v2/access/manifests/t", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer a" {
			w.Header().Set("Docker-Content-Digest", newImage)
			return
		}
		w.Header().Set("Www-Authenticate", `Bearer realm="`+srv.URL+`/access"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/r/v2/nodigest/manifests/t", func(w http.ResponseWriter, r *http.Request) {})
	src := Sources{GitHub: srv.URL + "/gh", Registry: func(string) string { return srv.URL + "/r" }, HTTP: srv.Client()}
	for repo, want := range map[string]string{"o/notag": "names no commit", "o/badtag": "404", "o/noref": "404", "o/loop": "within 5 tag objects"} {
		if _, _, err := src.release(repo); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", repo, err)
		}
	}
	for image, want := range map[string]string{"r.io/nochallenge": "credentials", "r.io/notoken": "no token", "r.io/deadtoken": "refused", "r.io/nodigest": "without a digest", "r.io/badrealm": "bad token realm"} {
		if _, err := src.digest(image, "t"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", image, err)
		}
	}
	if d, err := src.digest("r.io/access", "t"); err != nil || d != newImage {
		t.Errorf("access_token: %s %v", d, err)
	}
	if d, err := src.digest("r.io/query", "t"); err != nil || d != newImage {
		t.Errorf("realm with a query: %s %v", d, err)
	}
	if err := src.getJSON("://bad", false, nil); err == nil {
		t.Error("a malformed URL was fetched")
	}
	if _, err := src.manifest("://bad", ""); err == nil {
		t.Error("a malformed manifest URL was fetched")
	}
	if _, err := src.digest("bad image", "t"); err == nil {
		t.Error("an image name with a space reached the registry")
	}
}

// A file refresh cannot read or write is reported, not skipped.
func TestUnwritableFilesAreReported(t *testing.T) {
	var r Report
	r.edit(filepath.Join(t.TempDir(), "absent"), func(s string) string { return s })
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.WriteFile(locked, []byte("a"), 0o444); err != nil {
		t.Fatal(err)
	}
	r.edit(locked, func(s string) string { return s + "x" })
	if len(r.Problems) != 2 {
		t.Fatalf("%v", r.Problems)
	}
}

func TestSmallParts(t *testing.T) {
	for goproxy, want := range map[string]string{"": "https://proxy.golang.org", "direct": "https://proxy.golang.org", "https://p.example/,direct": "https://p.example", "off|https://q.example/": "https://q.example"} {
		if got := firstProxy(goproxy); got != want {
			t.Errorf("firstProxy(%q) = %q", goproxy, got)
		}
	}
	if got := escapeModule("github.com/BurntSushi/toml"); got != "github.com/!burnt!sushi/toml" {
		t.Error(got)
	}
	long := strings.Repeat("x\n", 30)
	if got := tail(long); strings.Count(got, "\n") != 19 {
		t.Errorf("tail kept %d lines", strings.Count(got, "\n")+1)
	}
	if out, err := run(t.TempDir(), "go", "version"); err != nil || !strings.HasPrefix(out, "go version") {
		t.Errorf("run: %q %v", out, err)
	}
	var out bytes.Buffer
	Print(&out, Report{})
	if !strings.Contains(out.String(), "Changed:\n  none") {
		t.Error(out.String())
	}
}

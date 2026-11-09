package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

type closedBundle struct{}

func (closedBundle) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }
func (closedBundle) Sub(string) (fs.FS, error)    { return nil, fs.ErrNotExist }

func TestHandlerServesTheShellAndFallsBack(t *testing.T) {
	handler := handlerFrom(fstest.MapFS{"dist/index.html": {Data: []byte("<title>Iris</title>")}})

	index := httptest.NewRecorder()
	handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/", nil))
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), "Iris") {
		t.Fatalf("index: %d %q", index.Code, index.Body.String())
	}

	fallback := httptest.NewRecorder()
	handler.ServeHTTP(fallback, httptest.NewRequest(http.MethodGet, "/threads/ms_1", nil))
	if fallback.Code != http.StatusOK || !strings.Contains(fallback.Body.String(), "Iris") {
		t.Fatalf("fallback: %d %q", fallback.Code, fallback.Body.String())
	}

	head := httptest.NewRecorder()
	handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/threads/ms_1", nil))
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("head fallback: %d body %d", head.Code, head.Body.Len())
	}

	method := httptest.NewRecorder()
	handler.ServeHTTP(method, httptest.NewRequest(http.MethodPost, "/", nil))
	if method.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post: %d", method.Code)
	}
	if method.Header().Get("Allow") != "GET, HEAD" {
		t.Fatal(method.Header().Get("Allow"))
	}
}

func TestEmbeddedConsolePresenceMatchesWhatIsServed(t *testing.T) {
	_, err := fs.Stat(dist, "dist/index.html")
	if Available() != (err == nil) {
		t.Fatalf("console availability does not match embedded index: %v", err)
	}
	want := http.StatusServiceUnavailable
	if Available() {
		want = http.StatusOK
	}
	r := httptest.NewRecorder()
	Handler().ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/", nil))
	if r.Code != want || !strings.Contains(r.Body.String(), "Iris") {
		t.Fatalf("embedded console: %d %q", r.Code, r.Body.String())
	}
}

func TestHandlerServesAnEmbeddedAssetAndMissesAShell(t *testing.T) {
	bundle := fstest.MapFS{
		"dist/app.js":     {Data: []byte("var iris = 1\n")},
		"dist/index.html": {Data: []byte("<title>Iris</title>")},
	}
	h := handlerFrom(bundle)
	asset := httptest.NewRecorder()
	h.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/app.js", nil))
	if asset.Code != http.StatusOK || !strings.Contains(asset.Body.String(), "var iris = 1") {
		t.Fatalf("asset: %d %q", asset.Code, asset.Body.String())
	}
	head := httptest.NewRecorder()
	h.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/app.js", nil))
	if head.Code != http.StatusOK {
		t.Fatal(head.Code)
	}

	shell := handlerFrom(fstest.MapFS{"dist/app.js": {Data: []byte("x")}})
	missing := httptest.NewRecorder()
	shell.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/threads/ms_1", nil))
	if missing.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing shell: %d", missing.Code)
	}
}

func TestMissingConsoleIsExplicitEvenWithStaleAssets(t *testing.T) {
	h := handlerFrom(fstest.MapFS{"dist/assets/old.js": {Data: []byte("stale console")}})
	for _, path := range []string{"/", "/threads/ms_1", "/assets/old.js"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			r := httptest.NewRecorder()
			h.ServeHTTP(r, httptest.NewRequest(method, path, nil))
			if r.Code != http.StatusServiceUnavailable {
				t.Fatalf("%s %s: %d %q", method, path, r.Code, r.Body.String())
			}
			if method == http.MethodHead {
				if r.Body.Len() != 0 {
					t.Fatalf("HEAD %s: %q", path, r.Body.String())
				}
			} else if !strings.Contains(r.Body.String(), "Iris console is missing") || !strings.Contains(r.Body.String(), "make build") || strings.Contains(r.Body.String(), "stale console") {
				t.Fatalf("GET %s: %q", path, r.Body.String())
			}
		}
	}
}

func TestHandlerFromPanicsWithoutDist(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("bundle without dist did not panic")
		}
	}()
	handlerFrom(closedBundle{})
}

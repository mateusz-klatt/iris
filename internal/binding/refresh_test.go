package binding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/store"
)

func TestBindingRefreshFailureRetainsRecoveryProof(t *testing.T) {
	h := &scriptedHub{binds: []string{"", "denied", ""}}
	r := resolverFor(t, h, "codex", map[string]string{"IRIS_EXECUTION": "ex_bound"}, proc.Ancestor{})
	before, err := r.Resolve(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(context.Background(), ""); !IsCode(err, "denied") || r.bound != before || r.Lost() {
		t.Fatalf("refresh failure discarded the binding or hid the refusal: bound %+v, lost %v, err %v", r.bound, r.Lost(), err)
	}
	if after, err := r.Resolve(context.Background(), ""); err != nil || after != before {
		t.Fatalf("refresh could not retry: %+v %v", after, err)
	}
}

func TestCachedBindingReattachesAfterHubRestart(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "hub.db")
	s, err := store.Open(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var serving atomic.Pointer[hub.Hub]
	heartbeat := make(chan struct{}, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		serving.Load().ServeHTTP(w, req)
		if req.URL.Path != "/v1/bind" {
			select {
			case heartbeat <- struct{}{}:
			default:
			}
		}
	}))
	serving.Store(hub.New(s, "secret", []string{srv.Listener.Addr().String()}))
	srv.Start()
	defer srv.Close()
	registration, err := s.Call(ctx, "sessions", store.Request{Client: "codex", Host: "host", SessionID: "running", Cwd: "/repo"})
	if err != nil {
		t.Fatal(err)
	}
	r := &Resolver{Hub: &hubclient.Client{Config: config{srv.URL}}, Client: "codex", Host: "host", CWD: "/repo", Getenv: func(key string) string {
		if key == "IRIS_EXECUTION" {
			return registration["execution_id"].(string)
		}
		return ""
	}}
	before, err := r.Resolve(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	serving.Store(hub.New(s, "secret", []string{srv.Listener.Addr().String()}))
	// A plain bind cannot grant authority to a recovering session. Its cached
	// generation must survive the refusal so the next heartbeat can reattach it.
	if _, err := r.Resolve(ctx, ""); !IsCode(err, "lost") || r.bound != before || r.Lost() {
		t.Fatalf("recovery refresh: bound %+v, lost %v, err %v", r.bound, r.Lost(), err)
	}
	run, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		r.Maintain(run, "shim", time.Millisecond, func() bool { return true }, func() { t.Error("recovery stopped the client") })
		close(done)
	}()
	select {
	case <-heartbeat:
	case <-time.After(5 * time.Second):
		stop()
		<-done
		t.Fatal("the recovering session never sent its proof")
	}
	stop()
	<-done
	if after, err := r.Resolve(ctx, ""); err != nil || after != before || r.Lost() {
		t.Fatalf("the preserved generation did not reattach: %+v %v lost %v", after, err, r.Lost())
	}
}

func TestLegacyLeaseOnlyHeartbeatKeepsBindingAndBindRefreshesIt(t *testing.T) {
	before := Bound{ExecutionID: "ex_running", Generation: 7, RepositoryID: "rp_old", Agent: hubclient.Agent{ID: "ag_mail", Name: "codex-host-repo"}, Rung: 3}
	current := before
	current.RepositoryID = "rp_current"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/bind" {
			// Old bind responses contain authority, but never the local evidence rung.
			_ = json.NewEncoder(w).Encode(map[string]any{"execution_id": current.ExecutionID, "generation": current.Generation, "repository_id": current.RepositoryID, "agent": current.Agent})
			return
		}
		_, _ = w.Write([]byte(`{"lease_ms":60000}`))
	}))
	defer srv.Close()
	r := &Resolver{Hub: &hubclient.Client{Config: config{srv.URL}}, bound: before, Client: "codex", Host: "host"}
	ticks := 0
	r.Maintain(context.Background(), "shim", time.Millisecond, func() bool { ticks++; return ticks == 1 }, func() {})
	if r.bound != before || r.Lost() {
		t.Fatalf("legacy lease reply erased the cached identity: %+v", r.bound)
	}
	if after, err := r.Resolve(context.Background(), ""); err != nil || after != current {
		t.Fatalf("legacy bind did not refresh repository authority: %+v %v", after, err)
	}
}

func TestInflightHeartbeatDoesNotOverwriteANewHandle(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	before := Bound{ExecutionID: "ex_old", Generation: 1, RepositoryID: "rp_old", Rung: 4}
	selected := Bound{ExecutionID: "ex_selected", Generation: 2, RepositoryID: "rp_selected", Rung: 4}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/bind" {
			_ = json.NewEncoder(w).Encode(selected)
			return
		}
		close(started)
		<-release
		fresh := before
		fresh.RepositoryID = "rp_merged"
		_ = json.NewEncoder(w).Encode(fresh)
	}))
	defer srv.Close()
	r := &Resolver{Hub: &hubclient.Client{Config: config{srv.URL}}, bound: before, Getenv: func(string) string { return "" }}
	done := make(chan struct{})
	go func() {
		ticks := 0
		r.Maintain(context.Background(), "shim", time.Millisecond, func() bool { ticks++; return ticks == 1 }, func() {})
		close(done)
	}()
	<-started
	after, err := r.Resolve(context.Background(), selected.ExecutionID)
	close(release)
	<-done
	if err != nil || after != selected || r.bound != selected {
		t.Fatalf("an older in-flight heartbeat replaced the selected binding: %+v %+v %v", after, r.bound, err)
	}
}

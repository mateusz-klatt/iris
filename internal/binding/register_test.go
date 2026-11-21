package binding

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

// An Execution of its own names the repository it runs in, as a hook's does, so its mail
// and claims land in the right place; and a hub that refuses it is said, not swallowed.
func TestAnOwnExecutionNamesItsRepositoryAndAHubRefusalIsSaid(t *testing.T) {
	hub := &scriptedHub{binds: []string{"unbound"}}
	r := resolverFor(t, hub, "grok", map[string]string{"GROK_SESSION_ID": "s2"}, proc.Ancestor{})
	if out, err := exec.Command("git", "-C", r.CWD, "init", "-q", "-b", "trunk").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if _, err := r.Resolve(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if hub.session.Worktree == "" || hub.session.CommonDir == "" || hub.session.Branch != "trunk" {
		t.Fatalf("%+v", hub.session)
	}
	refusing := &scriptedHub{binds: []string{"unbound"}, register: "denied"}
	if _, err := resolverFor(t, refusing, "copilot", map[string]string{"COPILOT_AGENT_SESSION_ID": "s3"}, proc.Ancestor{}).Resolve(context.Background(), ""); !IsCode(err, "denied") {
		t.Fatal(err)
	}
}

// The grace period ends with the call that waits in it: a caller that gives up is told Iris is
// still binding and to retry, and nothing is registered.
func TestTheGracePeriodEndsWithItsCaller(t *testing.T) {
	for _, ending := range []string{"cancel", "deadline"} {
		t.Run(ending, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				hub := &scriptedHub{binds: []string{"unbound", "unbound", "unbound", "unbound", "unbound"}}
				var hubMu sync.Mutex
				// Keep the refusal inside the virtual-time bubble. A real HTTP
				// handshake can consume the caller's deadline before grace begins.
				r := &Resolver{
					Hub: &hubclient.Client{Config: config{"http://127.0.0.1"}, HTTP: &http.Client{Transport: adoptionTransport(func(req *http.Request) (*http.Response, error) {
						hubMu.Lock()
						defer hubMu.Unlock()
						w := httptest.NewRecorder()
						hub.ServeHTTP(w, req)
						return w.Result(), nil
					})}},
					Client: "claude", Grace: time.Minute,
					Getenv: func(key string) string {
						if key == "CLAUDE_CODE_SESSION_ID" {
							return "s"
						}
						return ""
					},
				}
				var ctx context.Context
				var cancel context.CancelFunc
				if ending == "deadline" {
					ctx, cancel = context.WithTimeout(context.Background(), 150*time.Millisecond)
				} else {
					ctx, cancel = context.WithCancel(context.Background())
				}
				defer cancel()
				done := make(chan error, 1)
				go func() {
					_, err := r.Resolve(ctx, "")
					done <- err
				}()
				synctest.Wait()
				hubMu.Lock()
				binds := len(hub.bound)
				hubMu.Unlock()
				if binds != 1 {
					t.Fatalf("grace must wait after the initial refusal: %d binds", binds)
				}
				select {
				case err := <-done:
					t.Fatalf("grace ended before its caller: %v registered %+v", err, hub.session)
				default:
				}
				if ending == "deadline" {
					<-ctx.Done()
				} else {
					cancel()
				}
				err := <-done
				if !IsCode(err, "unbound") || !strings.Contains(err.Error(), "still binding this session; retry in a few seconds") || hub.session != nil {
					t.Fatalf("%v registered %+v", err, hub.session)
				}
			})
		})
	}
}

// With no way to look up the client process, that rung is skipped rather than failing.
func TestNoProcessLookupSkipsThatRung(t *testing.T) {
	hub := &scriptedHub{binds: []string{"unbound"}}
	r := resolverFor(t, hub, "codex", nil, proc.Ancestor{})
	r.Ancestor = nil
	if _, err := r.Resolve(context.Background(), ""); !IsCode(err, "unbound") || len(hub.bound) != 0 {
		t.Fatalf("%v, binds %v", err, hub.bound)
	}
}

// Until a hook registers, the heartbeat keeps this process's own Execution: giving it up
// with nothing to move to would leave the session without one.
func TestAnOwnExecutionIsKeptUntilTheHooksAppears(t *testing.T) {
	hub := &scriptedHub{binds: []string{"unbound", "unbound"}}
	r := resolverFor(t, hub, "claude", map[string]string{"CLAUDE_CODE_SESSION_ID": "s"}, proc.Ancestor{})
	mine, err := r.Resolve(context.Background(), "")
	if err != nil || !r.own {
		t.Fatalf("%+v %v", mine, err)
	}
	r.adopt(context.Background())
	if !r.own || r.bound != mine {
		t.Fatalf("gave up %+v for nothing: %+v", mine, r.bound)
	}
}

// A directory with no .git above it registers on the outside route; a hub without that route
// takes the same request on the session route and is said once; and a checkout Iris cannot read
// registers nothing (ADR 034).
func TestAnOwnExecutionOutsideGitRegistersOnItsRoute(t *testing.T) {
	hub := &scriptedHub{binds: []string{"unbound"}}
	r := resolverFor(t, hub, "grok", map[string]string{"GROK_SESSION_ID": "s"}, proc.Ancestor{})
	if _, err := r.Resolve(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if len(hub.routes) != 1 || hub.routes[0] != "/v1/sessions/outside" || hub.session.Worktree != "" || hub.session.CommonDir != "" {
		t.Fatalf("%v %+v", hub.routes, hub.session)
	}
	older := &scriptedHub{binds: []string{"unbound", "unbound", "unbound"}, older: true, register: "denied"}
	var said []string
	r = resolverFor(t, older, "grok", map[string]string{"GROK_SESSION_ID": "s"}, proc.Ancestor{})
	r.Observe = func(text string) { said = append(said, text) }
	// A legacy registration the hub refuses is not reported as made.
	if _, err := r.Resolve(context.Background(), ""); !IsCode(err, "denied") || len(said) != 0 {
		t.Fatalf("%v, said %v", err, said)
	}
	older.register = ""
	older.routes = nil
	if _, err := r.Resolve(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	r.bound = Bound{}
	if _, err := r.Resolve(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if len(older.routes) != 2 || older.routes[0] != "/v1/sessions" || len(said) != 1 || said[0] != ObservedOlderHub {
		t.Fatalf("routes %v, said %v", older.routes, said)
	}
	unreadable := &scriptedHub{binds: []string{"unbound"}}
	r = resolverFor(t, unreadable, "grok", map[string]string{"GROK_SESSION_ID": "s"}, proc.Ancestor{})
	if err := os.WriteFile(filepath.Join(r.CWD, ".git"), []byte("not a link\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(context.Background(), ""); !errors.Is(err, gitinfo.ErrUnreadable) || len(unreadable.routes) != 0 {
		t.Fatalf("%v %v", err, unreadable.routes)
	}
}

// A binding of an Execution outside git says so; a refresh from a hub that omits it, as an older
// one does, clears it.
func TestTheOutsideFlagFollowsTheHub(t *testing.T) {
	var outside bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answer := map[string]any{"execution_id": "ex", "generation": 1}
		if outside {
			answer["outside_git"] = true
		}
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(srv.Close)
	r := &Resolver{Hub: &hubclient.Client{Config: config{srv.URL}}, Client: "codex", Host: "host", bound: Bound{ExecutionID: "ex", Generation: 1}}
	outside = true
	if b, err := r.refresh(context.Background()); err != nil || !b.Outside {
		t.Fatalf("%+v %v", b, err)
	}
	outside = false
	if b, err := r.refresh(context.Background()); err != nil || b.Outside {
		t.Fatalf("%+v %v", b, err)
	}
}

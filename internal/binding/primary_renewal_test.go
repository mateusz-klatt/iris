package binding

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/proc"
)

func TestRenewPrimaryRefusesReplacementBeforeOrDuringHeartbeat(t *testing.T) {
	for _, stage := range []string{"before dispatch", "in flight"} {
		t.Run(stage, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			h := &tickHub{reply: func(req *http.Request, body map[string]any) (int, any) {
				switch req.URL.Path {
				case "/v1/bind":
					return found(body["execution"].(string), 1, "")
				case "/v1/executions/ex_first/heartbeat":
					close(entered)
					select {
					case <-release:
						return leaseGranted()
					case <-req.Context().Done():
						return 0, nil
					}
				case "/v1/executions/ex_second/heartbeat":
					return leaseGranted()
				default:
					t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
					return refused("not_found")
				}
			}}
			r := tickResolver(h.client(), "claude", nil, proc.Ancestor{})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			resolve := func(execution string) Bound {
				t.Helper()
				b, err := r.Resolve(ctx, execution)
				if err != nil || b.ExecutionID != execution {
					t.Fatalf("resolve %s: %+v %v", execution, b, err)
				}
				return b
			}
			requireLost := func(b Bound, err error) {
				t.Helper()
				if b != (Bound{}) || !IsCode(err, "lost") || r.Lost() || len(r.gone) != 0 {
					t.Fatalf("stale renewal became usable or confirmed loss: %+v %v", b, err)
				}
			}
			original := resolve("ex_first")
			var replacement Bound
			if stage == "in flight" {
				type result struct {
					b   Bound
					err error
				}
				out := make(chan result, 1)
				go func() {
					b, err := r.RenewPrimary(ctx, original)
					out <- result{b, err}
				}()
				select {
				case <-entered:
				case first := <-out:
					t.Fatalf("renewal did not reach hub: %+v", first)
				case <-ctx.Done():
					t.Fatal("renewal did not dispatch")
				}
				replacement = resolve("ex_second")
				close(release)
				select {
				case first := <-out:
					requireLost(first.b, first.err)
				case <-ctx.Done():
					t.Fatal("renewal did not return")
				}
			} else {
				replacement = resolve("ex_second")
				requireLost(r.RenewPrimary(ctx, original))
			}
			if b, err := r.RenewPrimary(ctx, replacement); err != nil || b != replacement {
				t.Fatalf("stale reply disrupted replacement: %+v %v", b, err)
			}
			if stage == "in flight" {
				h.expect(t, "bind execution=ex_first", "beat ex_first 1 shim", "bind execution=ex_second", "beat ex_second 1 shim")
			} else {
				h.expect(t, "bind execution=ex_first", "bind execution=ex_second", "beat ex_second 1 shim")
			}
		})
	}
}

func TestRenewPrimaryRejectsConflictingHeartbeatPair(t *testing.T) {
	conflict := true
	h := &tickHub{reply: func(req *http.Request, _ map[string]any) (int, any) {
		switch {
		case req.URL.Path == "/v1/bind":
			return found("ex_current", 2, "S")
		case conflict:
			return found("ex_current", 3, "S")
		default:
			return leaseGranted()
		}
	}}
	r := tickResolver(h.client(), "claude", sessionS, proc.Ancestor{})
	r.SuccessionEnabled = func(context.Context) bool { return true }
	b, err := r.Resolve(t.Context(), "")
	if err != nil || b.ExecutionID != "ex_current" || b.Generation != 2 {
		t.Fatalf("resolve admitted pair: %+v %v", b, err)
	}
	if stale, err := r.RenewPrimary(t.Context(), b); stale != (Bound{}) || !IsCode(err, "lost") || r.Lost() || len(r.gone) != 0 {
		t.Fatalf("conflicting reply replaced or excluded cache: %+v %v gone=%v", stale, err, r.gone)
	}
	conflict = false
	if fresh, err := r.RenewPrimary(t.Context(), b); fresh != b || err != nil {
		t.Fatalf("lease-only reply could not renew the original pair: %+v %v", fresh, err)
	}
	h.expect(t, "bind session_id=S", "beat ex_current 2 shim", "beat ex_current 2 shim")
}

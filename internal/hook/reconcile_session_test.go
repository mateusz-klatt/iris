package hook

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// oneCallHub is a hub that registers and reconciles in one call. answers are the one-call
// form's errors in turn; registered counts the two-call form's registrations.
type oneCallHub struct {
	scriptHub
	answers    []error
	generation int
	sent       *[]hubclient.ShellSessionRequest
	registered *int
}

func (h *oneCallHub) ReconcileSession(_ context.Context, r hubclient.ShellSessionRequest) (hubclient.ShellResponse, error) {
	*h.sent = append(*h.sent, r)
	var err error
	if len(h.answers) > 0 {
		err, h.answers = h.answers[0], h.answers[1:]
	}
	out := hubclient.ShellResponse{CheckResponse: hubclient.CheckResponse{Message: h.message}}
	out.Session.ExecutionID, out.Session.Generation = "ex", h.generation
	return out, err
}

func (h *oneCallHub) RegisterSession(ctx context.Context, r hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	*h.registered++
	return h.scriptHub.RegisterSession(ctx, r)
}

func newOneCallHub(answers ...error) *oneCallHub {
	return &oneCallHub{answers: answers, generation: 4, sent: &[]hubclient.ShellSessionRequest{}, registered: new(int)}
}

// A shell call makes one round trip: the session, with its remembered generation, and the
// snapshot go together, and the generation the hub answers with is remembered as the next
// proof (item b of the shell budget ruling).
func TestAShellCallRegistersAndReconcilesInOneCall(t *testing.T) {
	root := gitRepo(t)
	hub := newOneCallHub()
	r, ev := shellRunner(hub, root)
	statuses := 0
	r.Status = func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
		statuses++
		return map[string]string{"a.txt": "??"}, nil
	}
	m := fileMemory{dir: t.TempDir(), now: time.Now, restrict: func(string) error { return nil }}
	m.Remember(string(ev.Client), ev.SessionID, 3)
	r.Memory = m
	if out := r.reconcile(context.Background(), ev, &runState{}); !isAllow(out) {
		t.Fatalf("%s", out.Stdout)
	}
	sent := *hub.sent
	if len(sent) != 1 || *hub.registered != 0 || statuses != 1 {
		t.Fatalf("%d one-call requests, %d registrations, %d git status", len(sent), *hub.registered, statuses)
	}
	if got := sent[0]; got.SessionID != "s" || got.Generation != 3 || got.Kind != "before" || got.Status["a.txt"] != "??" {
		t.Fatalf("sent %+v", got)
	}
	if m.Recall(string(ev.Client), ev.SessionID) != 4 {
		t.Fatal("the hub's generation was not remembered as the next proof")
	}
	// A hub's warning reaches the model, as from the two-call form.
	hub = newOneCallHub()
	hub.message = "Iris: x.go is held by codex-host"
	r, ev = shellRunner(hub, root)
	ev.Kind = KindPostTool
	if out := r.reconcile(context.Background(), ev, &runState{}); !strings.Contains(string(out.Stdout), "x.go is held") {
		t.Fatalf("%s", out.Stdout)
	}
}

// The combined API registers only after git status. If that status spends the budget,
// the shell fails open without reattaching, even when the hook remembers valid proof.
func TestAShellStatusPastItsBudgetDoesNotRegisterTheSession(t *testing.T) {
	hub := newOneCallHub()
	r, ev := shellRunner(hub, "/repo")
	r.Find = func(string) (gitinfo.Repo, error) {
		return gitinfo.Repo{Worktree: ev.CWD}, nil
	}
	// The budget runs out inside git status, where this test needs it, however loaded the machine.
	r.ShellDeadline, r.shellTimeout = 100*time.Millisecond, handTimeout
	statusReturned := make(chan struct{})
	r.Status = func(ctx context.Context, _ string, _ gitinfo.Scope, _ *gitinfo.Attempt) (map[string]string, error) {
		defer close(statusReturned)
		passDeadline(ctx)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	m := newFileMemory(t)
	m.Remember(string(ev.Client), ev.SessionID, 3)
	r.Memory = m
	phase := ""
	hub.phase = &phase
	state := &runState{}
	out := r.reconcile(context.Background(), ev, state)
	<-statusReturned
	stage, err := state.snapshot()
	if !isAllow(out) || stage != "shell status" || !(Failure{Stage: stage, Error: failureText(err)}).Degraded() {
		t.Fatalf("stage %q err %v: %s", stage, err, out.Stdout)
	}
	if len(*hub.sent) != 0 || *hub.registered != 0 || phase != "" {
		t.Fatalf("a status past its budget sent %d combined requests, %d registrations, phase %q", len(*hub.sent), *hub.registered, phase)
	}
	if got := m.Recall(string(ev.Client), ev.SessionID); got != 3 {
		t.Fatalf("the unsent recovery proof changed to %d", got)
	}
}

// A hub from before the one-call form refuses it. The hook then makes the two calls, from the
// snapshot it already took; a lost answer, which an older hub also gives a request with no
// Execution, goes the same way, where a real loss is handled as for an edit.
func TestAShellCallFallsBackToTwoCalls(t *testing.T) {
	root := gitRepo(t)
	for _, code := range []string{"bad_request", "not_found", "lost"} {
		t.Run(code, func(t *testing.T) {
			hub := newOneCallHub(&hubclient.Error{Code: code})
			r, ev := shellRunner(hub, root)
			statuses := 0
			r.Status = func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
				statuses++
				return map[string]string{}, nil
			}
			phase := ""
			hub.phase = &phase
			if out := r.reconcile(context.Background(), ev, &runState{}); !isAllow(out) || *hub.registered != 1 || phase != "before" || statuses != 1 {
				t.Fatalf("registered %d, phase %q, %d git status: %s", *hub.registered, phase, statuses, out.Stdout)
			}
		})
	}
	// Any other failure is the hub's, and fails open as before, without a second try.
	hub := newOneCallHub(errors.New("connection refused"))
	r, ev := shellRunner(hub, root)
	state := &runState{}
	if out := r.reconcile(context.Background(), ev, state); !isAllow(out) || *hub.registered != 0 {
		t.Fatalf("registered %d: %s", *hub.registered, out.Stdout)
	}
	if stage, err := state.snapshot(); stage != "shell session" || err == nil {
		t.Fatalf("stage %q err %v", stage, err)
	}
	// A git status that fails before the one call fails open with nothing sent.
	hub = newOneCallHub()
	r, ev = shellRunner(hub, root)
	r.Status = func(context.Context, string, gitinfo.Scope, *gitinfo.Attempt) (map[string]string, error) {
		return nil, errors.New("git")
	}
	state = &runState{}
	if out := r.reconcile(context.Background(), ev, state); !isAllow(out) || len(*hub.sent) != 0 {
		t.Fatalf("sent %d: %s", len(*hub.sent), out.Stdout)
	}
	if stage, err := state.snapshot(); stage != "shell status" || err == nil || err.Error() != "git" {
		t.Fatalf("stage %q err %v", stage, err)
	}
}

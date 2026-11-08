package binding

import (
	"context"
	"os/exec"
	"testing"
	"time"

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

// The grace period ends with the call that waits in it: a caller that gives up gets the
// unbound answer it had, and nothing is registered.
func TestTheGracePeriodEndsWithItsCaller(t *testing.T) {
	hub := &scriptedHub{binds: []string{"unbound", "unbound", "unbound", "unbound", "unbound"}}
	r := resolverFor(t, hub, "claude", map[string]string{"CLAUDE_CODE_SESSION_ID": "s"}, proc.Ancestor{})
	r.Grace = time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := r.Resolve(ctx, ""); !IsCode(err, "unbound") || hub.session != nil {
		t.Fatalf("%v registered %+v", err, hub.session)
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

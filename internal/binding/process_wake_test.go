package binding

import (
	"testing"

	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/store"
)

func TestProcessWakeRequiresHookBindingAndPositiveGenerationRecovery(t *testing.T) {
	live := newLiveHub(t)
	h := live.client()
	ctx := t.Context()
	const session = "native"
	var generation int64
	req := store.Request{
		Client: "kimi", Host: "host", Cwd: t.TempDir(), SessionID: session,
		Source: "hook", ClientPID: 42, ClientPIDStartMS: 100, SingleSession: true,
	}
	registered := live.hook(t, &generation, req)
	candidate, err := h.Bind(ctx, hubclient.BindRequest{
		Client: req.Client, Host: req.Host, SessionID: session, ClientPID: 42, ClientPIDStartMS: 100, SuccessionProof: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := HookWakeBinding(candidate, session)
	if err != nil || b.ExecutionID != registered["execution_id"] || b.Rung != 3 || !b.HookSeen || !b.Classified {
		t.Fatalf("hook bind: %+v, %v", b, err)
	}
	withoutHook := candidate
	withoutHook.HookSeen.Value = false
	if _, err := HookWakeBinding(withoutHook, session); err != errInvalidCandidate {
		t.Fatalf("hookless bind: %v", err)
	}
	if fresh, err := MergeWakeHeartbeat(b, candidate); err != nil || fresh != b {
		t.Fatalf("confirmed heartbeat: %+v, %v", fresh, err)
	}
	if _, err := MergeWakeHeartbeat(b, withoutHook); err != errInvalidCandidate {
		t.Fatalf("hookless heartbeat: %v", err)
	}

	live.restart(t)
	recovery := hubclient.SessionRequest{
		Client: req.Client, Host: req.Host, CWD: req.Cwd, SessionID: session, Source: "shim",
		ClientPID: 42, ClientPIDStartMS: 101, SingleSession: true,
	}
	if _, err := RegisterProcessWake(ctx, h, recovery); err != errGenerationFloor {
		t.Fatalf("proofless registration: %v", err)
	}
	recovery.Generation = b.Generation
	if _, err := RegisterProcessWake(ctx, h, recovery); !hubLost(err) {
		t.Fatalf("registration by a different process: %v", err)
	}
	recovery.ClientPIDStartMS = 100
	fresh, err := RegisterProcessWake(ctx, h, recovery)
	if err != nil || fresh.ExecutionID != b.ExecutionID || fresh.Generation != b.Generation || !fresh.HookSeen {
		t.Fatalf("reattached binding: %+v, %v", fresh, err)
	}
}

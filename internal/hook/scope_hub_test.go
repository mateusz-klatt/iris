package hook

import (
	"context"
	"slices"
	"sync"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// statusEntry is how a stand-in for git status logs itself in a scopeHub's calls.
func statusEntry(s gitinfo.Scope) string {
	if s == gitinfo.ReducedScope {
		return "status reduced"
	}
	return "status full"
}

// scopeHub is a hub with every route a shell reconcile can take. It takes reduced snapshots
// unless incapable is set, answers each call from its queue of errors, and logs every call in
// order, together with what a test's stand-ins add, so a test sees what ran and what was sent.
type scopeHub struct {
	mu            sync.Mutex
	incapable     bool
	generation    int
	message       string
	log           []string
	registers     []hubclient.SessionRequest
	plain         []hubclient.ReconcileRequest
	oneCall       []hubclient.ShellSessionRequest
	scoped        []hubclient.ScopedRequest
	scopedOneCall []hubclient.ScopedSessionRequest
	registerErrs  []error
	scopedErrs    []error
	// waitOneCall and waitScoped make those calls last until the reconcile's deadline.
	waitOneCall, waitScoped bool
}

func (h *scopeHub) note(entry string) { h.mu.Lock(); h.log = append(h.log, entry); h.mu.Unlock() }

func (h *scopeHub) calls() []string { h.mu.Lock(); defer h.mu.Unlock(); return slices.Clone(h.log) }

func (h *scopeHub) capabilities() []string {
	if h.incapable {
		return nil
	}
	return []string{"shell_scope"}
}

func nextErr(errs *[]error) error {
	if len(*errs) == 0 {
		return nil
	}
	err := (*errs)[0]
	*errs = (*errs)[1:]
	return err
}

func (h *scopeHub) answer() hubclient.ShellResponse {
	out := hubclient.ShellResponse{CheckResponse: hubclient.CheckResponse{Message: h.message}}
	out.Session.ExecutionID, out.Session.Generation, out.Session.Capabilities = "ex", h.generation, h.capabilities()
	return out
}

func (h *scopeHub) RegisterSession(_ context.Context, r hubclient.SessionRequest) (hubclient.SessionResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.log = append(h.log, "register")
	h.registers = append(h.registers, r)
	if err := nextErr(&h.registerErrs); err != nil {
		return hubclient.SessionResponse{}, err
	}
	return hubclient.SessionResponse{ExecutionID: "ex", Generation: h.generation, Capabilities: h.capabilities()}, nil
}

func (h *scopeHub) Touch(context.Context, hubclient.TouchRequest) (hubclient.TouchResponse, error) {
	return hubclient.TouchResponse{Action: "allow"}, nil
}

func (h *scopeHub) EndExecution(context.Context, string, int) error { return nil }

func (h *scopeHub) Reconcile(_ context.Context, r hubclient.ReconcileRequest) (hubclient.CheckResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.log = append(h.log, "reconcile")
	h.plain = append(h.plain, r)
	return hubclient.CheckResponse{Message: h.message}, nil
}

func (h *scopeHub) ReconcileSession(ctx context.Context, r hubclient.ShellSessionRequest) (hubclient.ShellResponse, error) {
	h.mu.Lock()
	h.log = append(h.log, "one-call")
	h.oneCall = append(h.oneCall, r)
	wait, out := h.waitOneCall, h.answer()
	h.mu.Unlock()
	if wait {
		<-ctx.Done()
		return hubclient.ShellResponse{}, ctx.Err()
	}
	return out, nil
}

func (h *scopeHub) ReconcileScoped(ctx context.Context, r hubclient.ScopedRequest) (hubclient.CheckResponse, error) {
	h.mu.Lock()
	h.log = append(h.log, "scoped")
	h.scoped = append(h.scoped, r)
	wait, err, out := h.waitScoped, nextErr(&h.scopedErrs), h.answer().CheckResponse
	h.mu.Unlock()
	if wait {
		<-ctx.Done()
		return hubclient.CheckResponse{}, ctx.Err()
	}
	return out, err
}

func (h *scopeHub) ReconcileScopedSession(ctx context.Context, r hubclient.ScopedSessionRequest) (hubclient.ShellResponse, error) {
	h.mu.Lock()
	h.log = append(h.log, "scoped one-call")
	h.scopedOneCall = append(h.scopedOneCall, r)
	wait, err, out := h.waitScoped, nextErr(&h.scopedErrs), h.answer()
	h.mu.Unlock()
	if wait {
		<-ctx.Done()
		return hubclient.ShellResponse{}, ctx.Err()
	}
	if err != nil {
		return hubclient.ShellResponse{}, err
	}
	return out, nil
}

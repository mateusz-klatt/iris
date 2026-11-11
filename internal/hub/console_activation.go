package hub

import (
	"context"
	"errors"
	"net/http"

	"github.com/mateusz-klatt/iris/internal/consoleauth"
	"github.com/mateusz-klatt/iris/internal/store"
)

// ConfigureConsole loads immutable startup authority. Call before serving. The
// keyMissing callback only inspects presence, including a key this process could
// not load. keyAvailable checks existing usable material for restart reporting.
// The activate callback persists an independent protected key after
// fresh owner provisioning or verified activation commits. These callbacks never
// change this process's browser gate.
func (h *Hub) ConfigureConsole(key []byte, keyMissing, keyAvailable func() bool, activate func() error) error {
	h.console = newConsoleAuth(nil)
	h.consoleActivate = activate
	h.consoleKeyMissing = keyMissing
	h.consoleKeyAvailable = keyAvailable
	if len(key) == 0 {
		return nil
	}
	if len(key) != 32 {
		return errors.New("invalid console key")
	}
	enabled, err := h.store.HasEnabledConsoleOperator(context.Background())
	if err != nil {
		return errors.New("console operator readiness unavailable")
	}
	if !enabled {
		return errors.New("console needs an enabled operator")
	}
	h.console = newConsoleAuth(key)
	return nil
}

func (h *Hub) consoleOwnerOnly(w http.ResponseWriter, r *http.Request) bool {
	if !h.ownerOnly(w, r) {
		return false
	}
	if len(r.Header.Values("Authorization")) != 1 || len(r.Header.Values("Cookie")) != 0 {
		writeError(w, &store.Error{Code: "forbidden", Message: "console setup requires one owner credential without browser cookies"})
		return false
	}
	return true
}

func (h *Hub) activateConsole(w http.ResponseWriter, r *http.Request) {
	if !h.consoleOwnerOnly(w, r) {
		return
	}
	if len(h.console.key) == 32 {
		writeError(w, &store.Error{Code: "conflict", Message: "console is already active"})
		return
	}
	if h.consoleActivate == nil {
		writeError(w, &store.Error{Code: "bad_request", Message: "console activation is unavailable"})
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !consoleDecode(w, r, &input) {
		return
	}
	if len(input.Password) > consoleauth.MaxPasswordBytes {
		writeError(w, &store.Error{Code: "bad_request", Message: "password exceeds 1024 UTF-8 bytes"})
		return
	}
	if !h.console.passwordRate(w, h.now(), input.Username) {
		return
	}
	before, err := h.store.LookupConsoleAccount(r.Context(), input.Username)
	if err != nil {
		var domain *store.Error
		if !errors.As(err, &domain) || domain.Code != "not_found" {
			writeError(w, err)
			return
		}
	}
	unreadable := err == nil && consoleauth.ValidateHash(before.PasswordHash) != nil
	eligible := err == nil && before.Enabled() && before.Role == "operator" && !unreadable
	record := dummyConsoleHash
	if eligible {
		record = before.PasswordHash
	}
	verified, err := h.console.verify(input.Password, record)
	if errors.Is(err, consoleauth.ErrBusy) {
		w.Header().Set("Retry-After", "1")
		writeError(w, &store.Error{Code: "rate_limited", Message: "password work capacity reached"})
		return
	}
	if unreadable {
		writeError(w, &store.Error{Code: "bad_request", Message: "this version does not read the stored password format; recover the operator with iris admin console reset <username>, then activate"})
		return
	}
	if err != nil || !verified || !eligible {
		consoleFailure(w)
		return
	}
	account, err := h.store.PrepareConsoleActivation(r.Context(), before)
	if err != nil {
		writeError(w, err)
		return
	}
	if err = h.consoleActivate(); err != nil {
		writeError(w, &store.Error{Code: "bad_request", Message: "console key could not be prepared; console remains closed"})
		return
	}
	writeJSON(w, map[string]any{"account": account, "restart_required": true})
}

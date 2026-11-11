package hub

import (
	"errors"
	"net/http"

	"github.com/mateusz-klatt/iris/internal/consoleauth"
	"github.com/mateusz-klatt/iris/internal/store"
)

// The KDF boundary is replaceable in tests to hold admission or report capacity.
// Production shares consoleauth's two slots with login and password changes.
var createOwnerPassword = consoleauth.CreatePassword

// Owner setup remains available while browser authentication is closed. Neither a host
// token nor any browser account can create or recover accounts. Recovery always
// restores a named operator, never an anonymous console session.
func (h *Hub) ownerAccountsRoutes() {
	h.mux.HandleFunc("POST /v1/admin/console/accounts", func(w http.ResponseWriter, r *http.Request) {
		h.ownerConsoleAccount(w, r, false)
	})
	h.mux.HandleFunc("POST /v1/admin/console/accounts/reset", func(w http.ResponseWriter, r *http.Request) {
		h.ownerConsoleAccount(w, r, true)
	})
	h.mux.HandleFunc("POST /v1/admin/console/activate", h.activateConsole)
}

func (h *Hub) ownerConsoleAccount(w http.ResponseWriter, r *http.Request, reset bool) {
	if !h.consoleOwnerOnly(w, r) {
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !consoleDecode(w, r, &input) {
		return
	}
	if _, err := consoleauth.LoginKey(input.Username); err != nil {
		writeError(w, &store.Error{Code: "bad_request", Message: "invalid console username"})
		return
	}
	var before store.ConsoleAccount
	if reset {
		var err error
		before, err = h.store.LookupConsoleAccount(r.Context(), input.Username)
		if err != nil {
			writeError(w, err)
			return
		}
	}
	if !h.console.passwordRate(w, h.now(), input.Username) {
		return
	}
	hash, err := createOwnerPassword(input.Password)
	if err != nil {
		code, message := "bad_request", "new password must have at least 15 Unicode code points and at most 1024 UTF-8 bytes"
		if errors.Is(err, consoleauth.ErrBusy) {
			code, message = "rate_limited", "password work capacity reached"
			w.Header().Set("Retry-After", "1")
		}
		writeError(w, &store.Error{Code: code, Message: message})
		return
	}
	var account store.ConsoleAccount
	prepareKey := false
	if reset {
		account, err = h.store.RecoverConsoleOperator(r.Context(), before, hash)
	} else {
		account, prepareKey, err = h.store.ProvisionConsoleOperatorWithActivation(r.Context(), store.ConsoleAccountInput{Username: input.Username, PasswordHash: hash}, h.consoleKeyMissing)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if reset {
		writeJSON(w, map[string]any{"account": account})
		return
	}
	if prepareKey {
		if h.consoleActivate == nil || h.consoleActivate() != nil {
			writeError(w, &store.Error{Code: "bad_request", Message: "operator created; console key could not be prepared; console remains closed; use activate to retry"})
			return
		}
	}
	// Creation committed an enabled operator. An existing usable key can make
	// the next startup ready even when this command prepared no key.
	restartRequired := len(h.console.key) == 0 && (prepareKey || (h.consoleKeyAvailable != nil && h.consoleKeyAvailable()))
	writeJSON(w, map[string]any{"account": account, "account_created": true, "key_prepared": prepareKey, "restart_required": restartRequired})
}

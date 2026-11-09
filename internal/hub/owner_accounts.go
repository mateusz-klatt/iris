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

// The owner setup routes are staged with account authentication. Neither a host
// token nor any browser account can create or recover accounts. Recovery always
// restores a named operator, never an anonymous console session.
func (h *Hub) ownerAccountsRoutes() {
	h.mux.HandleFunc("POST /v1/admin/console/accounts", func(w http.ResponseWriter, r *http.Request) {
		h.ownerConsoleAccount(w, r, false)
	})
	h.mux.HandleFunc("POST /v1/admin/console/accounts/reset", func(w http.ResponseWriter, r *http.Request) {
		h.ownerConsoleAccount(w, r, true)
	})
}

func (h *Hub) ownerConsoleAccount(w http.ResponseWriter, r *http.Request, reset bool) {
	if h.console == nil {
		writeError(w, &store.Error{Code: "not_found", Message: "unknown API route"})
		return
	}
	if len(h.console.key) != 32 {
		writeError(w, &store.Error{Code: "unauthorized", Message: "console authentication unavailable"})
		return
	}
	if !h.ownerOnly(w, r) {
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
	if reset {
		account, err = h.store.RecoverConsoleOperator(r.Context(), before, hash)
	} else {
		account, err = h.store.ProvisionConsoleOperator(r.Context(), store.ConsoleAccountInput{Username: input.Username, PasswordHash: hash})
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"account": account})
}

package hub

import (
	"net/http"

	"github.com/mateusz-klatt/iris/internal/accountimport"
	"github.com/mateusz-klatt/iris/internal/store"
)

// Import remains an owner operation while the account console is staged. The
// source adapter sends a bounded snapshot; only the hub writes Iris records.
func (h *Hub) consoleImportRoutes() {
	h.mux.HandleFunc("POST /v1/admin/console/import", func(w http.ResponseWriter, r *http.Request) {
		h.consoleImport(w, r, false)
	})
	h.mux.HandleFunc("POST /v1/admin/console/import-grants", func(w http.ResponseWriter, r *http.Request) {
		h.consoleImport(w, r, true)
	})
}

func (h *Hub) consoleImport(w http.ResponseWriter, r *http.Request, assignments bool) {
	if !h.ownerOnly(w, r) {
		return
	}
	if len(r.Header.Values("Authorization")) != 1 || len(r.Header.Values("Cookie")) != 0 {
		writeError(w, &store.Error{Code: "forbidden", Message: "import requires one owner credential without browser cookies"})
		return
	}
	var input struct {
		Batch    *accountimport.Batch             `json:"batch"`
		Transfer *store.ConsoleAssignmentTransfer `json:"transfer"`
		DryRun   *bool                            `json:"dry_run"`
	}
	if err := accountimport.DecodeBatchJSON(r.Body, &input); err != nil {
		writeError(w, &store.Error{Code: "bad_request", Message: "invalid import request"})
		return
	}
	if input.DryRun == nil || (assignments && (input.Transfer == nil || input.Batch != nil)) || (!assignments && (input.Batch == nil || input.Transfer != nil)) {
		writeError(w, &store.Error{Code: "bad_request", Message: "import requires an explicit dry_run and the matching payload"})
		return
	}
	var report store.ConsoleImportReport
	var err error
	if assignments {
		report, err = h.store.TransferConsoleAssignments(r.Context(), *input.Transfer, *input.DryRun)
	} else {
		report, err = h.store.ImportConsoleAccounts(r.Context(), *input.Batch, *input.DryRun)
	}
	if err != nil {
		// writeError preserves the store's fixed refusal reasons while replacing
		// untyped failures, including SQLite diagnostics, with a generic error.
		writeError(w, err)
		return
	}
	writeJSON(w, report)
}

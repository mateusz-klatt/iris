package hub

import (
	"net/http"
	"strings"

	"github.com/mateusz-klatt/iris/internal/store"
)

// consoleRequestPrincipal is the staged account transport boundary. Authentication
// supplies the verified opaque-session digest in the gated account path; this
// function never derives a person from request fields or a machine bearer. It is
// deliberately not wired into the legacy routes before account cutover.
func consoleRequestPrincipal(r *http.Request, sessionDigest []byte) (store.Principal, error) {
	if len(r.Header.Values("Authorization")) != 0 {
		return store.Principal{}, &store.Error{Code: "forbidden", Message: "console account requests cannot include machine credentials"}
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/ui/") {
		return store.Principal{}, &store.Error{Code: "forbidden", Message: "account sessions authorize only the console API"}
	}
	if len(sessionDigest) != 32 {
		return store.Principal{}, &store.Error{Code: "unauthorized", Message: "console account session required"}
	}
	return store.Principal{Kind: store.ConsolePrincipal, SessionDigest: sessionDigest}, nil
}

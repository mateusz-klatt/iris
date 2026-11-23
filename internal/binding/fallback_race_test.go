package binding

import (
	"errors"
	"net/http"
	"testing"

	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

func TestFallbackFollowsSessionStartThatWinsRegistrationRace(t *testing.T) {
	for _, tc := range []struct {
		name       string
		watch      bool
		process    bool
		localProof int
	}{
		{name: "shim by session"},
		{name: "watch by session", watch: true},
		{name: "shim by process", process: true},
		{name: "winner below local proof", localProof: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRestartableBindingHub(t)
			registration, ancestor, env, rung := hookS, proc.Ancestor{}, sessionS, 2
			if tc.process {
				ancestor, env, rung = proc.Ancestor{PID: 42, StartMS: 100, Name: "cursor-agent"}, nil, 3
				registration.Client = "cursor"
				registration.ClientPID, registration.ClientPIDStartMS, registration.SingleSession = 42, 100, true
			}
			var winner string
			var lookups, proof int
			httpClient := *h.srv.Client()
			transport := httpClient.Transport
			httpClient.Transport = adoptionTransport(func(req *http.Request) (*http.Response, error) {
				switch req.URL.Path {
				case "/v1/bind":
					lookups++
				case "/v1/sessions/outside":
					// SessionStart commits after the client begins the fallback POST,
					// and before the store handles the generation-zero registration.
					out, err := h.store.Call(req.Context(), "sessions", registration)
					if err != nil {
						return nil, err
					}
					winner, proof = out["execution_id"].(string), tc.localProof
				}
				return transport.RoundTrip(req)
			})
			r := tickResolver(&hubclient.Client{Config: config{h.srv.URL}, HTTP: &httpClient}, registration.Client, env, ancestor)
			r.CWD = t.TempDir()
			if tc.watch {
				r.HeartbeatSource = "watch"
			}
			r.Recall = func(_, _ string) (int, bool, error) { return proof, proof > 0, nil }

			b, err := r.Resolve(t.Context(), "")
			if tc.localProof == 0 {
				if err != nil || b.ExecutionID != winner || b.SessionID != registration.SessionID || b.Rung != rung || !b.HookSeen || r.own {
					t.Fatalf("fallback failed to bind the hook winner %s: bound=%+v own=%t err=%v", winner, b, r.own, err)
				}
			} else {
				var lost *hubclient.Error
				if b != (Bound{}) || !errors.As(err, &lost) || lost.Code != "lost" {
					t.Fatalf("fallback bound below local generation %d: bound=%+v err=%v", tc.localProof, b, err)
				}
				if gen, valid, present := lost.HighestGeneration(); gen != 1 || !valid || !present {
					t.Fatalf("fallback refusal omitted the hub's generation: generation=%d valid=%t present=%t", gen, valid, present)
				}
			}
			if posted := h.sessions(); lookups != 2 || len(posted) != 1 || posted[0].Generation != 0 || posted[0].Source != "mcp" || len(h.successions()) != 0 {
				t.Fatalf("fallback race made unexpected requests: lookups=%d registrations=%+v successions=%+v", lookups, posted, h.successions())
			}
		})
	}
}

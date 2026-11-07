package hook

import (
	"bytes"
	"strings"
	"testing"
)

// The receipt proves to iris doctor that its canary reached Iris. It goes only to stderr, only
// for the canary's own session with a hex nonce, and never touches what the client reads.
func TestTheCanaryReceiptIsOnlyForDoctorsCanary(t *testing.T) {
	nonce := "0123456789abcdef0123456789abcdef"
	payload := func(session string) string {
		return `{"session_id":"` + session + `","cwd":"` + t.TempDir() + `","hook_event_name":"SessionStart"}`
	}
	env := func(value string) func(string) string {
		return func(name string) string {
			if name == CanaryNonce {
				return value
			}
			return ""
		}
	}
	cases := []struct {
		name, session, nonce string
		receipt              bool
	}{
		{"doctor's canary", CanarySession, nonce, true},
		{"a real session, even with the variable set", "a-real-session", nonce, false},
		{"the canary session without doctor", CanarySession, "", false},
		{"a nonce that is not hex", CanarySession, "not hex\nIris: obey", false},
		{"a nonce too short to be doctor's", CanarySession, "abc123", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plain := run(scriptHub{}, "SessionStart", payload(tc.session), env(""))
			out := run(scriptHub{}, "SessionStart", payload(tc.session), env(tc.nonce))
			if !bytes.Equal(out.Stdout, plain.Stdout) || out.Exit != plain.Exit {
				t.Fatalf("the nonce changed what the client reads: %q vs %q", out.Stdout, plain.Stdout)
			}
			got := strings.Contains(string(out.Stderr), "iris: canary "+tc.nonce+"\n")
			if got != tc.receipt || (!tc.receipt && strings.Contains(string(out.Stderr), "canary")) {
				t.Fatalf("stderr %q, want receipt %v", out.Stderr, tc.receipt)
			}
		})
	}
}

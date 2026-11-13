package hub

import "testing"

// A lookup by session and client process together answers with `match`, which Kimi's one-shot
// watcher requires before it trusts the binding (ADR 030): a hub from before the combined lookup
// would take the request by session alone and answer without it. The cwd is not matched, and an
// incomplete process identity is a bad request, not a session-only lookup.
func TestTheCombinedBindAnswerSaysHowItMatched(t *testing.T) {
	_, s := setup(t)
	registered := request(t, s, "POST", "/v1/sessions", map[string]any{"client": "kimi", "session_id": "session_k", "host": "h", "cwd": "/repo", "remote_url": "https://example.com/repo.git", "root_commit": "r", "source": "hook", "client_pid": 4242, "client_pid_start_ms": 77, "single_session": true}, 200)
	got := request(t, s, "POST", "/v1/bind", map[string]any{"client": "kimi", "host": "h", "cwd": "/elsewhere", "session_id": "session_k", "client_pid": 4242, "client_pid_start_ms": 77}, 200)
	if got["execution_id"] != registered["execution_id"] || got["match"] != "process_session" || got["hook_seen"] != true {
		t.Fatalf("combined bind answered %v", got)
	}
	if bySession := request(t, s, "POST", "/v1/bind", map[string]any{"client": "kimi", "host": "h", "session_id": "session_k"}, 200); bySession["match"] != nil {
		t.Fatalf("a session-only bind claimed the combined match: %v", bySession)
	}
	request(t, s, "POST", "/v1/bind", map[string]any{"client": "kimi", "host": "h", "session_id": "session_k", "client_pid": 4242}, 400)
}

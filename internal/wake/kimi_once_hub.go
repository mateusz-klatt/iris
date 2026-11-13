package wake

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/mateusz-klatt/iris/internal/binding"
)

// errUnmatched is a bind answer that did not come from the lookup of a session and its client
// process together. A hub from before that lookup takes such a request by session alone, and its
// answer names no match.
var errUnmatched = errors.New("the hub did not match this Kimi process and session together")

// kimiBind looks up, and never registers, the Execution a hook registered for this Kimi process
// and native session (ADR 030). Only the combined lookup's answer, with hook evidence and a live
// generation, is accepted.
func (s hubSource) kimiBind(ctx context.Context, host, cwd, session string, pid int, startMS int64) (binding.Bound, error) {
	var out struct {
		binding.Bound
		HookSeen bool   `json:"hook_seen"`
		Match    string `json:"match"`
	}
	err := s.client.Do(ctx, http.MethodPost, "/v1/bind", map[string]any{
		"client": "kimi", "host": host, "cwd": cwd, "session_id": session, "client_pid": pid, "client_pid_start_ms": startMS,
	}, &out)
	if err != nil {
		return binding.Bound{}, err
	}
	if out.Match != "process_session" || !out.HookSeen || out.ExecutionID == "" || out.Generation <= 0 {
		return binding.Bound{}, errUnmatched
	}
	return out.Bound, nil
}

// heartbeat renews only the pinned generation's lease, as a shim does. It sets no watch evidence,
// so the session is not marked monitored and keeps its prompt reminders. A matching heartbeat
// also reattaches an Execution that is recovering after a hub restart.
func (s hubSource) heartbeat(ctx context.Context, target binding.WakeTarget) error {
	return s.client.Do(ctx, http.MethodPost, "/v1/executions/"+url.PathEscape(target.Bound.ExecutionID)+"/heartbeat",
		map[string]any{"generation": target.Bound.Generation, "source": "shim"}, nil)
}

// checkpoint saves cursor as the session's iris-watch cursor, which only moves forward, exactly as
// the persistent watch saves what it has announced.
func (s hubSource) checkpoint(ctx context.Context, target binding.WakeTarget, cursor int64) error {
	query := url.Values{
		"execution_id": {target.Bound.ExecutionID}, "generation": {strconv.Itoa(target.Bound.Generation)},
		"consumer_id": {"iris-watch"}, "limit": {"1"}, "after": {strconv.FormatInt(cursor, 10)},
	}
	return s.client.Do(ctx, http.MethodGet, "/v1/events?"+query.Encode(), nil, nil)
}

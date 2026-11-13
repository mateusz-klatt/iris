package wake

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// hubSource observes attention without fetching mail, acknowledging it, retaining a consumer
// cursor or occupying the session's monitor slot. Its caller owns retry and reconnect policy.
type hubSource struct{ client *hubclient.Client }

func (s hubSource) HookSeen(ctx context.Context, target binding.WakeTarget) (bool, error) {
	var out struct {
		binding.Bound
		HookSeen bool `json:"hook_seen"`
	}
	client := target.Client
	if client == "" {
		client = "codex"
	}
	err := s.client.Do(ctx, http.MethodPost, "/v1/bind", map[string]string{
		"execution": target.Bound.ExecutionID, "client": client, "host": target.Host,
	}, &out)
	if err != nil {
		return false, err
	}
	return out.HookSeen && out.ExecutionID == target.Bound.ExecutionID && out.Generation == target.Bound.Generation, nil
}

func eventQuery(target binding.WakeTarget, after int64, stream string) url.Values {
	return url.Values{
		"execution_id": {target.Bound.ExecutionID}, "generation": {strconv.Itoa(target.Bound.Generation)},
		"after": {strconv.FormatInt(after, 10)}, "stream": {stream},
	}
}

func (s hubSource) Attention(ctx context.Context, target binding.WakeTarget, after int64, stream string) (attentionRead, error) {
	query := eventQuery(target, after, stream)
	query.Set("limit", "1")
	query.Set("attention", "1")
	var out struct {
		Attention hubclient.Attention `json:"attention"`
		Stream    string              `json:"stream"`
		HighWater int64               `json:"high_water"`
	}
	if err := s.client.Do(ctx, http.MethodGet, "/v1/events?"+query.Encode(), nil, &out); err != nil {
		return attentionRead{}, err
	}
	return attentionRead{Attention: out.Attention, Stream: out.Stream, HighWater: out.HighWater}, nil
}

func (s hubSource) Connect(ctx context.Context, target binding.WakeTarget, after int64, stream string) (eventConnection, error) {
	if s.client == nil || s.client.Config == nil {
		return nil, errors.New("hub client is not configured")
	}
	base := strings.TrimRight(s.client.Config.HubURL(), "/")
	if err := hubclient.ValidateURL(base); err != nil {
		return nil, err
	}
	token, err := s.client.Config.Token()
	if err != nil {
		return nil, err
	}
	u, _ := url.Parse(base) // ValidateURL already parsed this exact URL.
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/watch"
	u.RawPath = ""
	u.RawQuery = eventQuery(target, after, stream).Encode()
	headers := http.Header{}
	if token != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	dial, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(dial, u.String(), &websocket.DialOptions{
		HTTPClient: hubclient.NoRedirects(s.client.HTTP), HTTPHeader: headers, Subprotocols: []string{"iris.v1"},
	})
	if err != nil {
		if response != nil && response.StatusCode == http.StatusUnauthorized {
			return nil, &hubclient.Error{Code: "unauthorized", Message: "the hub rejected the wake stream's token"}
		}
		if response != nil && response.StatusCode != http.StatusSwitchingProtocols {
			return nil, &hubclient.StatusError{Path: "/v1/watch", Code: response.StatusCode, Status: response.Status}
		}
		return nil, err
	}
	conn.SetReadLimit(4 << 10)
	return &hubConnection{conn: conn}, nil
}

type hubConnection struct{ conn *websocket.Conn }

func (c *hubConnection) Read(ctx context.Context) (eventFrame, error) {
	_, raw, err := c.conn.Read(ctx)
	if err != nil {
		return eventFrame{}, err
	}
	var frame eventFrame
	if err := json.Unmarshal(raw, &frame); err != nil {
		return eventFrame{}, err
	}
	return frame, nil
}

func (c *hubConnection) Close() { _ = c.conn.CloseNow() }

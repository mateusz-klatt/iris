package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestNamedTokenRevokesStreams(t *testing.T) {
	_, s := setup(t)
	session := register(t, s, 1)
	token := request(t, s, "POST", "/v1/tokens", map[string]string{"host": "laptop"}, 200)["token"].(string)
	call := func(path string, status int) {
		t.Helper()
		r, _ := http.NewRequest("GET", s.URL+path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		resp, err := s.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != status {
			t.Fatal(path, resp.StatusCode)
		}
	}
	call("/v1/agents", 200)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	headers := http.Header{"Authorization": {"Bearer " + token}}
	open := func(endpoint string, protocols []string, headers http.Header) *websocket.Conn {
		c, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPHeader: headers, Subprotocols: protocols})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.CloseNow() })
		return c
	}
	bearer := open(watchURL(s, session["execution_id"], 0), []string{"iris.v1"}, headers)
	ui := open("ws"+strings.TrimPrefix(s.URL, "http")+"/v1/ui/watch", []string{"iris.v1"}, headers)
	r, _ := http.NewRequest("POST", s.URL+"/v1/wake-tickets", strings.NewReader(`{"execution_id":"`+session["execution_id"].(string)+`","generation":1}`))
	r.Header = headers
	resp, err := s.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	var ticket struct {
		URL       string   `json:"url"`
		Protocols []string `json:"protocols"`
	}
	err = json.NewDecoder(resp.Body).Decode(&ticket)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	wake := open(ticket.URL, ticket.Protocols, nil)
	request(t, s, "DELETE", "/v1/tokens/laptop", nil, 200)
	for _, c := range []*websocket.Conn{bearer, ui, wake} {
		for {
			_, _, err := c.Read(ctx)
			if err != nil {
				if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
					t.Fatalf("stream not revoked: %v", err)
				}
				break
			}
		}
	}
	call("/v1/agents", 401)
	c, resp, err := websocket.Dial(ctx, watchURL(s, session["execution_id"], 0), &websocket.DialOptions{HTTPHeader: headers, Subprotocols: []string{"iris.v1"}})
	if err == nil {
		c.CloseNow()
		t.Fatal("revoked token opened websocket")
	}
	if resp == nil || resp.StatusCode != 401 {
		t.Fatal(resp, err)
	}
	listed := request(t, s, "GET", "/v1/tokens", nil, 200)
	text, _ := json.Marshal(listed)
	if strings.Contains(string(text), token) || strings.Contains(string(text), "digest") {
		t.Fatal("secret leaked", listed)
	}
	request(t, s, "POST", "/v1/tokens", map[string]string{"host": "laptop"}, 200)
	call("/v1/agents", 401)
	request(t, s, "GET", "/v1/status", nil, 200) // legacy remains accepted
}

func TestOnlyTheOwnerManagesTokens(t *testing.T) {
	_, s := setup(t)
	host := request(t, s, "POST", "/v1/tokens", map[string]string{"host": "laptop"}, 200)["token"].(string)
	as := func(method, path, body string) int {
		t.Helper()
		r, _ := http.NewRequest(method, s.URL+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+host)
		resp, err := s.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	for _, route := range []struct{ method, path, body string }{
		{"GET", "/v1/tokens", ""},
		{"POST", "/v1/tokens", `{"host":"minted"}`},
		{"DELETE", "/v1/tokens/laptop", ""},
	} {
		if got := as(route.method, route.path, route.body); got != http.StatusForbidden {
			t.Errorf("%s %s: %d", route.method, route.path, got)
		}
	}
	// Refused means nothing changed: no token was minted and none was revoked.
	tokens := request(t, s, "GET", "/v1/tokens", nil, 200)["tokens"].([]any)
	if len(tokens) != 1 || tokens[0].(map[string]any)["revoked_ms"] != nil {
		t.Fatal(tokens)
	}
	if got := as("GET", "/v1/agents?repository_id=x", ""); got == http.StatusForbidden {
		t.Fatal("a host token lost its ordinary authority")
	}
}

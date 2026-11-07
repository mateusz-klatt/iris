package hubclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A response past the read limit is refused as too large, not cut short and then reported as
// broken JSON, which would hide why a valid hub answer could not be used (gap review A11).
func TestAResponsePastTheLimitIsRefusedAsTooLarge(t *testing.T) {
	for _, extra := range []int{0, 1} {
		body := `{"x":"` + strings.Repeat("a", responseLimit-len(`{"x":""}`)+extra) + `"}`
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		c := &Client{Config: staticConfig{url: srv.URL, token: "tok"}}
		var out map[string]any
		err := c.Do(context.Background(), "GET", "/v1/inbox", nil, &out)
		srv.Close()
		if extra == 0 && (err != nil || len(out["x"].(string)) == 0) {
			t.Fatalf("a response of exactly the limit was refused: %v", err)
		}
		if extra == 1 && !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("a response past the limit gave %v", err)
		}
	}
}

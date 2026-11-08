package watch

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// The watch handshake carries the bearer token, so it must not follow a redirect.
func TestTheWatchHandshakeDoesNotFollowARedirect(t *testing.T) {
	var reached atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
	defer elsewhere.Close()
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer hub.Close()
	var out bytes.Buffer
	cursor, stream := int64(0), ""
	r := &Runner{Resolver: &binding.Resolver{Hub: &hubclient.Client{Config: testConfig{url: hub.URL}}}}
	err := r.connect(context.Background(), binding.Bound{ExecutionID: "ex"}, &cursor, &stream, &Coalescer{Output: &out})
	if !errors.Is(err, hubclient.ErrRedirect) || reached.Load() != 0 {
		t.Fatalf("err %v, redirect target reached %d times", err, reached.Load())
	}
}

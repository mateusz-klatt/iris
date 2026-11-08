package doctor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// doctor sends the token to check it and to store the report, so neither call may
// follow a redirect: the token would go wherever the redirect points.
func TestDoctorNeverSendsTheTokenThroughARedirect(t *testing.T) {
	var reached atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
	defer elsewhere.Close()
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/status" {
			return
		}
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer hub.Close()
	probe := probeHub(context.Background(), hub.URL, "secret", nil, hub.Client())
	if !probe.up || probe.tokenOK || !probe.unchecked {
		t.Fatalf("%+v", probe)
	}
	if err := putReport(context.Background(), hub.URL, "secret", Report{}, hub.Client()); err == nil {
		t.Fatal("a redirected report was taken as stored")
	}
	if reached.Load() != 0 {
		t.Fatalf("the redirect target was called %d times", reached.Load())
	}
}

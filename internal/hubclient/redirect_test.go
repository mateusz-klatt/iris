package hubclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Go keeps the Authorization header across a redirect to the same host, whatever
// the scheme or port. A hub URL that passed ValidateURL could otherwise hand the
// token to a cleartext or different endpoint, so no hub request follows one.
func TestTheTokenNeverFollowsARedirect(t *testing.T) {
	var reached atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("the token followed the redirect")
		}
	}))
	defer elsewhere.Close()
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer hub.Close()
	for name, hc := range map[string]*http.Client{"default": nil, "hedged": HedgedClient(), "custom": {}} {
		c := &Client{Config: staticConfig{url: hub.URL, token: "secret"}, HTTP: hc}
		_, err := c.RegisterSession(context.Background(), SessionRequest{SessionID: "s"})
		if !errors.Is(err, ErrRedirect) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if reached.Load() != 0 {
		t.Fatalf("the redirect target was called %d times", reached.Load())
	}
}

func TestNoRedirectsLeavesTheCallersClientAlone(t *testing.T) {
	custom := &http.Client{}
	if NoRedirects(custom).CheckRedirect == nil || custom.CheckRedirect != nil {
		t.Fatal("the copy refuses redirects and the original is unchanged")
	}
	if NoRedirects(nil).CheckRedirect == nil || http.DefaultClient.CheckRedirect != nil {
		t.Fatal("the default client was changed")
	}
}

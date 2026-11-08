package hubclient

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"time"
)

// StatusTimeout bounds the one request Status makes.
var StatusTimeout = 10 * time.Second

// StatusFailure is why Status has no document. Code is stable for scripts: config, no_token,
// unreachable, token_rejected or hub_error.
type StatusFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// StatusReport is what iris status --json prints: the hub it asked, with any credentials in its
// URL removed, and either the hub's document as the hub sent it or why there is none.
type StatusReport struct {
	Hub    string         `json:"hub"`
	Status map[string]any `json:"status,omitempty"`
	Error  *StatusFailure `json:"error,omitempty"`
}

// Status asks the configured hub for GET /v1/status with the client's token. A hub answers a
// missing or rejected token with only {"status":"ok"} and 200, never 401, so an answer without
// "stream" after a token was sent is read as a rejected token. The token is never part of the
// report, and neither are credentials written into the hub URL.
func Status(ctx context.Context, cfg Config) StatusReport {
	hub := cfg.HubURL()
	report := StatusReport{Hub: redact(hub)}
	doc, failure := status(ctx, cfg, hub)
	if failure != nil {
		failure.Message = scrub(failure.Message)
		report.Error = failure
	} else {
		report.Status = doc
	}
	return report
}

func status(ctx context.Context, cfg Config, hub string) (map[string]any, *StatusFailure) {
	token, err := cfg.Token()
	if err != nil {
		return nil, &StatusFailure{"config", err.Error()}
	}
	if hub == "" {
		return nil, &StatusFailure{"config", "no hub URL is configured"}
	}
	if token == "" {
		return nil, &StatusFailure{"no_token", "no token is configured, and without one the hub says only that it is up"}
	}
	ctx, cancel := context.WithTimeout(ctx, StatusTimeout)
	defer cancel()
	var doc map[string]any
	err = (&Client{Config: cfg}).Do(ctx, "GET", "/v1/status", nil, &doc)
	var transport *url.Error
	switch {
	case errors.As(err, &transport):
		return nil, &StatusFailure{"unreachable", err.Error()}
	case err != nil && ValidateURL(hub) != nil:
		return nil, &StatusFailure{"config", err.Error()}
	case err != nil: // an error status, an answer too large, or one that is not JSON
		return nil, &StatusFailure{"hub_error", err.Error()}
	case doc["stream"] == nil:
		return nil, &StatusFailure{"token_rejected", "the hub is up but did not accept the token"}
	}
	return doc, nil
}

// redact removes the user name and password from a hub URL.
func redact(hub string) string {
	if u, err := url.Parse(hub); err == nil && u.User != nil {
		u.User = nil
		return u.String()
	}
	return hub
}

// userinfo is the user name and password of a URL quoted in a message, which net/http writes
// with the password masked but the user name kept.
var userinfo = regexp.MustCompile(`//[^/@\s"]*@`)

// scrub removes the user name and password from every URL in a message.
func scrub(message string) string { return userinfo.ReplaceAllString(message, "//") }

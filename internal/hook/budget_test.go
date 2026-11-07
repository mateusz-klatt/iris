package hook

import (
	"testing"
	"time"
)

// A shell reconcile makes several hub calls inside its budget. On a loopback hub 300 ms is
// ample; across a network each hook is a new process that opens its own connection, and 300 ms
// went past routinely (32 times in 45 minutes on a Mac 60 to 80 ms from the hub), leaving those
// changes unclaimed. A hub that is not on this machine gets 1 s.
func TestTheShellBudgetFollowsWhereTheHubIs(t *testing.T) {
	for url, want := range map[string]time.Duration{
		"http://127.0.0.1:7777":         ReconcileDeadline,
		"http://localhost:7777":         ReconcileDeadline,
		"http://LOCALHOST":              ReconcileDeadline,
		"http://[::1]:7777":             ReconcileDeadline,
		"http://127.8.9.10":             ReconcileDeadline,
		"https://iris.example.com":      RemoteReconcileDeadline,
		"http://10.0.0.5:7777":          RemoteReconcileDeadline,
		"https://localhost.example.com": RemoteReconcileDeadline,
		"":                              ReconcileDeadline, // no hub: nothing to wait for
		"://not a url":                  ReconcileDeadline,
	} {
		if got := shellBudget(url); got != want {
			t.Errorf("%q: %s, want %s", url, got, want)
		}
	}
}

// The Runner takes the budget from the hub it is configured for, unless a test sets one.
func TestTheRunnerUsesTheConfiguredHubsBudget(t *testing.T) {
	env := func(url string) func(string) string {
		return func(k string) string {
			if k == "IRIS_HUB_URL" {
				return url
			}
			return ""
		}
	}
	if got := (Runner{Getenv: env("https://iris.example.com")}).shellLimit(); got != RemoteReconcileDeadline {
		t.Fatalf("remote: %s", got)
	}
	if got := (Runner{Getenv: env("http://127.0.0.1:1")}).shellLimit(); got != ReconcileDeadline {
		t.Fatalf("loopback: %s", got)
	}
	if got := (Runner{}).shellLimit(); got != ReconcileDeadline {
		t.Fatalf("no environment: %s", got)
	}
	if got := (Runner{ShellDeadline: time.Second / 7, Getenv: env("https://iris.example.com")}).shellLimit(); got != time.Second/7 {
		t.Fatalf("an explicit budget: %s", got)
	}
}

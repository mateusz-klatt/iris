package hook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"syscall"
	"testing"

	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// Every text failureText writes maps to a category of its own, so a reader that trusts only
// FailureCategory still tells the failures apart.
func TestFailureCategoryKnowsWhatFailureTextWrites(t *testing.T) {
	var syntax *json.SyntaxError
	if err := json.Unmarshal([]byte("{"), new(any)); !errors.As(err, &syntax) {
		t.Fatalf("no syntax error: %v", err)
	}
	var mismatch *json.UnmarshalTypeError
	if err := json.Unmarshal([]byte(`{"a":1}`), new(struct{ A string })); !errors.As(err, &mismatch) {
		t.Fatalf("no type error: %v", err)
	}
	cases := map[error]string{
		context.DeadlineExceeded:              "hook deadline exceeded",
		context.Canceled:                      "hook operation canceled",
		errNoSession:                          errNoSession.Error(),
		failPanic:                             string(failPanic),
		failPanicShell:                        string(failPanicShell),
		failUnknownClient:                     string(failUnknownClient),
		failNoHub:                             string(failNoHub),
		failNoPaths:                           string(failNoPaths),
		failBadTouch:                          string(failBadTouch),
		hookFailure(shellOverBudget):          shellOverBudget,
		syntax:                                "invalid JSON",
		mismatch:                              "invalid JSON field type",
		&hubclient.Error{Code: "lost"}:        "hub request failed: lost",
		&hubclient.Error{Code: "teapot"}:      "hub request failed",
		syscall.Errno(2):                      syscall.Errno(2).Error(),
		io.ErrUnexpectedEOF:                   "incomplete hook input or hub response",
		errors.New("secret at /home/someone"): "dependency failure",
	}
	for err, want := range cases {
		if got := FailureCategory(failureText(err)); got != want {
			t.Errorf("FailureCategory(failureText(%v)) = %q, want %q", err, got, want)
		}
	}
}

// A line failureText never writes, from an older iris or another writer, is "other", and
// so is any stage a run never records.
func TestFailureCategoryAndStageHideUnknownText(t *testing.T) {
	for _, text := range []string{
		"unknown tool secret-tool", "hub request failed: secret-code", "Post https://secret.example/v1",
		"invalid JSONsecret", "", "hook deadline exceeded ",
	} {
		if got := FailureCategory(text); got != "other" {
			t.Errorf("FailureCategory(%q) = %q, want other", text, got)
		}
	}
	for _, stage := range []string{"secret stage", "", "Session"} {
		if got := FailureStage(stage); got != "other" {
			t.Errorf("FailureStage(%q) = %q, want other", stage, got)
		}
	}
	for _, stage := range []string{"session", "shell reconcile", stageObserved} {
		if got := FailureStage(stage); got != stage {
			t.Errorf("FailureStage(%q) = %q", stage, got)
		}
	}
}

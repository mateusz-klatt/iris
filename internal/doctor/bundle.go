package doctor

import (
	"fmt"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/guard"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/integrate"
)

// Bundle is what `iris doctor --bundle` prints for someone helping from elsewhere. It is an
// allowlist: every field is a version, a state word, a flag or a count, built field by field
// from a doctor run. No URL, path, host name, token, message, tool name or free-text detail is
// copied in, because those are where secrets and private data sit, and a bundle is made to be
// pasted into an issue or a chat.
type Bundle struct {
	Iris         BundleIris     `json:"iris"`
	Hub          BundleHub      `json:"hub"`
	Clients      []BundleClient `json:"clients"`
	Failures     []BundleCount  `json:"failures"`
	Observations []BundleCount  `json:"observations"`
	Guard        string         `json:"guard"`
	CaseWarning  bool           `json:"case_warning"`
	// LineEndingsWarning says doctor printed a line endings warning, without its counts.
	LineEndingsWarning bool `json:"line_endings_warning"`
}

// BundleIris is the build and platform of this iris.
type BundleIris struct {
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Go      string `json:"go"`
}

// BundleHub says whether the hub could be reached and the token was taken, never which hub or
// what token. Source is the kind of setting that named the hub: command line, environment,
// file or default.
type BundleHub struct {
	Source           string `json:"source"`
	Up               bool   `json:"up"`
	TokenOK          bool   `json:"token_ok"`
	TokenUnchecked   bool   `json:"token_unchecked"`
	TokenUnavailable bool   `json:"token_unavailable"`
	Stored           bool   `json:"report_stored"`
	DatabaseBytes    int64  `json:"database_bytes,omitempty"`
}

// BundleClient is one client's doctor row without its detail, which can name paths.
type BundleClient struct {
	Client  string `json:"client"`
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
}

// BundleCount is how often one kind of hook failure or observation happened in 24 hours.
type BundleCount struct {
	Client   string `json:"client"`
	Stage    string `json:"stage"`
	Category string `json:"category"`
	Count    int    `json:"count"`
}

// NewBundle reduces a doctor run, and the hook logs beside its token, to a Bundle.
func NewBundle(res Result, version string, now time.Time) Bundle {
	b := Bundle{
		Iris:               BundleIris{Version: version, OS: runtime.GOOS, Arch: runtime.GOARCH, Go: runtime.Version()},
		Hub:                BundleHub{Source: oneOf(word(res.HubSource), "command line", "default", "file", "environment"), Up: res.HubUp, TokenOK: res.TokenOK, TokenUnchecked: res.TokenUnchecked, TokenUnavailable: res.TokenUnavailable, Stored: res.Stored, DatabaseBytes: res.DatabaseBytes},
		Clients:            []BundleClient{},
		Guard:              oneOf(word(res.Guard), guard.StateInstalled, guard.StateMissing, guard.StateOther, "not in a git repository", "unknown"),
		CaseWarning:        res.Case != "",
		LineEndingsWarning: res.LineEndings != "",
		Failures:           []BundleCount{},
		Observations:       []BundleCount{},
	}
	for _, c := range res.Report.Clients {
		client := integrate.Canonical(c.Client)
		if client == "" {
			continue
		}
		status := oneOf(c.Status, StatusOK, StatusNotConfigured, StatusNeedsHuman, StatusSilent, StatusSlow)
		b.Clients = append(b.Clients, BundleClient{Client: client, Status: status, Version: bareVersion(c.Version)})
	}
	// A log that cannot be read leaves its counts empty; doctor's own report says why.
	failures, _ := hook.ReadRecentFailures(res.tokenFile, failureRead)
	observations, _ := hook.ReadRecentObservations(res.tokenFile, failureRead)
	b.Failures = counts(failures, now, func(f hook.Failure) string { return hook.FailureCategory(f.Error) })
	b.Observations = counts(observations, now, func(f hook.Failure) string { return observationKind(f.Error) })
	return b
}

// word is the state a doctor line starts with, before any path or hint in parentheses:
// "installed (/repo/.git/hooks/pre-commit)" is "installed".
func word(line string) string {
	state, _, _ := strings.Cut(line, " (")
	return strings.TrimSpace(state)
}

// oneOf is value when it is one of the known values, and "other" when it is not, so a line
// some other code formats differently later cannot carry a path or a name into a bundle.
func oneOf(value string, known ...string) string {
	if slices.Contains(known, value) {
		return value
	}
	return "other"
}

// bareVersion is the first x.y.z in a client's version output, and nothing when there is
// none: the rest of that output is the client's to choose.
func bareVersion(text string) string {
	v, ok := parsedVersion(text)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
}

// observationKind keeps an observation's kind and drops what follows it, a tool name or a
// client, which comes from the client: "unknown tool view" is "unknown tool".
func observationKind(text string) string {
	for _, kind := range []string{"unknown tool", "cross-load", binding.ObservedTwoSessions, binding.ObservedUnusable, binding.ObservedRebound} {
		if strings.HasPrefix(text, kind) {
			return kind
		}
	}
	return "other"
}

// counts groups the last 24 hours' entries of known clients by client, stage and category.
// The stage and the category are each mapped onto a fixed set, never copied: the log is a
// file any older iris or other writer may have added to.
func counts(entries []hook.Failure, now time.Time, category func(hook.Failure) string) []BundleCount {
	by := map[BundleCount]int{}
	for _, e := range entries {
		client := integrate.Canonical(e.Client)
		if client == "" || now.Sub(e.Time) > failureWindow {
			continue
		}
		by[BundleCount{Client: client, Stage: hook.FailureStage(e.Stage), Category: category(e)}]++
	}
	out := []BundleCount{}
	for k, n := range by {
		k.Count = n
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Client != b.Client {
			return a.Client < b.Client
		}
		if a.Stage != b.Stage {
			return a.Stage < b.Stage
		}
		return a.Category < b.Category
	})
	return out
}

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A path follows keys and indexes, counts negative indexes from the end, and gives nil for any
// step that does not fit rather than panicking.
func TestAtFollowsKeysAndIndexes(t *testing.T) {
	doc := map[string]any{"jobs": map[string]any{"a": map[string]any{"steps": []any{"first", "second"}}}}
	for _, c := range []struct {
		path []any
		want any
	}{
		{[]any{"jobs", "a", "steps", 0}, "first"},
		{[]any{"jobs", "a", "steps", -1}, "second"},
		{[]any{"jobs", "a", "steps", 2}, nil},
		{[]any{"jobs", "a", "steps", -3}, nil},
		{[]any{"jobs", "a", "steps", "x"}, nil},
		{[]any{"jobs", 0}, nil},
		{[]any{"jobs", 1.5}, nil},
		{[]any{"missing", "deeper"}, nil},
	} {
		if got := at(doc, c.path...); got != c.want {
			t.Errorf("at(%v) = %v, want %v", c.path, got, c.want)
		}
	}
}

// A mutation replaces a map value or a list element in place, and a path that does not exist is
// an error, never a silent no-op that would let a mutation test nothing.
func TestSetReplacesOnlyWhatExists(t *testing.T) {
	doc := map[string]any{"jobs": map[string]any{"steps": []any{"a", "b"}}}
	if err := set(doc, []any{"jobs", "steps", -1}, "z"); err != nil || at(doc, "jobs", "steps", 1) != "z" {
		t.Fatalf("list: %v %v", err, doc)
	}
	if err := set(doc, []any{"jobs", "new"}, 1); err != nil || at(doc, "jobs", "new") != 1 {
		t.Fatalf("map: %v %v", err, doc)
	}
	for _, path := range [][]any{
		{},
		{"jobs", "steps", "x"},
		{"jobs", 0},
		{"jobs", "steps", 2},
		{"jobs", "steps", -3},
		{"jobs", 1.5},
	} {
		if err := set(doc, path, "v"); err == nil {
			t.Errorf("set(%v) succeeded", path)
		}
	}
}

// A clone shares nothing with its original, so a mutation of the copy leaves the real document
// as it was for the next mutation.
func TestCloneIsDeep(t *testing.T) {
	doc := map[string]any{"a": []any{map[string]any{"b": "c"}}, "n": 1}
	copied := clone(doc)
	if err := set(copied, []any{"a", 0, "b"}, "changed"); err != nil {
		t.Fatal(err)
	}
	if at(doc, "a", 0, "b") != "c" || !equal(at(copied, "n"), 1) {
		t.Fatalf("original %v, copy %v", doc, copied)
	}
}

// The text searches see keys and string values at any depth, as a search of the YAML dump did,
// and nothing else.
func TestTextSearchesSeeKeysAndValues(t *testing.T) {
	doc := map[string]any{"jobs": []any{map[string]any{"env": map[string]any{"X": expression("SECRETS.x")}}, 3, true}}
	if !mentionsSecrets(doc) {
		t.Error("a value in any case was missed")
	}
	if !mentionsSecrets(map[string]any{"secrets": 1}) {
		t.Error("a key was missed")
	}
	if mentionsSecrets(map[string]any{"a": []any{1, nil, "plain"}}) {
		t.Error("found a secret where there is none")
	}
	isEnv := func(k string) bool { return k == "environment" }
	if !anyKey(map[string]any{"jobs": []any{map[string]any{"environment": "x"}}}, isEnv) {
		t.Error("a nested key was missed")
	}
	if anyKey(map[string]any{"a": []any{"environment"}, "b": "environment"}, isEnv) {
		t.Error("a value was taken for a key")
	}
}

// Small accessors give the zero value for the wrong type, lines splits a script as splitlines
// did, and a step is found by its identity, not by an equal copy.
func TestSmallParts(t *testing.T) {
	if text(1) != "" || items("x") != nil || fields("x") != nil || keys("x") == nil || len(keys("x")) != 0 {
		t.Error("an accessor did not give its zero value")
	}
	if lines(nil) != nil || !equal(lines("a\r\nb\n"), []string{"a", "b"}) {
		t.Errorf("lines: %q", lines("a\r\nb\n"))
	}
	if !equal(with(map[string]any{"a": "b"}, "c", "d"), map[string]any{"a": "b", "c": "d"}) || !equal(with(nil, "c", "d"), map[string]any{"c": "d"}) {
		t.Error("with")
	}
	step := map[string]any{"name": "s", "id": "i", "uses": "o/r@x"}
	job := map[string]any{"steps": []any{map[string]any{"name": "first"}, step}}
	if index(job, step) != 1 || index(job, map[string]any{"name": "s", "id": "i", "uses": "o/r@x"}) != -1 || index(job, "x") != -1 {
		t.Error("index")
	}
	if stepWithID(job, "i") == nil || stepWithID(job, "none") != nil || len(stepsUsing(job, "o/r")) != 1 || len(stepsNamed(job, "s")) != 1 {
		t.Error("step lookups")
	}
	if !strings.HasPrefix(expression("x"), "$") {
		t.Error("expression")
	}
}

// A file that is missing or not YAML is an error that names it.
func TestLoadReportsUnreadableFiles(t *testing.T) {
	dir := t.TempDir()
	if _, err := load(filepath.Join(dir, "missing.yml")); err == nil {
		t.Error("a missing file loaded")
	}
	bad := filepath.Join(dir, "bad.yml")
	if err := os.WriteFile(bad, []byte("a: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := load(bad); err == nil || !strings.Contains(err.Error(), "bad.yml") {
		t.Errorf("bad YAML: %v", err)
	}
}

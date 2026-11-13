package main

import (
	"fmt"
	"maps"
	"os"
	"reflect"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// load reads a YAML file into the plain values the checks compare: maps with string keys,
// slices, strings, ints, floats, bools and nil.
func load(path string) (any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return v, nil
}

// at follows keys and indexes through a decoded document. A negative index counts from the end,
// as in Python. A step that does not fit gives nil, so a check fails on a comparison and never
// panics on a workflow of the wrong shape.
func at(v any, path ...any) any {
	for _, k := range path {
		switch k := k.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[k]
		case int:
			s, ok := v.([]any)
			if !ok {
				return nil
			}
			if k < 0 {
				k += len(s)
			}
			if k < 0 || k >= len(s) {
				return nil
			}
			v = s[k]
		default:
			return nil
		}
	}
	return v
}

// text is v as a string, or "" when it is not one.
func text(v any) string {
	s, _ := v.(string)
	return s
}

// items is v as a list, or nil when it is not one.
func items(v any) []any {
	s, _ := v.([]any)
	return s
}

// fields is v as a map, or nil when it is not one.
func fields(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// equal compares decoded values exactly, as Python's == compares them.
func equal(a, b any) bool { return reflect.DeepEqual(a, b) }

// keys is a map's keys in order.
func keys(v any) []string {
	m := fields(v)
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// lines splits a run script as Python's splitlines does for the scripts checked here.
func lines(v any) []string {
	s := strings.TrimSuffix(strings.ReplaceAll(text(v), "\r\n", "\n"), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// clone copies a decoded document deeply, so a mutation never reaches the original.
func clone(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[k] = clone(x)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = clone(x)
		}
		return out
	default:
		return v
	}
}

// set replaces the value at path in a document, as `target[path[-1]] = value` did after the guard
// walked the rest of the path. A path that does not exist is an error in the mutation list.
func set(root any, path []any, value any) error {
	if len(path) == 0 {
		return fmt.Errorf("empty mutation path")
	}
	parent := at(root, path[:len(path)-1]...)
	switch last := path[len(path)-1].(type) {
	case string:
		m, ok := parent.(map[string]any)
		if !ok {
			return fmt.Errorf("mutation path %v does not lead to a map", path)
		}
		m[last] = value
	case int:
		s, ok := parent.([]any)
		if !ok {
			return fmt.Errorf("mutation path %v does not lead to a list", path)
		}
		if last < 0 {
			last += len(s)
		}
		if last < 0 || last >= len(s) {
			return fmt.Errorf("mutation path %v is out of range", path)
		}
		s[last] = value
	default:
		return fmt.Errorf("mutation path %v has a key of type %T", path, last)
	}
	return nil
}

// with is a copy of the map v with key set to value, as Python's dict(v, key=value).
func with(v any, key string, value any) map[string]any {
	out := map[string]any{}
	maps.Copy(out, fields(clone(v)))
	out[key] = value
	return out
}

// anyText reports whether f holds for any key or string value in v, at any depth. It stands in
// for searching the YAML dump of v: a dump holds no other text.
func anyText(v any, f func(string) bool) bool {
	switch v := v.(type) {
	case map[string]any:
		for k, x := range v {
			if f(k) || anyText(x, f) {
				return true
			}
		}
	case []any:
		for _, x := range v {
			if anyText(x, f) {
				return true
			}
		}
	case string:
		return f(v)
	}
	return false
}

// anyKey reports whether f holds for any map key in v, at any depth.
func anyKey(v any, f func(string) bool) bool {
	switch v := v.(type) {
	case map[string]any:
		for k, x := range v {
			if f(k) || anyKey(x, f) {
				return true
			}
		}
	case []any:
		for _, x := range v {
			if anyKey(x, f) {
				return true
			}
		}
	}
	return false
}

// mentionsSecrets is the guards' test for a secret reference: "secrets" in any letter case.
func mentionsSecrets(v any) bool {
	return anyText(v, func(s string) bool { return strings.Contains(strings.ToLower(s), "secrets") })
}

// expression writes a GitHub expression without putting its delimiters in this source.
func expression(value string) string { return "$" + "{{ " + value + " }}" }

// stepsNamed is every step of a job with the given name.
func stepsNamed(job any, name string) []any {
	var out []any
	for _, step := range items(at(job, "steps")) {
		if text(at(step, "name")) == name {
			out = append(out, step)
		}
	}
	return out
}

// stepsUsesPrefix is every step of a job whose uses: starts with prefix.
func stepsUsesPrefix(job any, prefix string) []any {
	var out []any
	for _, step := range items(at(job, "steps")) {
		if strings.HasPrefix(text(at(step, "uses")), prefix) {
			out = append(out, step)
		}
	}
	return out
}

// stepsUsing is every step of a job whose uses: names the action, at any revision.
func stepsUsing(job any, action string) []any { return stepsUsesPrefix(job, action+"@") }

// stepsWithID is every step of a job with the given id.
func stepsWithID(job any, id string) []any {
	var out []any
	for _, step := range items(at(job, "steps")) {
		if text(at(step, "id")) == id {
			out = append(out, step)
		}
	}
	return out
}

// stepWithID is the first step of a job with the given id, or nil.
func stepWithID(job any, id string) any { return at(stepsWithID(job, id), 0) }

// index is the position of step in a job's steps, compared by identity, or -1.
func index(job any, step any) int {
	for i, s := range items(at(job, "steps")) {
		if sameStep(s, step) {
			return i
		}
	}
	return -1
}

// sameStep reports whether two decoded steps are the same map, not merely equal ones.
func sameStep(a, b any) bool {
	ma, oka := a.(map[string]any)
	mb, okb := b.(map[string]any)
	return oka && okb && reflect.ValueOf(ma).Pointer() == reflect.ValueOf(mb).Pointer()
}

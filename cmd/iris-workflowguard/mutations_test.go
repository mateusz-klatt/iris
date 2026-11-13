package main

import (
	"fmt"
	"path/filepath"
	"testing"
)

// Every mutation must change the real document, and no two may be the same change: a mutation
// built from the wrong value (an empty script, a replacement whose text is absent) would still
// be rejected and still be counted, while testing nothing of its own.
func TestEveryMutationIsADistinctChange(t *testing.T) {
	load := func(name string) any {
		t.Helper()
		doc, err := load(filepath.Join(repoRoot, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
	release, config := load(".github/workflows/release.yml"), load(".goreleaser.yaml")
	proof, macOS := load(".github/workflows/signing-proof.yml"), load(".github/workflows/macos-signing-proof.yml")
	signing, err := signingMutations(proof)
	if err != nil {
		t.Fatal(err)
	}
	for _, guard := range []struct {
		name      string
		doc       any
		mutations []mutation
		want      int
	}{
		{"release", map[string]any{"release": release, "config": config}, append(prefixed("release", releaseMutations(release)), prefixed("config", configMutations())...), 82},
		{"signing-proof", proof, signing, 9},
		{"macos-proof", macOS, macOSMutations(macOS), 66},
	} {
		if len(guard.mutations) != guard.want {
			t.Errorf("%s: %d mutations, want %d", guard.name, len(guard.mutations), guard.want)
		}
		seen := map[string]int{}
		for i, m := range guard.mutations {
			if equal(at(guard.doc, m.path...), m.value) {
				t.Errorf("%s: mutation %d at %v changes nothing", guard.name, i, m.path)
			}
			key := fmt.Sprintf("%#v=%#v", m.path, m.value)
			if j, ok := seen[key]; ok {
				t.Errorf("%s: mutations %d and %d are the same change at %v", guard.name, j, i, m.path)
			}
			seen[key] = i
		}
	}
}

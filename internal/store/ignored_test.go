package store

import "testing"

// D10: ignored paths take no automatic claim, but an explicit claim still applies to
// them. Another session's pre-edit touch must therefore be refused, whether the
// explicit claim is exact, a prefix or repository-wide.
func TestExplicitClaimsProtectIgnoredPathsFromAnotherSessionsEdit(t *testing.T) {
	for name, claim := range map[string]func(Request) Request{
		"exact":  func(r Request) Request { return claimRequest(r, "c", "go.sum", "exact") },
		"prefix": func(r Request) Request { return claimRequest(r, "c", "generated", "prefix") },
		"repository-wide": func(r Request) Request {
			r = claimRequest(r, "c", "", "prefix")
			r.RepoWide = true
			return r
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _ := testStore(t)
			holder, _ := session(t, s, 1)
			editor, _ := session(t, s, 2)
			call(t, s, "claim", claim(holder))
			path := "go.sum"
			if name == "prefix" {
				path = "generated/types.go"
			}
			out := call(t, s, "touch", touchRequest(editor, "edit", path))
			if out["action"] != "deny" || len(out["conflicts"].([]map[string]any)) != 1 {
				t.Fatalf("an explicit claim on an ignored path was not enforced: %v", out)
			}
			// The holder itself still edits freely, and still takes no automatic claim there.
			mine := call(t, s, "touch", touchRequest(holder, "own", path))
			if mine["action"] != "allow" || len(mine["granted"].([]string)) != 0 {
				t.Fatal(mine)
			}
		})
	}
}

// Without an explicit claim an ignored path is advisory: allowed, never auto-claimed.
func TestAnIgnoredPathWithoutAnExplicitClaimIsAllowed(t *testing.T) {
	s, _, _ := testStore(t)
	a, _ := session(t, s, 1)
	b, _ := session(t, s, 2)
	call(t, s, "touch", touchRequest(a, "a", "go.sum"))
	if out := call(t, s, "touch", touchRequest(b, "b", "go.sum")); out["action"] != "allow" {
		t.Fatal(out)
	}
}

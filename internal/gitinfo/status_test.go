package gitinfo

import "testing"

func TestPorcelainPaths(t *testing.T) {
	out, err := ParseStatus([]byte("R  new name\x00old\nname\x00?? untracked\x00 D gone\x00"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"new name", "old\nname", "untracked", "gone"} {
		if out[p] == "" {
			t.Fatalf("missing %q: %v", p, out)
		}
	}
	if _, err = ParseStatus([]byte("R  missing\x00")); err == nil {
		t.Fatal("truncated rename accepted")
	}
}

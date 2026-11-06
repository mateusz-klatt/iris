package hook

import "testing"

func TestParsePatch(t *testing.T) {
	text := "" +
		"*** Begin Patch\n" +
		"*** Update File: src/old.go\n" +
		"*** Move to: src/new.go\n" +
		"@@\n" +
		"-a\n" +
		"+b\n" +
		"*** Delete File: src/gone.go\n" +
		"*** Add File: src/fresh.go\n" +
		"+x\n" +
		"*** End Patch\n"
	paths, from, to := parsePatch(text)
	want := []string{"src/old.go", "src/new.go", "src/gone.go", "src/fresh.go"}
	if len(paths) != len(want) {
		t.Fatalf("paths %v", paths)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("paths %v", paths)
		}
	}
	if from != "src/old.go" || to != "src/new.go" {
		t.Fatalf("rename %s -> %s", from, to)
	}
}

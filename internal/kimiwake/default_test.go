package kimiwake

import (
	"os"
	"runtime"
	"testing"
)

// withDefault makes "default" mean on or off for one test, as it does where Kimi's native acceptance
// passed or has not, and restores this platform's value afterwards.
func withDefault(t *testing.T, on bool) {
	t.Helper()
	old := DefaultOn
	t.Cleanup(func() { DefaultOn = old })
	DefaultOn = on
}

// "default" is on only where Kimi's native acceptance passed, which is Linux (ADR 030). An explicit
// choice means the same everywhere, and a file without a choice never enables wake.
func TestTheDefaultFollowsThePlatformsAcceptance(t *testing.T) {
	if DefaultOn != (runtime.GOOS == "linux") {
		t.Fatalf("default on %v on %s", DefaultOn, runtime.GOOS)
	}
	root := t.TempDir()
	for _, defaultOn := range []bool{false, true} {
		withDefault(t, defaultOn)
		for text, want := range map[string]bool{"default\n": defaultOn, "on\n": true, "off\n": false, "\n": false} {
			if err := os.WriteFile(PreferencePath(root), []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := Enabled(root); got != want {
				t.Fatalf("default on %v, %q: enabled %v, want %v", defaultOn, text, got, want)
			}
		}
	}
}

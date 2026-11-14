package main

import (
	"path/filepath"
	"testing"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/integrate"
)

// The command records the choice its flags make, keeps it when they make none, and takes it back
// with the integration.
func TestIntegrateKimiWakeFlagsAndPreferencePreservation(t *testing.T) {
	home := filepath.Join(t.TempDir(), "kimi")
	t.Setenv("KIMI_CODE_HOME", home)
	t.Setenv("IRIS_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv("PATH", t.TempDir())
	pinned, err := config.PinnedDir()
	if err != nil {
		t.Fatal(err)
	}
	// The watcher reads only the pinned configuration, so --wake needs it to be the one in use.
	t.Setenv("IRIS_TOKEN_FILE", filepath.Join(pinned, "hub.token"))
	for _, tc := range []struct {
		args    []string
		enabled bool
	}{
		{[]string{"kimi"}, false},
		{[]string{"kimi", "--no-wake"}, false},
		{[]string{"kimi"}, false},
		{[]string{"--wake", "kimi"}, true},
		{[]string{"kimi"}, true},
		{[]string{"kimi", "--wake=false"}, false},
		{[]string{"kimi", "--no-wake=false"}, true},
	} {
		out, errText := capture(t, func() {
			if code := runIntegrate(tc.args); code != 0 {
				t.Fatalf("%v returned %d", tc.args, code)
			}
		})
		configured, enabled, err := integrate.KimiWake(integrate.Layout{})
		if err != nil || !configured || enabled != tc.enabled || out != "integrated kimi\n" || errText != "" {
			t.Fatalf("%v: configured/enabled %v/%v, %v, stdout %q, stderr %q", tc.args, configured, enabled, err, out, errText)
		}
	}
	out, errText := capture(t, func() {
		if code := runIntegrate([]string{"--remove", "kimi"}); code != 0 {
			t.Fatal("removal failed")
		}
	})
	configured, enabled, err := integrate.KimiWake(integrate.Layout{})
	if err != nil || configured || enabled || out != "removed kimi\n" || errText != "" {
		t.Fatalf("after removal: configured/enabled %v/%v, %v, stdout %q, stderr %q", configured, enabled, err, out, errText)
	}
}

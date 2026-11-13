package main

import (
	"path/filepath"
	"testing"

	"github.com/mateusz-klatt/iris/internal/integrate"
)

func TestIntegrateKimiWakeFlagsAndPreferencePreservation(t *testing.T) {
	home := filepath.Join(t.TempDir(), "kimi")
	t.Setenv("KIMI_CODE_HOME", home)
	t.Setenv("IRIS_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv("PATH", t.TempDir())
	for _, tc := range []struct {
		args    []string
		enabled bool
	}{
		{[]string{"kimi"}, true},
		{[]string{"kimi", "--no-wake"}, false},
		{[]string{"kimi"}, false},
		{[]string{"--wake", "kimi"}, true},
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
}

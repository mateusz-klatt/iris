package integrate

import (
	"errors"
	"testing"
)

func TestKimiPlatformCommand(t *testing.T) {
	for _, goos := range []string{"windows", "linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			l := testLayout(t)
			l.GOOS = goos
			bin := `C:\Program Files\Iris\iris.exe`
			l.LookPath = func(name string) (string, error) {
				if name == "iris" {
					return bin, nil
				}
				return "", errors.New("not found")
			}
			c, _ := ByName(Kimi)
			if err := c.Install(l, nil); err != nil {
				t.Fatal(err)
			}
			in, err := c.Canary(l)
			if err != nil {
				t.Fatal(err)
			}
			want := hookShell(bin, Kimi, "SessionStart")
			if goos == "windows" {
				want = hookCmd(bin, Kimi, "SessionStart")
			}
			if in.Shell != want || in.Kind != clientShell(goos, Kimi) {
				t.Fatalf("%+v want %q", in, want)
			}
		})
	}
}

package integrate

import (
	"testing"

	"github.com/mateusz-klatt/iris/internal/kimiwake"
)

// withKimiDefault makes "default" mean on or off for one test, as it does where Kimi's native
// acceptance passed or has not, and restores this platform's value afterwards.
func withKimiDefault(t *testing.T, on bool) {
	t.Helper()
	old := kimiwake.DefaultOn
	t.Cleanup(func() { kimiwake.DefaultOn = old })
	kimiwake.DefaultOn = on
}

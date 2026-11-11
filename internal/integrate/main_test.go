package integrate

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	code := m.Run()
	CleanupFake()
	os.Exit(code)
}

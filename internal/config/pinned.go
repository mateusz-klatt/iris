package config

import (
	"os"
	"runtime"
)

var (
	pinnedDirFn        = pinnedConfigDirOS
	pinnedRuntimeEnvFn = pinnedRuntimeEnvOS
)

// PinnedDir returns the OS account's default Iris configuration directory
// without consulting HOME, XDG_CONFIG_HOME or APPDATA.
func PinnedDir() (string, error) {
	return pinnedDirFn()
}

// PinnedRuntimeEnv returns the minimal trusted OS runtime environment variables
// (OS-account HOME on Unix, SYSTEMROOT on Windows) for a scrubbed subprocess.
func PinnedRuntimeEnv() ([]string, error) {
	return pinnedRuntimeEnvFn()
}

func testPinnedRuntimeEnv(goos, dir, systemRoot string) []string {
	if goos == "windows" && systemRoot != "" {
		return []string{"SYSTEMROOT=" + systemRoot}
	}
	return []string{"HOME=" + dir}
}

// SetPinnedForTest redirects PinnedDir and PinnedRuntimeEnv to dir for the
// duration of a test or TestMain, returning a function that restores the
// previous resolvers. Production code never calls it.
func SetPinnedForTest(dir string) func() {
	prevDir, prevEnv := pinnedDirFn, pinnedRuntimeEnvFn
	env := testPinnedRuntimeEnv(runtime.GOOS, dir, os.Getenv("SYSTEMROOT"))
	pinnedDirFn = func() (string, error) { return dir, nil }
	pinnedRuntimeEnvFn = func() ([]string, error) {
		out := make([]string, len(env))
		copy(out, env)
		return out, nil
	}
	return func() {
		pinnedDirFn, pinnedRuntimeEnvFn = prevDir, prevEnv
	}
}

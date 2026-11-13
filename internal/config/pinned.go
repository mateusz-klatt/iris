package config

// PinnedDir returns the OS account's default Iris configuration directory
// without consulting HOME, XDG_CONFIG_HOME or APPDATA.
func PinnedDir() (string, error) {
	return pinnedConfigDirOS()
}

// PinnedRuntimeEnv returns the minimal trusted OS runtime environment variables
// (OS-account HOME on Unix, SYSTEMROOT on Windows) for a scrubbed subprocess.
func PinnedRuntimeEnv() ([]string, error) {
	return pinnedRuntimeEnvOS()
}

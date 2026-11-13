package hubclient

import (
	"fmt"
	"path/filepath"

	"github.com/mateusz-klatt/iris/internal/config"
)

// PinnedConfig resolves Iris configuration strictly from the OS account's default
// configuration directory, ignoring IRIS_HUB_URL, IRIS_TOKEN_FILE, XDG_CONFIG_HOME,
// HOME and APPDATA environment overrides.
type PinnedConfig struct {
	ConfigDir func() (string, error)
	ReadFile  func(string) ([]byte, error)
}

// NewPinnedConfig loads the OS-account-pinned Iris configuration and returns its directory.
func NewPinnedConfig() (Resolved, string, error) {
	return (PinnedConfig{}).Load()
}

func (p PinnedConfig) configDir() (string, error) {
	if p.ConfigDir != nil {
		return p.ConfigDir()
	}
	return PinnedConfigDir()
}

// Load reads client.json and hub.token exclusively from the OS-account-pinned directory.
func (p PinnedConfig) Load() (Resolved, string, error) {
	dir, err := p.configDir()
	if err != nil {
		return Resolved{err: err}, "", err
	}
	defaultToken := filepath.Join(dir, "hub.token")
	r := (EnvConfig{
		Getenv:           func(string) string { return "" },
		ReadFile:         p.ReadFile,
		DefaultTokenFile: defaultToken,
	}).Load()
	if r.err == nil && filepath.Clean(r.TokenFile) != filepath.Clean(defaultToken) {
		r.err = fmt.Errorf("read %s: token_file must stay at %s", r.ConfigFile, defaultToken)
	}
	return r, dir, r.err
}

// PinnedConfigDir returns the OS account's default Iris configuration directory
// without consulting HOME, XDG_CONFIG_HOME or APPDATA.
func PinnedConfigDir() (string, error) {
	return config.PinnedDir()
}

// PinnedRuntimeEnv returns the minimal trusted OS runtime environment variables
// (OS-account HOME on Unix, SYSTEMROOT on Windows) for a scrubbed subprocess.
func PinnedRuntimeEnv() ([]string, error) {
	return config.PinnedRuntimeEnv()
}

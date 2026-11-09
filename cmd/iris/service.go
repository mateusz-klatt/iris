package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/service"
)

// hostService is the process boundary for the user service manager.
var hostService = service.Host

// absPath is the process boundary for making the service binary absolute.
var absPath = filepath.Abs

func runService(args []string) int {
	if len(args) == 0 || (args[0] != "install" && args[0] != "uninstall" && args[0] != "status" && args[0] != "start" && args[0] != "stop") {
		fmt.Fprintln(os.Stderr, "usage: iris service install|uninstall|status|start|stop")
		return 2
	}
	fs := flag.NewFlagSet("service", flag.ContinueOnError)
	listen := fs.String("listen", "", "hub listen address")
	token := fs.String("token-file", "", "token file")
	db := fs.String("db", "", "database file")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return 2
	}
	ctx := context.Background()
	if args[0] == "install" {
		if err := installUserService(ctx, *listen, *token, *db); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("installed")
		return 0
	}
	// Every other command acts on the service as installed, not on today's defaults.
	saved, err := loadServiceSettings()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	mgr, err := userServiceDB(first(*listen, saved.Listen), first(*token, saved.TokenFile), first(*db, saved.Database))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	switch args[0] {
	case "start":
		if err := mgr.Start(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	case "stop":
		if err := mgr.Stop(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	case "uninstall":
		if err := mgr.Uninstall(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := removeServiceSettings(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("uninstalled")
	case "status":
		st, err := mgr.Status(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("installed: %v\nactive: %v\n", st.Installed, st.Active)
		if st.Detail != "" {
			fmt.Println(st.Detail)
		}
		return statusExit(st)
	}
	return 0
}

// statusExit follows systemctl and LSB, so that scripts can tell the states
// apart on every OS: 0 running, 3 installed but not running, 4 not installed.
// A status that could not be read at all is 1, as for the other commands.
func statusExit(st service.Status) int {
	switch {
	case !st.Installed:
		return 4
	case !st.Active:
		return 3
	}
	return 0
}

// serviceSettings are what `iris service install` was given. Lifecycle commands need
// them because the manager finds a hub through its command line.
type serviceSettings struct {
	Listen    string `json:"listen,omitempty"`
	TokenFile string `json:"token_file,omitempty"`
	Database  string `json:"database,omitempty"`
}

func serviceSettingsPath() (string, error) {
	cfg, err := config.Default()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(cfg.TokenFile), "service.json"), nil
}

func loadServiceSettings() (serviceSettings, error) {
	var saved serviceSettings
	path, err := serviceSettingsPath()
	if err != nil {
		return saved, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return saved, nil
	}
	if err != nil {
		return saved, err
	}
	if err = json.Unmarshal(raw, &saved); err != nil {
		return saved, fmt.Errorf("read %s: %w", path, err)
	}
	return saved, nil
}

func saveServiceSettings(saved serviceSettings) error {
	path, err := serviceSettingsPath()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, _ := json.Marshal(saved) // strings always encode
	return os.WriteFile(path, raw, 0o600)
}

func removeServiceSettings() error {
	path, err := serviceSettingsPath()
	if err != nil {
		return err
	}
	if err = os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// installUserService installs the service and records its settings, for the
// commands that have to find this same hub later.
func installUserService(ctx context.Context, listen, tokenFile, database string) error {
	mgr, err := userServiceDB(listen, tokenFile, database)
	if err != nil {
		return err
	}
	if err = mgr.Install(ctx); err != nil {
		return err
	}
	return saveServiceSettings(serviceSettings{Listen: listen, TokenFile: absolute(tokenFile), Database: absolute(database)})
}

// absolute keeps an empty setting empty, which means the default. userServiceDB has
// already resolved these paths, so this cannot fail where that did not.
func absolute(path string) string {
	if path == "" {
		return ""
	}
	path, _ = absPath(path)
	return path
}

func first(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// userService builds the per-user manager. listen and tokenFile override the
// defaults from config when setup points the hub at a specific loopback URL.
func userService(listen, tokenFile string) (service.Manager, error) {
	return userServiceDB(listen, tokenFile, "")
}

// serviceBinary prefers iris on PATH, which survives an upgrade. A Windows logon can
// start the autostart command before PATH is complete, so the running binary is the
// fallback, not a failure.
func serviceBinary(look func(string) (string, error), self func() (string, error)) (string, error) {
	if bin, err := look("iris"); err == nil {
		return absPath(bin)
	}
	return self()
}

func userServiceDB(listen, tokenFile, database string) (service.Manager, error) {
	bin, err := serviceBinary(exec.LookPath, os.Executable)
	if err != nil {
		return nil, err
	}
	// Both come from the environment. Which of them a missing home fails depends on the system:
	// macOS has no other source for either, while Linux and Windows can still name a config folder.
	cfg, err := config.Default()
	home, homeErr := os.UserHomeDir()
	if err = errors.Join(err, homeErr); err != nil {
		return nil, err
	}
	if listen == "" {
		listen = cfg.Listen
	}
	if tokenFile == "" {
		tokenFile = cfg.TokenFile
	}
	if database == "" {
		database = cfg.Database
	}
	// The autostart command runs from another directory.
	if tokenFile, err = absPath(tokenFile); err != nil {
		return nil, err
	}
	if database, err = absPath(database); err != nil {
		return nil, err
	}
	argv := []string{bin, "serve", "--listen", listen, "--token-file", tokenFile, "--db", database}
	return hostService(home, os.Getenv("XDG_CONFIG_HOME"), argv), nil
}

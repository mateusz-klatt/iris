package main

import (
	"context"
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
	mgr, err := userServiceDB(*listen, *token, *db)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx := context.Background()
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
	case "install":
		if err := mgr.Install(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("installed")
	case "uninstall":
		if err := mgr.Uninstall(ctx); err != nil {
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
	}
	return 0
}

// userService builds the per-user manager. listen and tokenFile override the
// defaults from config when setup points the hub at a specific loopback URL.
func userService(listen, tokenFile string) (service.Manager, error) {
	return userServiceDB(listen, tokenFile, "")
}

func userServiceDB(listen, tokenFile, database string) (service.Manager, error) {
	bin, err := exec.LookPath("iris")
	if err != nil {
		return nil, fmt.Errorf("iris is not on PATH: %w", err)
	}
	bin, err = absPath(bin)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Default()
	if err != nil {
		return nil, err
	}
	if listen == "" {
		listen = cfg.Listen
	}
	if tokenFile == "" {
		tokenFile = cfg.TokenFile
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	if database == "" {
		database = cfg.Database
	}
	argv := []string{bin, "serve", "--listen", listen, "--token-file", tokenFile, "--db", database}
	return hostService(home, os.Getenv("XDG_CONFIG_HOME"), argv), nil
}

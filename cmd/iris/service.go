package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/service"
)

func runService(args []string) int {
	if len(args) != 1 || (args[0] != "install" && args[0] != "uninstall" && args[0] != "status") {
		fmt.Fprintln(os.Stderr, "usage: iris service install|uninstall|status")
		return 2
	}
	mgr, err := userService("", "")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx := context.Background()
	switch args[0] {
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
	bin, err := exec.LookPath("iris")
	if err != nil {
		return nil, fmt.Errorf("iris is not on PATH: %w", err)
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
	argv := []string{bin, "serve", "--listen", listen, "--token-file", tokenFile}
	return service.Host(home, os.Getenv("XDG_CONFIG_HOME"), argv), nil
}

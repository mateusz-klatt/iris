package main

import (
	"errors"
	"fmt"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"net/url"
	"os/exec"
	"strings"
)

// openBrowser is best effort. The link is already printed for a machine without one.
func openBrowser(argv []string) bool {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return false
	}
	command := exec.Command(path, argv[1:]...)
	if err = command.Start(); err != nil {
		return false
	}
	go func() { _ = command.Wait() }()
	return true
}

func runUI(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: iris ui")
	}
	console, err := consoleURL(hubclient.NewConfig())
	if err != nil {
		return err
	}
	fmt.Println(console)
	openBrowser(browserCommand(console))
	return nil
}

// consoleURL resolves only the configured origin. Opening the ordinary sign-in
// page needs neither a running hub nor any machine credential.
func consoleURL(cfg hubclient.Resolved) (string, error) {
	if err := cfg.ConfigErr(); err != nil {
		return "", err
	}
	raw := cfg.HubURL()
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return "", errors.New("console URL must be an HTTP loopback or HTTPS origin without credentials, path, query or fragment")
	}
	if err = hubclient.ValidateURL(raw); err != nil {
		return "", errors.New("console URL must be an HTTP loopback or HTTPS origin")
	}
	u.Path = "/"
	return u.String(), nil
}

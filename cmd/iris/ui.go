package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"os/exec"
	"time"
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := &hubclient.Client{Config: hubclient.NewConfig()}
	var link struct {
		URL string `json:"url"`
	}
	if err := h.Do(ctx, "POST", "/v1/ui/links", map[string]any{}, &link); err != nil {
		return err
	}
	fmt.Println(link.URL)
	openBrowser(browserCommand(link.URL))
	return nil
}

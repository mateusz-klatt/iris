package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"os/exec"
	"runtime"
	"time"
)

func runUI(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: iris ui")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := &hubclient.Client{Config: hubclient.EnvConfig{}}
	var link struct {
		URL string `json:"url"`
	}
	if err := h.Do(ctx, "POST", "/v1/ui/links", map[string]any{}, &link); err != nil {
		return err
	}
	fmt.Println(link.URL)
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", link.URL)
	case "darwin":
		command = exec.Command("open", link.URL)
	default:
		command = exec.Command("xdg-open", link.URL)
	}
	if command.Start() == nil {
		go command.Wait()
	}
	return nil
}

package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"time"
)

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
	command := browserCommand(link.URL)
	if command.Start() == nil {
		go command.Wait()
	}
	return nil
}

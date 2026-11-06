package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/mateusz-klatt/iris/internal/hubclient"
)

func runToken(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: iris token add <host> | list | revoke <host>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := &hubclient.Client{Config: hubclient.NewConfig()}
	switch {
	case args[0] == "add" && len(args) == 2:
		var out struct {
			Token string `json:"token"`
		}
		if err := client.Do(ctx, "POST", "/v1/tokens", map[string]string{"host": args[1]}, &out); err != nil {
			return err
		}
		fmt.Println(out.Token)
	case args[0] == "list" && len(args) == 1:
		var out struct {
			Tokens []struct {
				Name    string `json:"name"`
				Revoked *int64 `json:"revoked_ms"`
			} `json:"tokens"`
		}
		if err := client.Do(ctx, "GET", "/v1/tokens", nil, &out); err != nil {
			return err
		}
		for _, token := range out.Tokens {
			state := "active"
			if token.Revoked != nil {
				state = "revoked"
			}
			fmt.Printf("%s\t%s\n", token.Name, state)
		}
	case args[0] == "revoke" && len(args) == 2:
		if err := client.Do(ctx, "DELETE", "/v1/tokens/"+url.PathEscape(args[1]), nil, nil); err != nil {
			return err
		}
		fmt.Println("revoked", args[1])
	default:
		return errors.New("usage: iris token add <host> | list | revoke <host>")
	}
	return nil
}

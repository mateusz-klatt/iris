package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"time"

	"github.com/mateusz-klatt/iris/internal/hubclient"
)

const tokenUsage = "usage: iris token add <host> | list | revoke <host>"

func runToken(args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return tokenCommand(ctx, &hubclient.Client{Config: hubclient.NewConfig()}, args, os.Stdout)
}

func tokenCommand(ctx context.Context, client *hubclient.Client, args []string, stdout io.Writer) error {
	switch {
	case len(args) == 2 && args[0] == "add":
		var out struct {
			Token string `json:"token"`
		}
		if err := client.Do(ctx, "POST", "/v1/tokens", map[string]string{"host": args[1]}, &out); err != nil {
			return err
		}
		fmt.Fprintln(stdout, out.Token)
	case len(args) == 1 && args[0] == "list":
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
			fmt.Fprintf(stdout, "%s\t%s\n", token.Name, state)
		}
	case len(args) == 2 && args[0] == "revoke":
		if err := client.Do(ctx, "DELETE", "/v1/tokens/"+url.PathEscape(args[1]), nil, nil); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "revoked", args[1])
	default:
		return errors.New(tokenUsage)
	}
	return nil
}

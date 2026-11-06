package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/doctor"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/integrate"
)

func runSetup(args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	yes := fs.Bool("yes", false, "do not ask")
	hubFlag := fs.String("hub-url", "", "hub base URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: iris setup [--yes] [--hub-url url]")
		return 2
	}
	hubURL := strings.TrimSpace(*hubFlag)
	if hubURL == "" {
		hubURL = strings.TrimSpace(os.Getenv("IRIS_HUB_URL"))
	}
	if hubURL == "" {
		hubURL = hubclient.DefaultHubURL
	}
	if err := hubclient.ValidateURL(hubURL); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	layout := integrate.Layout{}
	found := integrate.Detect(nil)
	foundSet := map[string]bool{}
	var files []string
	for _, name := range found {
		foundSet[name] = true
		c, ok := integrate.ByName(name)
		if !ok {
			continue
		}
		list, err := c.Files(layout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
			return 1
		}
		files = append(files, list...)
	}
	cfg, err := config.Default()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	listen, local := loopbackListen(hubURL)
	fmt.Printf("Hub: %s\n", hubURL)
	if local {
		fmt.Printf("Service: iris serve --listen %s --token-file %s\n", listen, cfg.TokenFile)
	} else {
		fmt.Println("Service: remote hub, local service skipped")
	}
	if len(files) == 0 {
		fmt.Println("Files: none (no client binary found)")
	} else {
		fmt.Println("Files:")
		for _, f := range files {
			fmt.Printf("  %s\n", f)
		}
	}
	fmt.Printf("Hooks need IRIS_HUB_URL=%s and IRIS_TOKEN_FILE=%s\n", hubURL, cfg.TokenFile)
	if !*yes {
		fmt.Fprint(os.Stdout, "Proceed? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		line = strings.TrimSpace(strings.ToLower(line))
		if line != "y" && line != "yes" {
			for _, name := range integrate.Names {
				fmt.Printf("Iris inactive on %s\n", name)
			}
			return 0
		}
	}
	code := 0
	if local {
		mgr, err := userService(listen, cfg.TokenFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		} else if err := mgr.Install(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "service: %v\n", err)
			code = 1
		}
	}
	done := map[string]bool{}
	plugins := integrate.ExecPlugins{}
	for _, name := range integrate.Names {
		if !foundSet[name] {
			continue
		}
		c, ok := integrate.ByName(name)
		if !ok {
			continue
		}
		if err := c.Install(layout, plugins); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
			code = 1
			continue
		}
		done[name] = true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	res, err := doctor.Run(ctx, doctor.Options{Layout: layout, HubURL: hubURL})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	} else {
		fmt.Print(res.Format())
		if res.ExitCode() != 0 {
			code = 1
		}
	}
	for _, name := range integrate.Names {
		if !done[name] {
			fmt.Printf("Iris inactive on %s\n", name)
			continue
		}
		c, _ := integrate.ByName(name)
		if step := c.Human(); step != "" {
			fmt.Printf("%s: %s\n", name, step)
		}
	}
	return code
}

func loopbackListen(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return "", false
	}
	host := u.Hostname()
	if host != "127.0.0.1" && host != "::1" {
		return "", false
	}
	port := u.Port()
	if port == "" {
		port = "80"
	}
	if host == "::1" {
		return "[::1]:" + port, true
	}
	return host + ":" + port, true
}

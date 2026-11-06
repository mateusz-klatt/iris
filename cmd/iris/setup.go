package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mateusz-klatt/iris/internal/doctor"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/integrate"
)

// setupEnv is the process boundary. Tests fill it so setup never touches the
// real home, the real Claude installer, or the host service manager.
type setupEnv struct {
	in             io.Reader
	out            io.Writer
	errw           io.Writer
	getenv         func(string) string
	layout         integrate.Layout
	detect         func(func(string) (string, error)) []string
	plugins        integrate.Plugins
	installService func(listen, tokenFile string) error
	runDoctor      func(context.Context, doctor.Options) (doctor.Result, error)
	tokenFile      string
}

func runSetup(args []string) int {
	return setup(args, setupEnv{in: os.Stdin, out: os.Stdout, errw: os.Stderr})
}

func setup(args []string, env setupEnv) int {
	if env.in == nil {
		env.in = os.Stdin
	}
	if env.out == nil {
		env.out = os.Stdout
	}
	if env.errw == nil {
		env.errw = os.Stderr
	}
	if env.getenv == nil {
		env.getenv = os.Getenv
	}
	if env.detect == nil {
		env.detect = integrate.Detect
	}
	if env.plugins == nil {
		env.plugins = integrate.ExecPlugins{}
	}
	if env.runDoctor == nil {
		env.runDoctor = doctor.Run
	}
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(env.errw)
	yes := fs.Bool("yes", false, "do not ask")
	var hubFlag string
	fs.StringVar(&hubFlag, "hub-url", "", "hub base URL (alias for --hub)")
	fs.StringVar(&hubFlag, "hub", "", "hub base URL")
	clientsFlag := fs.String("clients", "", "comma-separated clients to integrate (default: all detected)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(env.errw, "usage: iris setup [--yes] [--hub-url url]")
		return 2
	}
	resolved := (hubclient.EnvConfig{Getenv: env.getenv, DefaultTokenFile: env.tokenFile}).Load()
	tokenFile, err := resolved.TokenPath()
	if err != nil {
		fmt.Fprintln(env.errw, err)
		return 1
	}
	hubURL := strings.TrimSpace(hubFlag)
	if hubURL == "" {
		hubURL = resolved.HubURL()
	}
	if err := hubclient.ValidateURL(hubURL); err != nil {
		fmt.Fprintln(env.errw, err)
		return 2
	}
	layout := env.layout
	found := env.detect(layout.LookPath)
	if strings.TrimSpace(*clientsFlag) != "" {
		selected := map[string]bool{}
		for _, name := range strings.Split(*clientsFlag, ",") {
			name = strings.TrimSpace(name)
			c, ok := integrate.ByName(name)
			if !ok {
				fmt.Fprintf(env.errw, "unknown client %q\n", name)
				return 2
			}
			selected[c.Name()] = true
		}
		filtered := []string{}
		for _, name := range found {
			if selected[name] {
				filtered = append(filtered, name)
			}
		}
		found = filtered
	}
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
			fmt.Fprintf(env.errw, "%s: %v\n", name, err)
			return 1
		}
		files = append(files, list...)
	}
	listen, local := loopbackListen(hubURL)
	fmt.Fprintf(env.out, "Hub: %s\n", hubURL)
	if local {
		fmt.Fprintf(env.out, "Service: %s\n", serveCommand(listen, tokenFile))
	} else {
		fmt.Fprintln(env.out, "Service: remote hub, local service skipped")
	}
	if len(files) == 0 {
		fmt.Fprintln(env.out, "Files: none (no client binary found)")
	} else {
		fmt.Fprintln(env.out, "Files:")
		for _, f := range files {
			fmt.Fprintf(env.out, "  %s\n", f)
		}
	}
	configFile := resolved.ConfigFile
	fmt.Fprintf(env.out, "Client configuration: %s\n", configFile)
	if override := strings.TrimSpace(env.getenv("IRIS_HUB_URL")); override != "" && !sameHub(override, hubURL) {
		fmt.Fprintf(env.out, "IRIS_HUB_URL is set to %s and overrides this hub for every command that sees it. Unset it to use %s.\n", override, hubURL)
	}
	if !local {
		fmt.Fprintf(env.out, "Place the hub bearer token in %s (setup cannot fetch it).\n", tokenFile)
	}
	if !*yes {
		fmt.Fprint(env.out, "Proceed? [y/N] ")
		line, _ := bufio.NewReader(env.in).ReadString('\n')
		line = strings.TrimSpace(strings.ToLower(line))
		if line != "y" && line != "yes" {
			for _, name := range integrate.Names {
				fmt.Fprintf(env.out, "Iris inactive on %s\n", name)
			}
			return 0
		}
	}
	if err := hubclient.WriteConfig(configFile, hubclient.FileConfig{Hub: hubURL, TokenFile: tokenFile}); err != nil {
		fmt.Fprintln(env.errw, err)
		return 1
	}
	code := 0
	var serviceErr error
	if local {
		var err error
		if env.installService != nil {
			err = env.installService(listen, tokenFile)
		} else {
			var mgr interface{ Install(context.Context) error }
			mgr, err = userService(listen, tokenFile)
			if err == nil {
				err = mgr.Install(context.Background())
			}
		}
		if err != nil {
			serviceErr = err
			fmt.Fprintf(env.errw, "service: %v\n", err)
			code = 1
		}
	}
	done := map[string]bool{}
	for _, name := range integrate.Names {
		if !foundSet[name] {
			continue
		}
		// integrate.Names is the only set that reaches this loop, and ByName resolves every one of them.
		c, _ := integrate.ByName(name)
		if err := c.Install(layout, env.plugins); err != nil {
			fmt.Fprintf(env.errw, "%s: %v\n", name, err)
			code = 1
			continue
		}
		done[name] = true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	res, err := env.runDoctor(ctx, doctor.Options{Layout: layout, HubURL: hubURL, TokenFile: tokenFile})
	if err != nil {
		fmt.Fprintln(env.errw, err)
		code = 1
	} else {
		if local && !res.HubUp && serviceErr == nil {
			serviceErr = fmt.Errorf("hub is unreachable after service installation")
		}
		fmt.Fprint(env.out, res.Format())
		if res.ExitCode() != 0 {
			code = 1
		}
	}
	for _, name := range integrate.Names {
		if !done[name] {
			fmt.Fprintf(env.out, "Iris inactive on %s\n", name)
			continue
		}
		c, _ := integrate.ByName(name)
		if step := c.Human(); step != "" {
			fmt.Fprintf(env.out, "%s: %s\n", name, step)
		}
	}
	if serviceErr != nil {
		fmt.Fprintf(env.out, "Service could not start: %v\nNext step: %s\n", serviceErr, serveCommand(listen, tokenFile))
	}
	return code
}

func sameHub(a, b string) bool {
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}

// serveCommand is the command text shown to a person. It is never executed.
func serveCommand(listen, tokenFile string) string {
	if strings.ContainsAny(tokenFile, " \t") {
		tokenFile = `"` + tokenFile + `"`
	}
	return "iris serve --listen " + listen + " --token-file " + tokenFile
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

// Command iris coordinates coding agents. See docs/architecture.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/store"
	"github.com/mateusz-klatt/iris/internal/webui"
)

// version is set at release time with -ldflags.
var version = "dev"

// exitProcess is the process boundary. Tests replace it so main can be called.
var exitProcess = os.Exit

func main() {
	exitProcess(dispatch(os.Args[1:]))
}

func dispatch(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "hook":
			return runHook(args[1:])
		case "integrate":
			return runIntegrate(args[1:])
		case "setup":
			return runSetup(args[1:])
		case "doctor":
			return runDoctor(args[1:])
		case "service":
			return runService(args[1:])
		}
	}
	if err := run(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
func run(args []string) error {
	if len(args) > 0 && args[0] == "admin" {
		return runAdmin(args[1:])
	}
	if len(args) > 0 && args[0] == "token" {
		return runToken(args[1:])
	}
	if len(args) > 0 && args[0] == "guard" {
		return runGuard(args[1:])
	}
	if len(args) > 0 && args[0] == "watch" {
		return runWatch(args[1:])
	}
	if len(args) > 0 && args[0] == "ui" {
		return runUI(args[1:])
	}
	if len(args) > 0 && args[0] == "mcp" {
		return runMCP(args[1:])
	}
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Println(version)
		return nil
	}
	if len(args) == 0 || args[0] != "serve" {
		return errors.New("usage: iris serve [--listen address] [--db path] [--token-file path] [--tls-cert path --tls-key path] [--allowed-hosts host:port,...] | setup | doctor | service | ui | token | admin | guard | hook | mcp | watch | integrate | version")
	}
	cfg, err := config.Default()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.StringVar(&cfg.Listen, "listen", cfg.Listen, "HTTP listen address")
	flags.StringVar(&cfg.Database, "db", cfg.Database, "hub-local SQLite file (not a network or drvfs mount)")
	flags.StringVar(&cfg.TokenFile, "token-file", cfg.TokenFile, "owner-only bearer token file")
	flags.StringVar(&cfg.TLSCert, "tls-cert", "", "TLS certificate file")
	flags.StringVar(&cfg.TLSKey, "tls-key", "", "TLS private key file")
	publicURL := flags.String("public-url", "", "public https origin behind a trusted loopback TLS proxy")
	accessLog := flags.Bool("access-log", false, "log every authenticated request, not only each token's first use")
	limits := hub.RateLimitFlags(flags)
	var hosts string
	flags.StringVar(&hosts, "allowed-hosts", "", "additional allowed Host authorities, including ports")
	if err = flags.Parse(args[1:]); err != nil {
		return err
	}
	if err = limits.Validate(); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected serve argument")
	}
	if err = cfg.Validate(); err != nil {
		return err
	}
	// Validate the public origin before creating any state.
	if *publicURL != "" {
		if err = hub.New(nil, "", nil).SetPublicURL(*publicURL); err != nil {
			return err
		}
	}
	s, err := store.Open(cfg.Database, nil)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	token, err := config.Token(cfg.TokenFile)
	if err != nil {
		return err
	}
	allowed := []string{cfg.Listen}
	for _, host := range strings.Split(hosts, ",") {
		if host = strings.TrimSpace(host); host != "" {
			allowed = append(allowed, host)
		}
	}
	handler := hub.New(s, token, allowed)
	handler.Limits = *limits
	handler.AccessLog = *accessLog
	if *publicURL != "" {
		// The same origin was accepted before the store was opened.
		_ = handler.SetPublicURL(*publicURL)
	}
	handler.SetUI(webui.Handler())
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	// Bind before announcing: a port already in use must not read as a hub that started.
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdown)
		case <-done:
		}
	}()
	// Retention (ADR 006): sweep at start, then hourly, until the hub stops.
	go s.RunSweeps(ctx, time.Hour, os.Stderr)
	fmt.Fprintf(os.Stderr, "Iris listening on %s\n", listener.Addr())
	if cfg.TLSCert != "" {
		err = server.ServeTLS(listener, cfg.TLSCert, cfg.TLSKey)
	} else {
		err = server.Serve(listener)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

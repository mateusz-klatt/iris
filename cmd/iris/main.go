// Command iris coordinates coding agents. See docs/architecture.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hook" {
		os.Exit(runHook(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "integrate" {
		os.Exit(runIntegrate(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "setup" {
		os.Exit(runSetup(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		os.Exit(runDoctor(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "service" {
		os.Exit(runService(os.Args[2:]))
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string) error {
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
	if len(args) == 1 && args[0] == "version" {
		fmt.Println(version)
		return nil
	}
	if len(args) == 0 || args[0] != "serve" {
		return errors.New("usage: iris serve [--listen address] [--db path] [--token-file path] [--tls-cert path --tls-key path] [--allowed-hosts host:port,...] | hook | integrate | setup | doctor | service | version")
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
	var hosts string
	flags.StringVar(&hosts, "allowed-hosts", "", "additional allowed Host authorities, including ports")
	if err = flags.Parse(args[1:]); err != nil {
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
	defer s.Close()
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
	if *publicURL != "" {
		if err = handler.SetPublicURL(*publicURL); err != nil {
			return err
		}
	}
	handler.SetUI(webui.Handler())
	server := &http.Server{Addr: cfg.Listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
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
	fmt.Fprintf(os.Stderr, "Iris listening on %s\n", cfg.Listen)
	if cfg.TLSCert != "" {
		err = server.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
	} else {
		err = server.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

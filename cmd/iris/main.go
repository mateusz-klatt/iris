// Command iris coordinates coding agents. See docs/architecture.md.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mateusz-klatt/iris/internal/buildinfo"
	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/store"
	"github.com/mateusz-klatt/iris/internal/webui"
)

// exitProcess is the process boundary. Tests replace it so main can be called.
var exitProcess = os.Exit

// serveContext ends when the hub should stop. It is the process boundary for a signal:
// Windows has no way for a process to send itself one, so a test there ends the hub here.
var serveContext = func() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

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
		case "status":
			return runStatus(args[1:])
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
		fmt.Println(buildinfo.String())
		return nil
	}
	if len(args) == 0 || args[0] != "serve" {
		return errors.New("usage: iris serve [--listen address] [--db path] [--token-file path] [--console-key-file path] [--tls-cert path --tls-key path] [--allowed-hosts host:port,...] | setup | doctor | status | service | ui | token | admin | guard | hook | mcp | watch | integrate | version")
	}
	cfg, err := config.Default()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.StringVar(&cfg.Listen, "listen", cfg.Listen, "HTTP listen address")
	flags.StringVar(&cfg.Database, "db", cfg.Database, "hub-local SQLite file (not a network or drvfs mount)")
	flags.StringVar(&cfg.TokenFile, "token-file", cfg.TokenFile, "owner-only bearer token file")
	consoleKeyFile := flags.String("console-key-file", "", "existing independent console key file (default: database path plus .console.key)")
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
	if *consoleKeyFile == "" {
		*consoleKeyFile = cfg.Database + ".console.key"
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
	configureConsole(handler, *consoleKeyFile, cfg.TokenFile, token, os.Stderr)
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
	ctx, stop := serveContext()
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

// consoleConfiguration is the immutable hub setup boundary, before serving any
// request. Explicit owner activation prepares the key for the next process; its
// callback never changes the key or opens the running console.
type consoleConfiguration interface {
	ConfigureConsole([]byte, func() error) error
}

func configureConsole(handler consoleConfiguration, keyFile, ownerFile, ownerToken string, logs io.Writer) {
	key, err := consoleKeyForHub(keyFile, ownerFile, ownerToken, false)
	if err != nil {
		fmt.Fprintln(logs, "Iris console closed: console key unavailable or not independent")
	}
	if err = handler.ConfigureConsole(key, func() error {
		_, err := consoleKeyForHub(keyFile, ownerFile, ownerToken, true)
		return err
	}); err != nil {
		// A database/setup error cannot stop coordination or disclose its details.
		fmt.Fprintln(logs, "Iris console closed: account authentication unavailable")
	}
}

// consoleKeyForHub refuses file aliases before protecting or creating a key, and
// copied owner credentials after decoding. The owner token has already passed
// config.Token validation. No file path, key or filesystem diagnostic is returned
// in a failure message. Only the explicit activation callback requests creation.
func consoleKeyForHub(keyFile, ownerFile, ownerToken string, create bool) ([]byte, error) {
	ownerInfo, err := os.Stat(ownerFile)
	if err != nil {
		return nil, errors.New("owner credential file unavailable")
	}
	keyInfo, err := os.Stat(keyFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("console key file unavailable")
	}
	if err == nil && os.SameFile(ownerInfo, keyInfo) {
		return nil, errors.New("console key must be independent of owner credential")
	}
	var key []byte
	if create {
		key, err = config.ConsoleKey(keyFile)
	} else {
		key, err = config.LoadConsoleKey(keyFile)
	}
	if err != nil {
		return nil, errors.New("console key file unavailable")
	}
	ownerKey, _ := base64.RawURLEncoding.DecodeString(ownerToken) // config.Token already validated this credential.
	if bytes.Equal(key, ownerKey) {
		return nil, errors.New("console key must be independent of owner credential")
	}
	return key, nil
}

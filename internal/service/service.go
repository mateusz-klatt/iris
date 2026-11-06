// Package service installs the per-user Iris hub process.
// Tests pass a Runner so nothing here talks to the real service manager
// unless Host is used.
package service

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

const Marker = "iris-managed"

// Status is the installed hub service.
type Status struct {
	Installed bool
	Active    bool
	Detail    string
}

// Manager installs, removes and reports the user service.
type Manager interface {
	Install(ctx context.Context) error
	Uninstall(ctx context.Context) error
	Status(ctx context.Context) (Status, error)
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// Runner executes a host command and returns its combined output.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExecRunner is the real process runner.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			return "", err
		}
		return text, fmt.Errorf("%w: %s", err, text)
	}
	return text, nil
}

// SystemdUnit is the user unit. argv is the iris serve command.
func SystemdUnit(argv []string) []byte {
	var execStart strings.Builder
	for i, arg := range argv {
		if i > 0 {
			execStart.WriteByte(' ')
		}
		execStart.WriteString(systemdQuote(arg))
	}
	return []byte(fmt.Sprintf(`# %s
[Unit]
Description=Iris hub
After=default.target

[Service]
Type=simple
ExecStart=%s
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
`, Marker, execStart.String()))
}

func systemdQuote(s string) string {
	if s == "" || strings.ContainsAny(s, " \t\"'\\") {
		return strconv.Quote(s)
	}
	return s
}

// LaunchdPlist is the user LaunchAgent.
func LaunchdPlist(argv []string) []byte {
	var args strings.Builder
	for _, arg := range argv {
		fmt.Fprintf(&args, "    <string>%s</string>\n", xmlEscape(arg))
	}
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- %s -->
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>iris</string>
  <key>ProgramArguments</key>
  <array>
%s  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
</dict>
</plist>
`, Marker, args.String()))
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

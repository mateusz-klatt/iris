package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"

	"github.com/mateusz-klatt/iris/internal/consoleauth"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

const adminConsoleUsage = "usage: iris admin console (create | reset | activate) <username> < password-file\nOne final LF or CRLF is removed as input line framing; all other password bytes are preserved"

// adminConsoleCommand sends setup or activation material to owner-only routes.
// Passwords are never command arguments, printed output or URL components. The
// hub performs the KDF and is the only process writing account records.
func adminConsoleCommand(ctx context.Context, client *hubclient.Client, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) != 2 || (args[0] != "create" && args[0] != "reset" && args[0] != "activate") {
		return errors.New(adminConsoleUsage)
	}
	if _, err := consoleauth.LoginKey(args[1]); err != nil {
		return errors.New("invalid console username")
	}
	// The extra three bytes admit a maximum-size password plus CRLF while still
	// detecting oversized input after removing at most one framing sequence.
	raw, err := io.ReadAll(io.LimitReader(stdin, consoleauth.MaxPasswordBytes+3))
	if err != nil {
		return errors.New("could not read password from standard input")
	}
	if bytes.HasSuffix(raw, []byte("\r\n")) {
		raw = raw[:len(raw)-2]
	} else if bytes.HasSuffix(raw, []byte("\n")) {
		raw = raw[:len(raw)-1]
	}
	password := string(raw)
	if args[0] == "activate" {
		if len(raw) > consoleauth.MaxPasswordBytes || !utf8.Valid(raw) {
			return errors.New("password must be valid UTF-8 and at most 1024 bytes")
		}
	} else if err = consoleauth.ValidateNewPassword(password); err != nil {
		return errors.New("new password must have at least 15 Unicode code points and at most 1024 UTF-8 bytes")
	}
	path := "/v1/admin/console/accounts"
	switch args[0] {
	case "reset":
		path += "/reset"
	case "activate":
		path = "/v1/admin/console/activate"
	}
	var out map[string]any
	if err = client.Do(ctx, "POST", path, map[string]string{"username": args[1], "password": password}, &out); err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

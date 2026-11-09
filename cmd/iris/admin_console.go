package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/mateusz-klatt/iris/internal/consoleauth"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

const adminConsoleUsage = "usage: iris admin console (create | reset) <username> < password-file\n       iris admin console import --source <SQLite file> --manifest <JSON file> [--apply]\n       iris admin console import-grants --manifest <JSON file> [--apply]\nThe password is read exactly from standard input, without trimming a final newline"

// adminConsoleCommand sends a new password to the owner-only staged setup path.
// Passwords are never command arguments, printed output or URL components. The
// hub performs the KDF and is the only process writing account records.
func adminConsoleCommand(ctx context.Context, client *hubclient.Client, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) > 0 && (args[0] == "import" || args[0] == "import-grants") {
		return adminConsoleImport(ctx, client, args, stdout)
	}
	if len(args) != 2 || (args[0] != "create" && args[0] != "reset") {
		return errors.New(adminConsoleUsage)
	}
	if _, err := consoleauth.LoginKey(args[1]); err != nil {
		return errors.New("invalid console username")
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, consoleauth.MaxPasswordBytes+1))
	if err != nil {
		return errors.New("could not read password from standard input")
	}
	password := string(raw)
	if err = consoleauth.ValidateNewPassword(password); err != nil {
		return errors.New("new password must have at least 15 Unicode code points and at most 1024 UTF-8 bytes")
	}
	path := "/v1/admin/console/accounts"
	if args[0] == "reset" {
		path += "/reset"
	}
	var out map[string]any
	if err = client.Do(ctx, "POST", path, map[string]string{"username": args[1], "password": password}, &out); err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

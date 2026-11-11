// Command iris-release-tools signs Windows builds and validates their release handoff.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

var exitProcess = os.Exit

func main() {
	exitProcess(dispatch(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func dispatch(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if err := run(args, getenv, stdout); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func run(args []string, getenv func(string) string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("release tools: expected sign-windows or verify-windows-archives")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	switch args[0] {
	case "sign-windows":
		arch := flags.String("arch", "", "Windows architecture")
		path := flags.String("path", "", "Windows executable")
		if flags.Parse(args[1:]) != nil || flags.NArg() != 0 {
			return fmt.Errorf("release tools: invalid signing arguments")
		}
		if err := signWindows(*arch, *path, getenv); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(out, "Windows signing completed.")
		return nil
	case "verify-windows-archives":
		version := flags.String("version", "", "GoReleaser version without the tag prefix")
		dir := flags.String("dir", "", "Downloaded handoff directory")
		dest := flags.String("out", "", "Extraction directory")
		if flags.Parse(args[1:]) != nil || flags.NArg() != 0 {
			return fmt.Errorf("release tools: invalid archive arguments")
		}
		return verifyWindowsArchives(*version, *dir, *dest, out)
	default:
		return fmt.Errorf("release tools: unknown command")
	}
}

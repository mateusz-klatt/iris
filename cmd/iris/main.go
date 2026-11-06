// Command iris coordinates coding agents. See docs/architecture.md.
package main

import (
	"fmt"
	"os"
)

// version is set at release time with -ldflags.
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "hook" {
		os.Exit(runHook(os.Args[2:]))
	}
	fmt.Fprintln(os.Stderr, "usage: iris <command>")
	os.Exit(2)
}

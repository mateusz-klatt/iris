package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/mateusz-klatt/iris/internal/integrate"
)

func runIntegrate(args []string) int {
	fs := flag.NewFlagSet("integrate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	remove := fs.Bool("remove", false, "restore the files Iris wrote")
	all := fs.Bool("all", false, "every supported client")
	wake := fs.Bool("wake", false, "enable Codex's Iris wake runner")
	noWake := fs.Bool("no-wake", false, "disable Codex's Iris wake runner")
	if err := fs.Parse(integrateFlagArgs(args)); err != nil {
		return 2
	}
	var wakeSet, noWakeSet bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "wake" {
			wakeSet = true
		}
		if f.Name == "no-wake" {
			noWakeSet = true
		}
	})
	if wakeSet && noWakeSet {
		fmt.Fprintln(os.Stderr, "--wake and --no-wake are mutually exclusive")
		return 2
	}
	if (wakeSet || noWakeSet) && (*all || *remove) {
		fmt.Fprintln(os.Stderr, "--wake and --no-wake require a Codex install without --all or --remove")
		return 2
	}
	var names []string
	if *all {
		if fs.NArg() != 0 {
			fmt.Fprintln(os.Stderr, "usage: iris integrate [--remove] [--all] <client>")
			return 2
		}
		names = append(names, integrate.Names...)
	} else if fs.NArg() == 1 {
		name := integrate.Canonical(fs.Arg(0))
		if name == "" {
			fmt.Fprintf(os.Stderr, "unknown client %q\n", fs.Arg(0))
			return 2
		}
		names = []string{name}
	} else {
		fmt.Fprintln(os.Stderr, "usage: iris integrate [--remove] [--all] <client>")
		return 2
	}
	if (wakeSet || noWakeSet) && names[0] != integrate.Codex {
		fmt.Fprintln(os.Stderr, "--wake and --no-wake require Codex")
		return 2
	}
	layout := integrate.Layout{}
	plugins := integrate.ExecPlugins{}
	code := 0
	for _, name := range names {
		// names are Canonical or integrate.Names, both of which ByName resolves.
		c, _ := integrate.ByName(name)
		var err error
		if *remove {
			err = c.Remove(layout, plugins)
		} else if wakeSet || noWakeSet {
			enabled := *wake
			if noWakeSet {
				enabled = !*noWake
			}
			err = integrate.InstallCodexWake(layout, enabled)
		} else {
			err = c.Install(layout, plugins)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
			code = 1
			continue
		}
		if *remove {
			fmt.Printf("removed %s\n", name)
		} else {
			fmt.Printf("integrated %s\n", name)
		}
	}
	return code
}

// Integrate's options are booleans, so they can be collected before the positional client.
// Respect --: anything after it remains positional, as the standard flag parser promises.
func integrateFlagArgs(args []string) []string {
	var flags, positional []string
	for i, arg := range args {
		if arg == "--" {
			positional = append(positional, args[i:]...)
			break
		}
		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
		} else {
			positional = append(positional, arg)
		}
	}
	return append(flags, positional...)
}

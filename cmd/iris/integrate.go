package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/integrate"
)

var integrateLayout = func() integrate.Layout { return integrate.Layout{} }

func stopAntigravityHelpers(layout integrate.Layout) error {
	dirs, err := integrate.AntigravityConfigDirs(layout)
	for _, dir := range dirs {
		err = errors.Join(err, hook.StopAntigravityWakeHelpers(dir))
	}
	return err
}

func runIntegrate(args []string) int {
	fs := flag.NewFlagSet("integrate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	remove := fs.Bool("remove", false, "restore the files Iris wrote")
	all := fs.Bool("all", false, "every supported client")
	wake := fs.Bool("wake", false, "enable the native Iris wake runner for Antigravity, Codex, Cursor, Kimi or Grok")
	noWake := fs.Bool("no-wake", false, "disable the native Iris wake runner for Antigravity, Codex, Cursor, Kimi or Grok")
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
		fmt.Fprintln(os.Stderr, "--wake and --no-wake require an Antigravity, Codex, Cursor, Kimi or Grok install without --all or --remove")
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
	if (wakeSet || noWakeSet) && names[0] != integrate.Antigravity && names[0] != integrate.Codex && names[0] != integrate.Cursor && names[0] != integrate.Kimi && names[0] != integrate.Grok {
		fmt.Fprintln(os.Stderr, "--wake and --no-wake require Antigravity, Codex, Cursor, Kimi or Grok")
		return 2
	}
	layout := integrateLayout()
	plugins := integrate.ExecPlugins{}
	code := 0
	for _, name := range names {
		// names are Canonical or integrate.Names, both of which ByName resolves.
		c, _ := integrate.ByName(name)
		var err error
		if *remove {
			err = c.Remove(layout, plugins)
			if name == integrate.Antigravity {
				err = errors.Join(err, stopAntigravityHelpers(layout))
			}
		} else if wakeSet || noWakeSet {
			enabled := *wake
			if noWakeSet {
				enabled = !*noWake
			}
			switch name {
			case integrate.Antigravity:
				err = integrate.InstallAntigravityWake(layout, enabled)
				if !enabled {
					err = errors.Join(err, stopAntigravityHelpers(layout))
				}
			case integrate.Kimi:
				err = integrate.InstallKimiWake(layout, enabled)
			case integrate.Grok:
				err = integrate.InstallGrokWake(layout, enabled)
			case integrate.Cursor:
				err = integrate.InstallCursorWake(layout, enabled)
			default:
				err = integrate.InstallCodexWake(layout, enabled)
			}
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
			continue
		}
		fmt.Printf("integrated %s\n", name)
		if step := humanStep(c, layout); step != "" {
			fmt.Printf("%s: %s\n", name, step)
		}
	}
	return code
}

// humanStep is what a person must still do before the client runs Iris, read as doctor reads it:
// a client that can check whether the step was done says so instead of repeating it.
func humanStep(c integrate.Client, l integrate.Layout) string {
	if r, ok := c.(integrate.Reviewer); ok {
		step, _ := r.Review(l)
		return step
	}
	return c.Human()
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

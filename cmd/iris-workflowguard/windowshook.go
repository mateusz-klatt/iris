package main

import (
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// syntheticSigner stands in for the Certum signer: it signs by creating <path>.signed, and fails
// if that file exists already, so a binary signed twice is caught.
const syntheticSigner = `package main
import "os"
func main() { if len(os.Args) != 2 { os.Exit(2) }; marker, err := os.OpenFile(os.Args[1]+".signed", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600); if err != nil { os.Exit(3) }; if _, err := marker.WriteString("called once"); err != nil { os.Exit(4) }; if marker.Close() != nil { os.Exit(5) } }
`

// A hookCase is one goreleaser build with the real Windows hook: whether it is a snapshot, which
// variable it lacks, whether goreleaser must succeed, how many times the signer must run, and a
// line its output must contain.
type hookCase struct {
	name      string
	snapshot  bool
	missing   string
	succeeds  bool
	signs     int
	mustSay   string
	twiceHook bool
}

var hookCases = []hookCase{
	{name: "snapshot", snapshot: true, succeeds: true},
	{name: "missing-helper", missing: "IRIS_RELEASE_HELPER", mustSay: "IRIS_RELEASE_HELPER"},
	{name: "missing-credentials", missing: "CERTUM_OTP", mustSay: "release tools: signing configuration is incomplete"},
	{name: "signed", succeeds: true, signs: 1},
	{name: "repeated-hook", signs: 1, mustSay: "release tools: Windows signing failed", twiceHook: true},
}

// hookExercise runs the release build's Windows signing hook through goreleaser, with the real
// release helper and a synthetic signer, in a fresh directory under temp.
type hookExercise struct {
	root    string // the repository
	temp    string // where the work directory goes; CI passes RUNNER_TEMP
	exe     string // ".exe" on Windows
	environ []string
	command func(dir string, env []string, name string, args ...string) (string, error)
	write   func(path string, data []byte, perm os.FileMode) error
}

func defaultExercise(out io.Writer) error {
	e := hookExercise{root: ".", temp: os.Getenv("RUNNER_TEMP"), exe: exeSuffix(runtime.GOOS), environ: os.Environ(), command: runCommand, write: os.WriteFile}
	return e.run(out)
}

// exeSuffix is an executable's suffix on goos.
func exeSuffix(goos string) string {
	if goos == "windows" {
		return ".exe"
	}
	return ""
}

// runCommand runs a command and returns what it wrote to stdout and stderr together.
func runCommand(dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e hookExercise) run(out io.Writer) error {
	work, err := os.MkdirTemp(e.temp, "iris-signing-hook-")
	if err != nil {
		return err
	}
	helper := filepath.Join(work, "iris-release-tools"+e.exe)
	signer := filepath.Join(work, "synthetic-signer"+e.exe)
	source := filepath.Join(work, "signer.go")
	if err := e.write(source, []byte(syntheticSigner), 0o600); err != nil {
		return err
	}
	build := environment(e.environ, map[string]string{"CGO_ENABLED": "0"}, "")
	if output, err := e.command(e.root, build, "go", "build", "-trimpath", "-o", helper, "./cmd/iris-release-tools"); err != nil {
		return fmt.Errorf("building the release helper: %v\n%s", err, output)
	}
	if output, err := e.command(e.root, build, "go", "build", "-trimpath", "-o", signer, source); err != nil {
		return fmt.Errorf("building the synthetic signer: %v\n%s", err, output)
	}
	config, err := load(filepath.Join(e.root, ".goreleaser.yaml"))
	if err != nil {
		return err
	}
	hook := at(config, "builds", 0, "hooks")
	for _, c := range hookCases {
		if err := e.check(c, work, helper, signer, hook); err != nil {
			return fmt.Errorf("%s: %w", c.name, err)
		}
		_, _ = fmt.Fprintln(out, c.name+": expected hook result and signer call count")
	}
	return nil
}

// check runs one goreleaser build and compares its result with the case.
func (e hookExercise) check(c hookCase, work, helper, signer string, hook any) error {
	dist := filepath.Join(work, c.name)
	if c.twiceHook {
		post := items(at(hook, "post"))
		hook = map[string]any{"post": append(slices.Clone(post), post...)}
	}
	// Marshal fails only for values YAML cannot hold, such as functions or channels; these are
	// plain maps, lists and strings, and the hook was decoded from YAML.
	raw, _ := yaml.Marshal(map[string]any{
		"version": 2, "project_name": "iris", "dist": dist,
		"builds": []any{map[string]any{
			"id": "iris", "main": "./cmd/iris-release-tools", "binary": "iris",
			"env": []any{"CGO_ENABLED=0"}, "goos": []any{"windows"}, "goarch": []any{"amd64"},
			"hooks": hook,
		}},
	})
	config := filepath.Join(work, c.name+".yaml")
	if err := e.write(config, raw, 0o600); err != nil {
		return err
	}
	env := environment(e.environ, map[string]string{
		"GOOS": "windows", "GOARCH": "amd64", "GORELEASER_CURRENT_TAG": "v0.0.0-signing-test",
		"IRIS_RELEASE_HELPER": helper, "IRIS_WINDOWS_SIGNER": signer, "XDG_RUNTIME_DIR": filepath.Join(work, "runtime"),
		"CERTUM_EMAIL": "synthetic@example.invalid", "CERTUM_OTP": "synthetic",
	}, c.missing)
	args := []string{"build", "--config", config, "--single-target", "--skip=before,validate"}
	if c.snapshot {
		args = append(args, "--snapshot")
	}
	output, err := e.command(e.root, env, "goreleaser", args...)
	if (err == nil) != c.succeeds {
		return fmt.Errorf("goreleaser succeeded %t, want %t:\n%s", err == nil, c.succeeds, output)
	}
	markers := signed(dist)
	if len(markers) != c.signs {
		return fmt.Errorf("the signer ran %d times, want %d: %v", len(markers), c.signs, markers)
	}
	for _, marker := range markers {
		if raw, err := os.ReadFile(marker); err != nil || string(raw) != "called once" {
			return fmt.Errorf("%s does not say called once", marker)
		}
	}
	if !strings.Contains(output, c.mustSay) {
		return fmt.Errorf("the output lacks %q:\n%s", c.mustSay, output)
	}
	return nil
}

// signed is every path under dist named *.signed, as the old guard's rglob listed them: a directory
// with that name counts too, and then fails the "called once" check. A missing dist has none.
func signed(dist string) []string {
	var out []string
	_ = filepath.WalkDir(dist, func(path string, _ fs.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(path, ".signed") {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// environment is base with the variables in set replaced and drop removed.
func environment(base []string, set map[string]string, drop string) []string {
	var out []string
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if _, replaced := set[name]; !replaced && name != drop {
			out = append(out, kv)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(set)) {
		if name != drop {
			out = append(out, name+"="+set[name])
		}
	}
	return out
}

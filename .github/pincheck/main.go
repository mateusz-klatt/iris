// Command pincheck fails when a workflow uses an action by a tag or branch. A tag can
// be moved to new code; a full commit SHA cannot. Local actions and docker images
// are out of its scope.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	uses   = regexp.MustCompile(`^\s*-?\s*uses:\s*['"]?([^'"\s#]+)`)
	pinned = regexp.MustCompile(`@[0-9a-f]{40}$`)
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout))
}

func run(dirs []string, out io.Writer) int {
	if len(dirs) == 0 {
		fmt.Fprintln(out, "usage: pincheck <workflow directory>...")
		return 2
	}
	var problems []string
	for _, dir := range dirs {
		// The pattern is fixed, so Glob cannot report ErrBadPattern.
		files, _ := filepath.Glob(filepath.Join(dir, "*.y*ml"))
		if len(files) == 0 {
			fmt.Fprintf(out, "%s: no workflows found\n", dir)
			return 2
		}
		for _, file := range files {
			raw, err := os.ReadFile(file)
			if err != nil {
				fmt.Fprintln(out, err)
				return 2
			}
			problems = append(problems, check(file, string(raw))...)
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		fmt.Fprintln(out, p)
	}
	if len(problems) > 0 {
		return 1
	}
	return 0
}

// check reports every `uses:` in text that is not pinned to a full commit SHA.
func check(file, text string) []string {
	var problems []string
	for i, line := range strings.Split(text, "\n") {
		m := uses.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ref := m[1]
		if strings.HasPrefix(ref, "./") || strings.HasPrefix(ref, "docker://") || pinned.MatchString(ref) {
			continue
		}
		problems = append(problems, fmt.Sprintf("%s:%d: %s is not pinned to a full commit SHA", file, i+1, ref))
	}
	return problems
}

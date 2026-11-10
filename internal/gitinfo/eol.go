package gitinfo

import (
	"bytes"
	"context"
	"strings"
)

// LineEndings counts the tracked files in a worktree that git stores with LF while the worktree
// holds them with CRLF, and that no attribute or setting tells this git to convert. Each one
// reads as modified to this git, whatever its content: a Windows checkout written with
// core.autocrlf=true and used from WSL is the measured case (474 of 537 files). Text is the
// number of tracked files git stores as LF text.
//
// It reads `git ls-files --eol`, which compares each file's index and worktree content
// directly. A count from git status or git diff would depend on whose stat data the index
// last recorded, and changes when another git refreshes it.
func LineEndings(ctx context.Context, root string) (crlf, text int, err error) {
	if conversion(ctx, root) {
		return 0, 0, nil
	}
	out, err := gitCommand(ctx, root, "--no-optional-locks", "ls-files", "--eol", "-z").Output()
	if err != nil {
		return 0, 0, err
	}
	crlf, text = CountLineEndings(out)
	return crlf, text, nil
}

// conversion reports whether this git turns CRLF into LF when it reads the worktree:
// core.autocrlf set to true or input. Then no file is affected.
func conversion(ctx context.Context, root string) bool {
	out, _ := gitCommand(ctx, root, "config", "--get", "core.autocrlf").Output() // unset exits 1, which means no conversion
	value := strings.ToLower(strings.TrimSpace(string(out)))
	return value == "true" || value == "input"
}

// CountLineEndings reads `git ls-files --eol -z` output. An entry is "i/<index> w/<worktree>
// attr/<attributes>" then a tab and the path. A file with any text or eol attribute is
// converted by git and is not counted.
func CountLineEndings(raw []byte) (crlf, text int) {
	for _, entry := range bytes.Split(raw, []byte{0}) {
		info, _, ok := bytes.Cut(entry, []byte{'\t'})
		if !ok {
			continue
		}
		fields := strings.Fields(string(info))
		if len(fields) < 2 || fields[0] != "i/lf" {
			continue
		}
		text++
		if fields[1] == "w/crlf" && len(fields) == 3 && fields[2] == "attr/" {
			crlf++
		}
	}
	return crlf, text
}

package gitinfo

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
)

// Status reads NUL-delimited porcelain, preserving spaces, newlines and both rename ends.
func Status(ctx context.Context, root string) (map[string]string, error) {
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain", "-z", "--untracked-files=all")
	cmd.Dir = root
	var out limitedStatus
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return ParseStatus(out.Bytes())
}

// Avoid allocating unbounded output in large generated trees.
type limitedStatus struct{ bytes.Buffer }

func (b *limitedStatus) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 48<<10 {
		return 0, errors.New("git status exceeds reconciliation limit")
	}
	return b.Buffer.Write(p)
}

func ParseStatus(raw []byte) (map[string]string, error) {
	entries := bytes.Split(raw, []byte{0})
	out := map[string]string{}
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) == 0 {
			continue
		}
		if len(e) < 4 || e[2] != ' ' {
			return nil, errors.New("invalid git porcelain record")
		}
		status, p := string(e[:2]), string(e[3:])
		out[p] = status
		if bytes.ContainsAny(e[:2], "RC") {
			i++
			if i >= len(entries) || len(entries[i]) == 0 {
				return nil, errors.New("missing rename source")
			}
			out[string(entries[i])] = status
		}
	}
	return out, nil
}

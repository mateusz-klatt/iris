package gitinfo

import (
	"bytes"
	"context"
	"errors"
)

// Status reads NUL-delimited porcelain, preserving spaces, newlines and both rename ends.
//
// --no-optional-locks keeps git from taking index.lock to refresh the index. The hook runs this
// under a 300 ms budget and kills git when it runs out. A git killed while holding the lock left
// a stale .git/index.lock, and the user's next commit then failed with "Another git process seems
// to be running" (measured on macOS: 6 of 40 killed runs left one; 0 of 40 with the flag).
func Status(ctx context.Context, root string) (map[string]string, error) {
	cmd := gitCommand(ctx, root, "--no-optional-locks", "status", "--porcelain", "-z", "--untracked-files=all", "--ignore-submodules=untracked")
	var out limitedStatus
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return ParseStatus(out.kept.Bytes())
}

// statusLimit bounds what a status may hold, so a large generated tree cannot make the hook
// allocate without limit or send the hub a snapshot it refuses.
const statusLimit = 48 << 10

// limitedStatus keeps git's output up to statusLimit and fails the write that would pass it.
// Its buffer is a named field on purpose. os/exec copies with io.Copy, which gives the whole
// copy to a writer that has ReadFrom, and an embedded bytes.Buffer has one that reads to the
// end without calling Write: with it, the limit never applied.
type limitedStatus struct{ kept bytes.Buffer }

func (b *limitedStatus) Write(p []byte) (int, error) {
	if b.kept.Len()+len(p) > statusLimit {
		return 0, errors.New("git status exceeds reconciliation limit")
	}
	return b.kept.Write(p)
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

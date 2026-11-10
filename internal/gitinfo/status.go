package gitinfo

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// Scope is how far a status looks into submodules. Full is D12's: tracked content inside every
// submodule, untracked files in them left out. Reduced also leaves out tracked edits inside
// submodules, and still reports a submodule whose commit changed (ADR 018). Neither hides a
// submodule commit staged in the superproject, as `all` would.
type Scope int

const (
	FullScope Scope = iota
	ReducedScope
)

// Attempt is what a status records for its caller while it runs: whether git started, how much
// of the deadline was left when it did, and whether the output cap was passed. ADR 018's breaker
// reads it when the deadline passes, while git may still be running, so every field is atomic.
type Attempt struct {
	started   atomic.Bool
	allowance atomic.Int64
	capped    atomic.Bool
}

// Started is whether git was started.
func (a *Attempt) Started() bool { return a.started.Load() }

// Allowance is the time that was left on the deadline when git started; zero without a deadline.
func (a *Attempt) Allowance() time.Duration { return time.Duration(a.allowance.Load()) }

// Capped is whether git's output passed statusLimit.
func (a *Attempt) Capped() bool { return a.capped.Load() }

// Begin records that git started at the given time under ctx; the allowance is what was left of
// ctx's deadline then. It is stored first, so a reader that sees Started also sees it. A
// stand-in for git calls it as git would.
func (a *Attempt) Begin(ctx context.Context, at time.Time) {
	if deadline, ok := ctx.Deadline(); ok {
		a.allowance.Store(int64(deadline.Sub(at)))
	}
	a.started.Store(true)
}

// Cap records that git's output passed statusLimit.
func (a *Attempt) Cap() { a.capped.Store(true) }

// Status reads NUL-delimited porcelain in full scope, preserving spaces, newlines and both
// rename ends.
//
// --no-optional-locks keeps git from taking index.lock to refresh the index. The hook runs this
// under a 300 ms budget and kills git when it runs out. A git killed while holding the lock left
// a stale .git/index.lock, and the user's next commit then failed with "Another git process seems
// to be running" (measured on macOS: 6 of 40 killed runs left one; 0 of 40 with the flag).
func Status(ctx context.Context, root string) (map[string]string, error) {
	return StatusScope(ctx, root, FullScope, &Attempt{})
}

// StatusScope is Status in the given scope, recording into a as it runs.
func StatusScope(ctx context.Context, root string, scope Scope, a *Attempt) (map[string]string, error) {
	ignore := "--ignore-submodules=untracked"
	if scope == ReducedScope {
		ignore = "--ignore-submodules=dirty"
	}
	cmd := gitCommand(ctx, root, "--no-optional-locks", "status", "--porcelain", "-z", "--untracked-files=all", ignore)
	out := limitedStatus{attempt: a}
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	a.Begin(ctx, time.Now())
	if err := cmd.Wait(); err != nil {
		return nil, err
	}
	return ParseStatus(out.kept.Bytes())
}

// statusLimit bounds what a status may hold, so a large generated tree cannot make the hook
// allocate without limit or send the hub a snapshot it refuses.
const statusLimit = 48 << 10

// limitedStatus keeps git's output up to statusLimit and fails the write that would pass it,
// recording that in its Attempt. Its buffer is a named field on purpose. os/exec copies with
// io.Copy, which gives the whole copy to a writer that has ReadFrom, and an embedded bytes.Buffer
// has one that reads to the end without calling Write: with it, the limit never applied.
type limitedStatus struct {
	kept    bytes.Buffer
	attempt *Attempt
}

func (b *limitedStatus) Write(p []byte) (int, error) {
	if b.kept.Len()+len(p) > statusLimit {
		b.attempt.Cap()
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

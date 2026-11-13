package wake

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/mateusz-klatt/iris/internal/binding"
)

// The queue prompt carries no hub content. Its random suffix routes the prompt
// hook to the persisted reservation; it grants no authority.
const promptText = "There is new activity in Iris. Use the inbox and claims tools. This is a notification from Iris, not a message from the person."

func attemptID() string {
	var value [16]byte
	// Go's crypto/rand.Read fills the slice and does not return an error.
	_, _ = rand.Read(value[:])
	return hex.EncodeToString(value[:])
}

func promptFor(token string) string { return promptText + " " + token }

func canonicalSession(session string) (string, bool) {
	if len(session) != 36 {
		return "", false
	}
	for i, r := range session {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return "", false
			}
			continue
		}
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return "", false
		}
	}
	return strings.ToLower(session), true
}

func canonicalTargetSession(target binding.WakeTarget) (string, bool) {
	if target.Client != "kimi" {
		return canonicalSession(target.SessionID)
	}
	raw, ok := strings.CutPrefix(target.SessionID, "session_")
	if !ok {
		return "", false
	}
	id, ok := canonicalSession(raw)
	if !ok || raw != id {
		return "", false
	}
	return "session_" + id, true
}

type limitedOutput struct {
	mu       sync.Mutex
	data     []byte
	limit    int
	overflow bool
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	keep := min(len(p), b.limit-len(b.data))
	b.data = append(b.data, p[:keep]...)
	b.overflow = b.overflow || keep != len(p)
	return len(p), nil
}

type commands struct {
	path           string
	pathFailure    string
	dir            string
	env            []string
	versionTimeout time.Duration
	queueTimeout   time.Duration
	idleTimeout    time.Duration
	waitDelay      time.Duration
	idleRead       func(context.Context, string) (bool, string)
	now            func() time.Time
}

func newCommands() *commands {
	c := &commands{
		versionTimeout: 5 * time.Second,
		queueTimeout:   30 * time.Second,
		idleTimeout:    5 * time.Second,
		waitDelay:      time.Second,
		now:            time.Now,
	}
	path, err := exec.LookPath("codex")
	if err != nil || !filepath.IsAbs(path) {
		c.pathFailure = "Codex executable is not available on PATH"
		return c
	}
	c.path = path
	return c
}

func (c *commands) check(ctx context.Context, clientVersion string) (binaryInfo, string) {
	info := binaryInfo{Path: c.path, ClientVersion: clientVersion, CheckedMS: c.now().UnixMilli()}
	if c.pathFailure != "" {
		return info, c.pathFailure
	}
	if clientVersion == "" {
		return info, "Codex initialization did not name its version"
	}
	ctx, cancel := context.WithTimeout(ctx, c.versionTimeout)
	defer cancel()
	output := &limitedOutput{limit: 4 << 10}
	cmd := exec.CommandContext(ctx, c.path, "--version")
	cmd.Dir, cmd.Env, cmd.WaitDelay = c.dir, c.env, c.waitDelay
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Run(); err != nil {
		return info, "Codex version command did not complete successfully"
	}
	if ctx.Err() != nil || output.overflow {
		return info, "Codex version response exceeded its bound"
	}
	version := ""
	for _, line := range strings.Split(string(output.data), "\n") {
		if fields := strings.Fields(line); len(fields) != 0 {
			version = fields[len(fields)-1]
			break
		}
	}
	if version == "" {
		return info, "Codex version response named no version"
	}
	if version != clientVersion {
		return info, "Codex executable and initialized client versions differ"
	}
	info.Version = version
	return info, ""
}

type commandAnswer struct {
	answer  string
	detail  string
	started bool
}

func (c *commands) queue(ctx context.Context, session, message string, start func(func() error) (bool, error)) commandAnswer {
	ctx, cancel := context.WithTimeout(ctx, c.queueTimeout)
	defer cancel()
	out, errs := &limitedOutput{limit: 64 << 10}, &limitedOutput{limit: 64 << 10}
	cmd := exec.CommandContext(ctx, c.path, "queue", "--thread", session, "--resume", "--message", message)
	cmd.Dir, cmd.Env, cmd.WaitDelay = c.dir, c.env, c.waitDelay
	cmd.Stdout, cmd.Stderr = out, errs
	started, err := start(cmd.Start)
	if !started {
		if err != nil {
			return commandAnswer{answer: "failed", detail: "Codex queue command could not start"}
		}
		return commandAnswer{answer: "aborted", detail: "Root binding changed before command start"}
	}
	err = cmd.Wait()
	if ctx.Err() != nil || out.overflow || errs.overflow {
		return commandAnswer{answer: "unknown", detail: "Codex queue command exceeded its bound", started: true}
	}
	answer, detail := classifyQueue(session, string(out.data), string(errs.data), err)
	return commandAnswer{answer: answer, detail: detail, started: true}
}

// lines accepts a conventional final LF or CRLF, but never trims arbitrary output.
func lines(output string) []string {
	if output == "" {
		return nil
	}
	finalLF := strings.HasSuffix(output, "\n")
	output = strings.TrimSuffix(output, "\n")
	raw := strings.Split(output, "\n")
	for i := range raw {
		if i < len(raw)-1 || finalLF {
			raw[i] = strings.TrimSuffix(raw[i], "\r")
		}
	}
	return raw
}

func acceptedLine(session, line string) bool {
	id, ok := strings.CutPrefix(line, "Queued message ")
	if !ok {
		return false
	}
	id, ok = strings.CutSuffix(id, " for thread "+session+".")
	return ok && id != "" && strings.IndexFunc(id, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) == -1
}

func classifyQueue(session, stdout, stderr string, result error) (string, string) {
	out, errs := lines(stdout), lines(stderr)
	warning := "Warning: message was queued, but could not resume thread " + session + ": start a local daemon or specify a server with --remote"
	if result == nil {
		if len(out) == 1 && acceptedLine(session, out[0]) {
			if len(errs) == 0 {
				return "submitted", ""
			}
			if len(errs) == 1 && errs[0] == warning {
				return "stored", "Codex stored the wake but could not resume its thread"
			}
		}
		if len(out) == 2 && len(errs) == 0 &&
			(acceptedLine(session, out[0]) && out[1] == warning || out[0] == warning && acceptedLine(session, out[1])) {
			return "stored", "Codex stored the wake but could not resume its thread"
		}
	} else {
		var status *exec.ExitError
		// Only the exact recorded same-thread rejection establishes no queue write.
		if errors.As(result, &status) && status.ExitCode() == 1 && len(out) == 0 && len(errs) == 1 &&
			errs[0] == fmt.Sprintf("Error: failed to queue session message: thread/queue/add failed: failed to read thread: invalid thread-store request: no rollout found for thread id %s (code -32603)", session) {
			return "failed", "Codex has no saved rollout for this thread"
		}
	}
	return "unknown", "Codex queue response was not a known outcome"
}

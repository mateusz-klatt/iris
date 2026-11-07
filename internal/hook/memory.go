package hook

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mateusz-klatt/iris/internal/config"
	"github.com/mateusz-klatt/iris/internal/hubclient"
)

// Memory keeps the generation a hook last saw for a client session. After a hub restart a
// session's Execution waits for proof that it is the same session (D9), and a hook is a new
// process for every tool call, so without this the first edits of a recovering session had
// none to give.
type Memory interface {
	Recall(client, session string) int
	Remember(client, session string, generation int)
	Forget(client, session string)
}

// memoryAge is how long an untouched entry survives. A session that ends normally removes its
// own; this only clears the entries of sessions that never reached SessionEnd.
const memoryAge = 30 * 24 * time.Hour

// fileMemory stores one small file per session beside the token, protected as the token is:
// mode 0600 in a 0700 directory, and on Windows the token's owner-only DACL. So anyone who
// can read it can read the token, which already grants everything the generation would.
type fileMemory struct {
	dir      string
	now      func() time.Time
	restrict func(string) error
}

func (m fileMemory) name(client, session string) string {
	sum := sha256.Sum256([]byte(client + "\x00" + session))
	return filepath.Join(m.dir, hex.EncodeToString(sum[:16]))
}

// read returns a file's first line.
func (m fileMemory) read(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(string(raw), "\n")
	return strings.TrimSpace(first)
}

func (m fileMemory) Recall(client, session string) int {
	first := m.read(m.name(client, session))
	generation, err := strconv.Atoi(first)
	if err != nil || generation < 0 {
		return 0
	}
	return generation
}

// Remember is best effort. A failed write only means the next restart costs this session
// its proof, which is what every session had before.
func (m fileMemory) Remember(client, session string, generation int) {
	m.write(m.name(client, session), strconv.Itoa(generation))
}

// Counts are a session's attention numbers: unread deliveries and freed paths.
type Counts struct{ Unread, Available int }

// attentionMemory keeps, per session, the counts the hub last reported and the counts the
// session was last told, so each new event is said once (gap review A4).
type attentionMemory interface {
	Counts(client, session string) (latest, told Counts)
	SetCounts(client, session string, latest, told Counts)
}

// counts is the attention file beside a session's generation. It is separate, so a counts
// write can never race a reattach's generation write and roll the proof back (D9).
func (m fileMemory) counts(client, session string) string {
	return m.name(client, session) + ".attention"
}

func (m fileMemory) Counts(client, session string) (latest, told Counts) {
	rest := m.read(m.counts(client, session))
	var n [4]int
	for i, field := range strings.Fields(rest) {
		if i < len(n) {
			n[i], _ = strconv.Atoi(field)
		}
	}
	return Counts{n[0], n[1]}, Counts{n[2], n[3]}
}

// SetCounts is best effort, as Remember is: losing it can only repeat or skip one note.
func (m fileMemory) SetCounts(client, session string, latest, told Counts) {
	m.write(m.counts(client, session), strconv.Itoa(latest.Unread)+" "+strconv.Itoa(latest.Available)+" "+strconv.Itoa(told.Unread)+" "+strconv.Itoa(told.Available))
}

func (m fileMemory) write(path, body string) {
	if err := os.MkdirAll(m.dir, 0o700); err != nil || m.restrict(m.dir) != nil {
		return
	}
	f, err := os.CreateTemp(m.dir, ".tmp-*")
	if err != nil {
		return
	}
	_, err = f.WriteString(strings.TrimRight(body, "\n") + "\n")
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	// The file is protected before it gets its name, so it is never readable as an entry.
	if err == nil {
		err = m.restrict(f.Name())
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
}

func (m fileMemory) Forget(client, session string) {
	_ = os.Remove(m.name(client, session))
	_ = os.Remove(m.counts(client, session))
}

// prune removes entries untouched for memoryAge, and leftover temporary files.
func (m fileMemory) prune() {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return
	}
	cutoff := m.now().Add(-memoryAge)
	for _, e := range entries {
		info, err := e.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(m.dir, e.Name()))
		}
	}
}

// memory returns the Runner's Memory, or one beside the configured token file. Without a
// token file there is nowhere private to keep it, and the hook behaves as before.
func (r Runner) memory() Memory {
	if r.Memory != nil {
		return r.Memory
	}
	if r.Getenv == nil {
		return nil
	}
	token, err := (hubclient.EnvConfig{Getenv: r.Getenv}).TokenPath()
	if err != nil {
		return nil
	}
	return fileMemory{dir: filepath.Join(filepath.Dir(token), "sessions"), now: time.Now, restrict: config.OwnerOnly}
}

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

func (m fileMemory) Recall(client, session string) int {
	raw, err := os.ReadFile(m.name(client, session))
	if err != nil {
		return 0
	}
	generation, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || generation < 0 {
		return 0
	}
	return generation
}

// Remember is best effort. A failed write only means the next restart costs this session
// its proof, which is what every session had before.
func (m fileMemory) Remember(client, session string, generation int) {
	if err := os.MkdirAll(m.dir, 0o700); err != nil || m.restrict(m.dir) != nil {
		return
	}
	f, err := os.CreateTemp(m.dir, ".tmp-*")
	if err != nil {
		return
	}
	_, err = f.WriteString(strconv.Itoa(generation) + "\n")
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	// The file is protected before it gets its name, so it is never readable as an entry.
	if err == nil {
		err = m.restrict(f.Name())
	}
	if err == nil {
		err = os.Rename(f.Name(), m.name(client, session))
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
}

func (m fileMemory) Forget(client, session string) {
	_ = os.Remove(m.name(client, session))
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

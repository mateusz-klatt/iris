package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/mateusz-klatt/iris/internal/hubclient"
)

const failureLogLimit = 64 << 10
const failureLogBudget = 5 * time.Millisecond

// Failure is a payload-free diagnostic for doctor. Time is in UTC.
type Failure struct {
	Time   time.Time `json:"time"`
	Client string    `json:"client"`
	Event  string    `json:"event"`
	Stage  string    `json:"stage"`
	Error  string    `json:"error"`
}
type hookFailure string

func (e hookFailure) Error() string { return string(e) }

type runState struct {
	mu    sync.Mutex
	stage string
	err   error
}

func (s *runState) at(stage string) { s.mu.Lock(); s.stage = stage; s.mu.Unlock() }
func (s *runState) fail(err error, out Output) Output {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
	return out
}
func (s *runState) snapshot() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stage, s.err
}

// failureText deliberately excludes dependency-provided messages: JSON errors,
// filesystem paths, hub replies and panic values may contain untrusted payload data.
func failureText(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "hook deadline exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "hook operation canceled"
	}
	if errors.Is(err, errNoSession) {
		return errNoSession.Error()
	}
	var own hookFailure
	if errors.As(err, &own) {
		return own.Error()
	}
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return fmt.Sprintf("invalid JSON at byte %d", syntax.Offset)
	}
	var mismatch *json.UnmarshalTypeError
	if errors.As(err, &mismatch) {
		return fmt.Sprintf("invalid JSON field type at byte %d", mismatch.Offset)
	}
	var hub *hubclient.Error
	if errors.As(err, &hub) {
		switch hub.Code {
		case "unauthorized", "bad_request", "not_found", "conflict", "key_reused", "unbound", "ambiguous", "lost", "too_large":
			return "hub request failed: " + hub.Code
		}
		return "hub request failed"
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return failureText(pathErr.Err)
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return failureText(urlErr.Err)
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return failureText(netErr.Err)
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno.Error()
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "incomplete hook input or hub response"
	}
	return fmt.Sprintf("dependency failure (%T)", err)
}
func (r Runner) recordFailure(stage string, err error) {
	// Logging is best effort. A slow disk, permissions failure or competing writer
	// must not delay the allow response beyond this small, fixed budget.
	done := make(chan struct{})
	go func() {
		if r.logged != nil {
			defer r.logged()
		}
		defer close(done)
		defer func() { _ = recover() }()
		token, errPath := (hubclient.EnvConfig{Getenv: r.Getenv}).TokenPath()
		if errPath != nil {
			return
		}
		client := string(Canonical(r.ClientName))
		if client == "" {
			client = "unknown"
		}
		entry := Failure{Time: time.Now().UTC(), Client: client, Event: boundedLabel(r.EventName), Stage: stage, Error: failureText(err)}
		_ = appendFailure(token, entry)
	}()
	after := r.logAfter
	if after == nil {
		after = time.After
	}
	select {
	case <-done:
	case <-after(failureLogBudget):
	}
}
func boundedLabel(label string) string {
	if len(label) > 128 {
		return label[:128]
	}
	return label
}

// logFile is what the failure log needs from a file. Once a file is open its calls
// cannot be made to fail from outside, and the log must still fail safely when they do.
type logFile interface {
	io.Reader
	io.Writer
	io.Seeker
	io.ReaderAt
	io.WriterAt
	io.Closer
	Stat() (os.FileInfo, error)
	Truncate(int64) error
	Fd() uintptr
}

var openFailureLog = func(path string, flag int, perm os.FileMode) (logFile, error) {
	return os.OpenFile(path, flag, perm)
}

func failurePath(tokenFile string) string {
	return filepath.Join(filepath.Dir(tokenFile), "hook-failures.log")
}
func appendFailure(tokenFile string, entry Failure) error {
	path := failurePath(tokenFile)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("failure log is not a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := openFailureLog(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err = lockFailureLog(f); err != nil {
		return err
	}
	defer unlockFailureLog(f)
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("failure log is not regular")
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if len(line) > failureLogLimit/2 {
		return errors.New("failure entry is too large")
	}
	if info.Size()+int64(len(line)) > failureLogLimit {
		size := min(info.Size(), int64(failureLogLimit/2))
		tail := make([]byte, size)
		if _, err = f.ReadAt(tail, info.Size()-size); err != nil {
			return err
		}
		// Keep complete JSON lines from the newest half, then append the new entry.
		if start := bytes.IndexByte(tail, '\n'); start >= 0 {
			tail = tail[start+1:]
		} else {
			tail = nil
		}
		if err = f.Truncate(0); err != nil {
			return err
		}
		if _, err = f.WriteAt(tail, 0); err != nil {
			return err
		}
	}
	if _, err = f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	_, err = f.Write(line)
	return err
}

// ReadRecentFailures reads up to limit newest entries beside tokenFile, oldest
// first. Missing logs are normal; at most 64 KiB is read even from oversized files.
func ReadRecentFailures(tokenFile string, limit int) ([]Failure, error) {
	out := []Failure{}
	if limit <= 0 {
		return out, nil
	}
	path := failurePath(tokenFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("failure log is not regular")
	}
	f, err := openFailureLog(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if err = lockFailureLogShared(f); err != nil {
		return nil, err
	}
	defer unlockFailureLog(f)
	// Size again under the lock: a writer may have grown or cut the file meanwhile.
	if info, err = f.Stat(); err != nil {
		return nil, err
	}
	offset := max(int64(0), info.Size()-failureLogLimit)
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, failureLogLimit))
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			raw = raw[i+1:]
		} else {
			return out, nil
		}
	}
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		var entry Failure
		if json.Unmarshal(line, &entry) == nil {
			out = append(out, entry)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

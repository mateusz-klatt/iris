package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mateusz-klatt/iris/internal/config"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

type registryValue struct{ key, name string }

// present turns a missing key or value into success.
func present(err error) error {
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	return err
}
func (v registryValue) Read() (string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, v.key, registry.QUERY_VALUE)
	if err != nil {
		return "", present(err)
	}
	s, _, err := k.GetStringValue(v.name)
	return s, errors.Join(present(err), k.Close())
}
func (v registryValue) Write(s string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, v.key, registry.SET_VALUE)
	if err != nil {
		return err
	}
	return errors.Join(k.SetStringValue(v.name, s), k.Close())
}
func (v registryValue) Remove() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, v.key, registry.SET_VALUE)
	if err != nil {
		return present(err)
	}
	return errors.Join(present(k.DeleteValue(v.name)), k.Close())
}

// Host uses HKCU Run, which requires no administrator rights. argv is iris serve.
func Host(_, _ string, argv []string) Manager {
	start := append([]string{argv[0], "service", "start"}, argv[2:]...)
	parts := make([]string, len(start))
	for i, arg := range start {
		parts[i] = syscall.EscapeArg(arg)
	}
	cfg, _ := config.Default()
	db, listen := cfg.Database, cfg.Listen
	for i := 2; i+1 < len(argv); i++ {
		switch argv[i] {
		case "--db":
			db = argv[i+1]
		case "--listen":
			listen = argv[i+1]
		}
	}
	return autostartManager{
		value:   registryValue{runKey, "Iris"},
		command: strings.Join(parts, " "),
		process: detachedHub{argv: argv, dir: filepath.Dir(db), stem: hubStem(db), url: "http://" + listen + "/v1/status"},
		legacy:  scheduledTask{ExecRunner{}},
	}
}

// hubStem names the process files of one database, so two hubs sharing a directory
// keep separate identities. The default database keeps the names it has always had,
// which lets an upgrade stop a hub that an earlier release started.
func hubStem(db string) string {
	base := filepath.Base(db)
	if base == "hub.db" {
		return "hub"
	}
	return base + ".hub"
}

type detachedHub struct {
	argv           []string
	dir, stem, url string
}
type processIdentity struct {
	PID     uint32
	Created uint64
}

func creationTime(h windows.Handle) (uint64, error) {
	var c, e, k, u windows.Filetime
	err := windows.GetProcessTimes(h, &c, &e, &k, &u)
	return uint64(c.HighDateTime)<<32 | uint64(c.LowDateTime), err
}
func (p detachedHub) path(suffix string) string { return filepath.Join(p.dir, p.stem+suffix) }
func processCreated(process *os.Process) (uint64, error) {
	var created uint64
	var timesErr error
	err := process.WithHandle(func(handle uintptr) {
		created, timesErr = creationTime(windows.Handle(handle))
	})
	return created, errors.Join(err, timesErr)
}

// record writes the identity Stop relies on. A hub that cannot be identified could
// never be stopped safely, so it does not keep running.
func (p detachedHub) record(process *os.Process, created func(*os.Process) (uint64, error)) error {
	at, err := created(process)
	if err == nil {
		raw, _ := json.Marshal(processIdentity{uint32(process.Pid), at})
		err = os.WriteFile(p.path(".pid"), raw, 0600)
	}
	if err != nil {
		_ = process.Kill()
	}
	return err
}
func (p detachedHub) Responds(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return false
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	// The probe uses only the received status; discarding its body cannot change that result.
	_ = res.Body.Close()
	return res.StatusCode == http.StatusOK
}
func (p detachedHub) locked(f func() error) (err error) {
	if err := os.MkdirAll(p.dir, 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(p.path("-process.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	// Closing the file also releases its lock, even when the operation failed.
	defer func() { err = errors.Join(err, file.Close()) }()
	ov := &windows.Overlapped{}
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ov); err != nil {
		return fmt.Errorf("service operation already in progress: %w", err)
	}
	return f()
}
func (p detachedHub) Start(ctx context.Context) error {
	return p.locked(func() (err error) {
		if p.Responds(ctx) {
			return nil
		}
		// A prior start may still be opening SQLite. Do not replace its identity.
		if h, err := p.openRecorded(); err != nil {
			return err
		} else if h != 0 {
			return windows.CloseHandle(h)
		}
		log, err := os.OpenFile(p.path(".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, log.Close()) }()
		cmd := exec.Command(p.argv[0], p.argv[1:]...)
		cmd.Stdout, cmd.Stderr = log, log
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP, HideWindow: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		// Keep the process handle open until its creation time has been recorded.
		defer func() { err = errors.Join(err, cmd.Process.Release()) }()
		return p.record(cmd.Process, processCreated)
	})
}

// openRecorded verifies creation time using the SAME handle later used to stop
// the process, so PID reuse between checking and termination cannot kill a peer.
func (p detachedHub) openRecorded() (windows.Handle, error) {
	raw, err := os.ReadFile(p.path(".pid"))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var id processIdentity
	if err := json.Unmarshal(raw, &id); err != nil {
		return 0, err
	}
	if id.PID == 0 || id.Created == 0 {
		return 0, errors.New("invalid hub process identity")
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, id.PID)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	// Either call failing leaves a value that does not match, so the process is not ours.
	created, _ := creationTime(h)
	state, _ := windows.WaitForSingleObject(h, 0)
	if created != id.Created || state != uint32(windows.WAIT_TIMEOUT) {
		return 0, windows.CloseHandle(h)
	}
	return h, nil
}

// terminate judges by the wait, not the call: terminating a process that has
// already exited fails, and that hub has stopped all the same.
func terminate(h windows.Handle, waitMS uint32) error {
	_ = windows.TerminateProcess(h, 0)
	if state, _ := windows.WaitForSingleObject(h, waitMS); state != windows.WAIT_OBJECT_0 {
		return errors.New("hub did not stop")
	}
	return nil
}
func (p detachedHub) Stop(_ context.Context) error {
	return p.locked(func() (err error) {
		h, err := p.openRecorded()
		if err != nil {
			return err
		}
		if h == 0 {
			return p.forget(nil)
		}
		defer func() { err = errors.Join(err, windows.CloseHandle(h)) }()
		return p.forget(terminate(h, 5000))
	})
}

// forget drops the identity once the hub is gone. A hub that did not stop keeps it,
// so a later stop can still find the process.
func (p detachedHub) forget(stopErr error) error {
	if stopErr != nil {
		return stopErr
	}
	err := os.Remove(p.path(".pid"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

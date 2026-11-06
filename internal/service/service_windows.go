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

type registryValue struct{ name string }

func (v registryValue) Read() (string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer k.Close()
	s, _, err := k.GetStringValue(v.name)
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	return s, err
}
func (v registryValue) Write(s string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(v.name, s)
}
func (v registryValue) Remove() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer k.Close()
	err = k.DeleteValue(v.name)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	return err
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
	return autostartManager{value: registryValue{"Iris"}, command: strings.Join(parts, " "), process: detachedHub{argv: argv, dir: filepath.Dir(db), url: "http://" + listen + "/v1/status"}}
}

type detachedHub struct {
	argv     []string
	dir, url string
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
	res.Body.Close()
	return res.StatusCode == http.StatusOK
}
func (p detachedHub) locked(f func() error) error {
	if err := os.MkdirAll(p.dir, 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(p.dir, "hub-process.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	ov := &windows.Overlapped{}
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ov); err != nil {
		return fmt.Errorf("service operation already in progress: %w", err)
	}
	defer windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, ov)
	return f()
}
func (p detachedHub) Start(ctx context.Context) error {
	return p.locked(func() error {
		if p.Responds(ctx) {
			return nil
		}
		// A prior start may still be opening SQLite. Do not replace its identity.
		if h, err := p.openRecorded(); err != nil {
			return err
		} else if h != 0 {
			windows.CloseHandle(h)
			return nil
		}
		log, err := os.OpenFile(filepath.Join(p.dir, "hub.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return err
		}
		defer log.Close()
		cmd := exec.Command(p.argv[0], p.argv[1:]...)
		cmd.Stdout, cmd.Stderr = log, log
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP, HideWindow: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		// Keep the process handle open until its creation time has been recorded.
		defer cmd.Process.Release()
		var created uint64
		var identityErr error
		err = cmd.Process.WithHandle(func(handle uintptr) {
			created, identityErr = creationTime(windows.Handle(handle))
		})
		if err == nil {
			err = identityErr
		}
		if err != nil {
			_ = cmd.Process.Kill()
			return err
		}
		raw, err := json.Marshal(processIdentity{uint32(cmd.Process.Pid), created})
		if err == nil {
			err = os.WriteFile(filepath.Join(p.dir, "hub.pid"), raw, 0600)
		}
		if err != nil {
			_ = cmd.Process.Kill()
		}
		return err
	})
}

// openRecorded verifies creation time using the SAME handle later used to stop
// the process, so PID reuse between checking and termination cannot kill a peer.
func (p detachedHub) openRecorded() (windows.Handle, error) {
	raw, err := os.ReadFile(filepath.Join(p.dir, "hub.pid"))
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
	created, err := creationTime(h)
	if err != nil {
		windows.CloseHandle(h)
		return 0, err
	}
	state, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		windows.CloseHandle(h)
		return 0, err
	}
	if created != id.Created || state == windows.WAIT_OBJECT_0 {
		windows.CloseHandle(h)
		return 0, nil
	}
	return h, nil
}
func (p detachedHub) Stop(_ context.Context) error {
	return p.locked(func() error {
		h, err := p.openRecorded()
		if err != nil {
			return err
		}
		if h != 0 {
			defer windows.CloseHandle(h)
			if err := windows.TerminateProcess(h, 0); err != nil {
				return err
			}
			if state, err := windows.WaitForSingleObject(h, 5000); err != nil {
				return err
			} else if state != windows.WAIT_OBJECT_0 {
				return errors.New("hub did not stop")
			}
		}
		err = os.Remove(filepath.Join(p.dir, "hub.pid"))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	})
}

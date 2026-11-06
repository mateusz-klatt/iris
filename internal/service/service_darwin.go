package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type launchdManager struct {
	plist string
	argv  []string
	uid   int
	run   Runner
}

func newManager(home, _ string, argv []string, run Runner) Manager {
	return launchdManager{
		plist: filepath.Join(home, "Library", "LaunchAgents", "iris.plist"),
		argv:  argv,
		uid:   os.Getuid(),
		run:   run,
	}
}

// Host returns the launchd manager for this machine.
func Host(home, configHome string, argv []string) Manager {
	return newManager(home, configHome, argv, ExecRunner{})
}

func (m launchdManager) domain() string { return fmt.Sprintf("gui/%d", m.uid) }
func (m launchdManager) target() string { return m.domain() + "/iris" }

// loaded reports whether launchd holds the job, which is not the same as the hub running.
func (m launchdManager) loaded(ctx context.Context) bool {
	_, err := m.run.Run(ctx, "launchctl", "print", m.target())
	return err == nil
}

// running reads the job's own state from launchctl print. Apple documents that output as
// not being API, so a format change reports an inactive hub rather than a live one. A loaded
// job can be "spawn scheduled" while KeepAlive restarts a hub that keeps exiting.
func running(detail string) bool {
	return strings.Contains("\n"+detail+"\n", "\n\tstate = running\n")
}

func (m launchdManager) Install(ctx context.Context) error {
	if data, err := os.ReadFile(m.plist); err == nil && !bytes.Contains(data, []byte(Marker)) {
		return errors.New("iris.plist exists and is not managed by Iris")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.plist), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(m.plist, LaunchdPlist(m.argv), 0o644); err != nil {
		return err
	}
	// launchd refuses to bootstrap a label it already holds, and would keep the old arguments.
	// When nothing is loaded this bootout fails, which is the normal first install.
	_, _ = m.run.Run(ctx, "launchctl", "bootout", m.target())
	_, err := m.run.Run(ctx, "launchctl", "bootstrap", m.domain(), m.plist)
	return err
}

func (m launchdManager) Uninstall(ctx context.Context) error {
	data, err := os.ReadFile(m.plist)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !bytes.Contains(data, []byte(Marker)) {
		return errors.New("iris.plist exists and is not managed by Iris")
	}
	_, _ = m.run.Run(ctx, "launchctl", "bootout", m.target())
	return os.Remove(m.plist)
}

func (m launchdManager) Status(ctx context.Context) (Status, error) {
	_, err := os.Stat(m.plist)
	installed := err == nil
	out, runErr := m.run.Run(ctx, "launchctl", "print", m.target())
	return Status{Installed: installed, Active: installed && runErr == nil && running(out), Detail: out}, nil
}

func (m launchdManager) Start(ctx context.Context) error {
	if _, err := os.Stat(m.plist); err != nil {
		return fmt.Errorf("the Iris service is not installed: %w", err)
	}
	if m.loaded(ctx) {
		return nil
	}
	_, err := m.run.Run(ctx, "launchctl", "bootstrap", m.domain(), m.plist)
	return err
}

func (m launchdManager) Stop(ctx context.Context) error {
	if !m.loaded(ctx) {
		return nil
	}
	_, err := m.run.Run(ctx, "launchctl", "bootout", m.target())
	return err
}

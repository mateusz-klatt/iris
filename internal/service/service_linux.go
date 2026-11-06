package service

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
)

type systemdManager struct {
	unit string
	argv []string
	run  Runner
}

func newManager(home, configHome string, argv []string, run Runner) Manager {
	base := configHome
	if base == "" {
		base = filepath.Join(home, ".config")
	}
	return systemdManager{unit: filepath.Join(base, "systemd", "user", "iris.service"), argv: argv, run: run}
}

// Host returns the systemd --user manager for this machine.
func Host(home, configHome string, argv []string) Manager {
	return newManager(home, configHome, argv, ExecRunner{})
}

func (m systemdManager) Install(ctx context.Context) error {
	if data, err := os.ReadFile(m.unit); err == nil && !bytes.Contains(data, []byte(Marker)) {
		return errors.New("iris.service exists and is not managed by Iris")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.unit), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(m.unit, SystemdUnit(m.argv), 0o644); err != nil {
		return err
	}
	if _, err := m.run.Run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	_, err := m.run.Run(ctx, "systemctl", "--user", "enable", "--now", "iris.service")
	return err
}

func (m systemdManager) Uninstall(ctx context.Context) error {
	data, err := os.ReadFile(m.unit)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !bytes.Contains(data, []byte(Marker)) {
		return errors.New("iris.service exists and is not managed by Iris")
	}
	_, _ = m.run.Run(ctx, "systemctl", "--user", "disable", "--now", "iris.service")
	if err := os.Remove(m.unit); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err = m.run.Run(ctx, "systemctl", "--user", "daemon-reload")
	return err
}

func (m systemdManager) Status(ctx context.Context) (Status, error) {
	_, err := os.Stat(m.unit)
	installed := err == nil
	out, runErr := m.run.Run(ctx, "systemctl", "--user", "is-active", "iris.service")
	active := runErr == nil && (out == "active" || out == "")
	detail := out
	if runErr != nil && detail == "" {
		detail = runErr.Error()
	}
	return Status{Installed: installed, Active: active && installed, Detail: detail}, nil
}

func (m systemdManager) Start(ctx context.Context) error {
	_, err := m.run.Run(ctx, "systemctl", "--user", "start", "iris.service")
	return err
}
func (m systemdManager) Stop(ctx context.Context) error {
	_, err := m.run.Run(ctx, "systemctl", "--user", "stop", "iris.service")
	return err
}

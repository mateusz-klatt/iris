package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// runValue isolates the per-user registry value from lifecycle decisions.
// Missing values are reported as an empty string.
type runValue interface {
	Read() (string, error)
	Write(string) error
	Remove() error
}
type background interface {
	Start(context.Context) error
	Stop(context.Context) error
	Responds(context.Context) bool
}

// retired is autostart registered by an earlier release.
type retired interface {
	Remove(context.Context) error
}
type autostartManager struct {
	value   runValue
	command string
	process background
	legacy  retired
}

func (m autostartManager) Start(ctx context.Context) error {
	if m.process.Responds(ctx) {
		return nil
	}
	return m.process.Start(ctx)
}
func (m autostartManager) Stop(ctx context.Context) error { return m.process.Stop(ctx) }
func (m autostartManager) Install(ctx context.Context) error {
	if err := m.value.Write(m.command); err != nil {
		return err
	}
	if err := m.Start(ctx); err != nil {
		return err
	}
	// Installation verifies readiness; service start itself returns after spawning.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for !m.process.Responds(ctx) {
		select {
		case <-ctx.Done():
			return errors.New("hub did not respond after starting; inspect the hub log beside the database")
		case <-time.After(100 * time.Millisecond):
		}
	}
	// A task an administrator registered can refuse removal. The hub still runs, so say so.
	if err := m.legacy.Remove(ctx); err != nil {
		return fmt.Errorf("hub is running, but the old Iris scheduled task remains: %w", err)
	}
	return nil
}
func (m autostartManager) Uninstall(ctx context.Context) error {
	return errors.Join(m.Stop(ctx), m.value.Remove(), m.legacy.Remove(ctx))
}
func (m autostartManager) Status(ctx context.Context) (Status, error) {
	value, err := m.value.Read()
	if err != nil {
		return Status{}, err
	}
	return Status{Installed: value != "", Active: m.process.Responds(ctx), Detail: value}, nil
}

// scheduledTask is the logon task from before ADR 001. Upgrading leaves it behind, and
// it would keep starting a hub after uninstall. schtasks prints localized text, so only
// the exit code and the XML are read. A failed query cannot tell a missing task from one
// this user may not read, and both are left alone.
type scheduledTask struct{ run Runner }

func (t scheduledTask) Remove(ctx context.Context) error {
	definition, err := t.run.Run(ctx, "schtasks", "/Query", "/TN", "Iris", "/XML")
	if err != nil || !strings.Contains(definition, "<Arguments>serve --listen") {
		return nil
	}
	_, err = t.run.Run(ctx, "schtasks", "/Delete", "/F", "/TN", "Iris")
	return err
}

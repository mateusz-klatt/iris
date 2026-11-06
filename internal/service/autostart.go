package service

import (
	"context"
	"errors"
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
type autostartManager struct {
	value   runValue
	command string
	process background
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
	for {
		if m.process.Responds(ctx) {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("hub did not respond after starting; inspect hub.log beside the database")
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func (m autostartManager) Uninstall(ctx context.Context) error {
	return errors.Join(m.Stop(ctx), m.value.Remove())
}
func (m autostartManager) Status(ctx context.Context) (Status, error) {
	value, err := m.value.Read()
	if err != nil {
		return Status{}, err
	}
	return Status{Installed: value != "", Active: m.process.Responds(ctx), Detail: value}, nil
}

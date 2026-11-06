package service

import (
	"context"
	"strings"
)

type taskManager struct {
	argv []string
	run  Runner
}

func newManager(_, _ string, argv []string, run Runner) Manager {
	return taskManager{argv: argv, run: run}
}

// Host returns the Task Scheduler manager for this machine.
func Host(home, configHome string, argv []string) Manager {
	return newManager(home, configHome, argv, ExecRunner{})
}

func (m taskManager) Install(ctx context.Context) error {
	_, err := m.run.Run(ctx, "schtasks", TaskCreateArgs(m.argv)...)
	return err
}

func (m taskManager) Uninstall(ctx context.Context) error {
	_, err := m.run.Run(ctx, "schtasks", TaskDeleteArgs()...)
	return err
}

func (m taskManager) Status(ctx context.Context) (Status, error) {
	out, err := m.run.Run(ctx, "schtasks", TaskQueryArgs()...)
	if err != nil {
		return Status{Detail: err.Error()}, nil
	}
	active := strings.Contains(strings.ToLower(out), "running") || strings.Contains(out, "Iris")
	return Status{Installed: true, Active: active, Detail: out}, nil
}

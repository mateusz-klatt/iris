//go:build !linux && !darwin && !windows

package service

import (
	"context"
	"errors"
)

type unsupported struct{}

func newManager(_, _ string, _ []string, _ Runner) Manager { return unsupported{} }

// Host reports that this OS has no Iris service integration.
func Host(_, _ string, _ []string) Manager { return unsupported{} }

func (unsupported) Install(context.Context) error {
	return errors.New("iris service is not supported on this OS")
}

func (unsupported) Uninstall(context.Context) error {
	return errors.New("iris service is not supported on this OS")
}

func (unsupported) Status(context.Context) (Status, error) {
	return Status{}, errors.New("iris service is not supported on this OS")
}

func (unsupported) Start(context.Context) error {
	return errors.New("iris service is not supported on this OS")
}
func (unsupported) Stop(context.Context) error {
	return errors.New("iris service is not supported on this OS")
}

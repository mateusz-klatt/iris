//go:build windows

package config

import (
	"errors"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func pinnedConfigDirWindows(profile func() (string, error)) (string, error) {
	dir, err := profile()
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(dir) {
		return "", errors.New("user profile is not an absolute path")
	}
	return filepath.Join(dir, "AppData", "Roaming", "iris"), nil
}

func windowsProfileDir() (string, error) {
	return windows.GetCurrentProcessToken().GetUserProfileDirectory()
}

func pinnedConfigDirOS() (string, error) {
	return pinnedConfigDirWindows(windowsProfileDir)
}

func pinnedRuntimeEnvWindows(sysDir func() (string, error)) ([]string, error) {
	dir, err := sysDir()
	if err != nil {
		return nil, err
	}
	root := filepath.Dir(dir)
	if !filepath.IsAbs(root) {
		return nil, errors.New("system root is not an absolute path")
	}
	return []string{"SYSTEMROOT=" + root}, nil
}

func windowsSystemDir() (string, error) {
	return windows.GetSystemDirectory()
}

func pinnedRuntimeEnvOS() ([]string, error) {
	return pinnedRuntimeEnvWindows(windowsSystemDir)
}

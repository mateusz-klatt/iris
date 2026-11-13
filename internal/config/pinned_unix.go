//go:build !windows

package config

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
)

func pinnedAccountHome(lookup func(string) (*user.User, error), uid int) (string, error) {
	u, err := lookup(strconv.Itoa(uid))
	if err != nil {
		return "", err
	}
	if u == nil || !filepath.IsAbs(u.HomeDir) {
		return "", errors.New("os account has no absolute home directory")
	}
	return u.HomeDir, nil
}

func pinnedConfigDirUnix(lookup func(string) (*user.User, error), uid int, goos string) (string, error) {
	home, err := pinnedAccountHome(lookup, uid)
	if err != nil {
		return "", err
	}
	if goos == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "iris"), nil
	}
	return filepath.Join(home, ".config", "iris"), nil
}

func pinnedConfigDirOS() (string, error) {
	return pinnedConfigDirUnix(user.LookupId, os.Getuid(), runtime.GOOS)
}

func pinnedRuntimeEnvUnix(lookup func(string) (*user.User, error), uid int) ([]string, error) {
	home, err := pinnedAccountHome(lookup, uid)
	if err != nil {
		return nil, err
	}
	return []string{"HOME=" + home}, nil
}

func pinnedRuntimeEnvOS() ([]string, error) {
	return pinnedRuntimeEnvUnix(user.LookupId, os.Getuid())
}

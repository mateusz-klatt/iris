//go:build linux

package main

import "os/exec"

func browserCommand(url string) *exec.Cmd {
	return exec.Command("xdg-open", url)
}

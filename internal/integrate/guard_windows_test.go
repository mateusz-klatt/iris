//go:build windows

package integrate

import (
	"os/exec"
	"strings"
	"syscall"
)

// startGuard passes the guard text as the raw command line. Go's usual
// argument escaping would insert backslashes that cmd and PowerShell, when a
// client runs the same text, do not insert.
func startGuard(script string) *exec.Cmd {
	if strings.Contains(script, "Get-Command") {
		exe := powershellExe()
		cmd := exec.Command(exe)
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: quoteExe(exe) + " -NoProfile -NonInteractive -Command " + script}
		return cmd
	}
	cmd := exec.Command("cmd")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: "cmd.exe /d /c " + script}
	return cmd
}

func powershellExe() string {
	if p, err := exec.LookPath("pwsh"); err == nil {
		return p
	}
	return "powershell"
}

func quoteExe(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`
	}
	return s
}

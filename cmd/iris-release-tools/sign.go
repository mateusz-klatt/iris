package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const signingDeadline = 5 * time.Minute

// These process boundaries let tests exercise failure and timeout behaviour without Certum.
var signTimeout = signingDeadline
var openNull = func() (*os.File, error) { return os.OpenFile(os.DevNull, os.O_RDWR, 0) }

func signWindows(arch, path string, getenv func(string) string) error {
	if arch != "amd64" && arch != "arm64" {
		return fmt.Errorf("release tools: unsupported Windows architecture")
	}
	signer := getenv("IRIS_WINDOWS_SIGNER")
	runtimeDir := getenv("XDG_RUNTIME_DIR")
	if getenv("CERTUM_EMAIL") == "" || getenv("CERTUM_OTP") == "" || !filepath.IsAbs(signer) || !filepath.IsAbs(runtimeDir) {
		return fmt.Errorf("release tools: signing configuration is incomplete")
	}
	for _, name := range []string{signer, path} {
		info, err := os.Stat(name)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("release tools: signing input is unavailable")
		}
	}
	if os.Chmod(signer, 0700) != nil || os.MkdirAll(filepath.Join(runtimeDir, "ssign"), 0700) != nil {
		return fmt.Errorf("release tools: signing preparation failed")
	}
	null, err := openNull()
	if err != nil {
		return fmt.Errorf("release tools: signing preparation failed")
	}
	defer func() { _ = null.Close() }()
	env := make([]string, 0, 5)
	for _, key := range []string{"CERTUM_EMAIL", "CERTUM_OTP", "XDG_RUNTIME_DIR", "HOME", "PATH"} {
		env = append(env, key+"="+getenv(key))
	}
	// os/exec adds SYSTEMROOT on Windows. StartProcess preserves the exact allowlist.
	process, err := os.StartProcess(signer, []string{signer, path}, &os.ProcAttr{
		Env: env, Files: []*os.File{null, null, null},
	})
	if err != nil {
		return fmt.Errorf("release tools: Windows signing failed")
	}
	done := make(chan bool, 1)
	go func() {
		state, err := process.Wait()
		done <- err == nil && state.Success()
	}()
	timer := time.NewTimer(signTimeout)
	defer timer.Stop()
	select {
	case success := <-done:
		if !success {
			return fmt.Errorf("release tools: Windows signing failed")
		}
		return nil
	case <-timer.C:
		_ = process.Kill()
		<-done
		return fmt.Errorf("release tools: Windows signing deadline exceeded")
	}
}

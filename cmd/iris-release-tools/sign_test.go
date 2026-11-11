package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const fakeSignerSource = `package main
import("encoding/json"; "fmt"; "os"; "time")
func main() {
 var config struct { Log string; Sleep bool; Exit int }
 data, _ := os.ReadFile(os.Args[1]); _ = json.Unmarshal(data, &config)
 file, _ := os.OpenFile(config.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
 _ = json.NewEncoder(file).Encode(map[string]any{"args":os.Args[1:], "env":os.Environ()}); _ = file.Close()
 fmt.Fprintln(os.Stdout,"synthetic signer private stdout"); fmt.Fprintln(os.Stderr,"synthetic signer private stderr")
 if config.Sleep { time.Sleep(time.Hour) }
 _ = os.WriteFile(os.Args[1], []byte("synthetically signed in place"), 0600)
 os.Exit(config.Exit)
}`

func signerFixture(t *testing.T) (string, string, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "fake.go")
	if err := os.WriteFile(source, []byte(fakeSignerSource), 0600); err != nil {
		t.Fatal(err)
	}
	signer := filepath.Join(dir, "synthetic signer")
	if runtime.GOOS == "windows" {
		signer += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", signer, source)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build synthetic signer: %v\n%s", err, output)
	}
	target := filepath.Join(dir, "target with spaces.exe")
	env := map[string]string{"IRIS_WINDOWS_SIGNER": signer, "CERTUM_EMAIL": "synthetic@example.invalid", "CERTUM_OTP": "synthetic seed", "XDG_RUNTIME_DIR": filepath.Join(dir, "runtime"), "HOME": dir, "PATH": dir}
	return target, filepath.Join(dir, "calls.jsonl"), env
}

func signerTarget(t *testing.T, target, log string, sleep bool, code int) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"Log": log, "Sleep": sleep, "Exit": code})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSigningSubprocessAllowlistAndSingleCall(t *testing.T) {
	target, log, env := signerFixture(t)
	signerTarget(t, target, log, false, 0)
	for _, key := range []string{"GITHUB_TOKEN", "ACTIONS_ID_TOKEN_REQUEST_TOKEN", "ACTIONS_ID_TOKEN_REQUEST_URL", "MACOS_SIGN_P12", "MACOS_SIGN_PASSWORD", "MACOS_NOTARY_KEY", "GITHUB_ENV", "GITHUB_OUTPUT", "GITHUB_STEP_SUMMARY"} { // gitleaks:allow (variable names, not credentials)
		t.Setenv(key, "synthetic forbidden value")
		env[key] = "synthetic forbidden value"
	}
	var out, stderr bytes.Buffer
	if got := dispatch([]string{"sign-windows", "--arch=amd64", "--path=" + target}, func(k string) string { return env[k] }, &out, &stderr); got != 0 || out.String() != "Windows signing completed.\n" || stderr.Len() != 0 {
		t.Fatalf("sign status %d stdout %q stderr %q", got, out.String(), stderr.String())
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(data, []byte{'\n'}) != 1 {
		t.Fatalf("expected one signer call")
	}
	var record struct {
		Args []string
		Env  []string
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if len(record.Args) != 1 || record.Args[0] != target {
		t.Fatalf("positional target: %v", record.Args)
	}
	if len(record.Env) != 5 {
		t.Fatalf("expected five child variables, got %d", len(record.Env))
	}
	for _, entry := range record.Env {
		key, value, _ := strings.Cut(entry, "=")
		if key != "CERTUM_EMAIL" && key != "CERTUM_OTP" && key != "XDG_RUNTIME_DIR" && key != "HOME" && key != "PATH" {
			t.Fatalf("unexpected child variable %q", key)
		}
		if value != env[key] {
			t.Fatalf("wrong child value for %s", key)
		}
	}
	if info, err := os.Stat(filepath.Join(env["XDG_RUNTIME_DIR"], "ssign")); err != nil || !info.IsDir() {
		t.Fatalf("cache: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(env["XDG_RUNTIME_DIR"], "ssign"))
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("cache permissions: %v", err)
		}
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "synthetically signed in place" {
		t.Fatalf("signer did not modify positional input: %v", err)
	}
	if signingDeadline != 5*time.Minute {
		t.Fatal("signing deadline changed")
	}
	// Capture the real CLI's process streams as well: a child inheriting stdout or
	// stderr would leak its distinctive private markers outside dispatch's writers.
	helper := filepath.Join(t.TempDir(), "iris-release-tools")
	if runtime.GOOS == "windows" {
		helper += ".exe"
	}
	build := exec.Command("go", "build", "-o", helper, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v\n%s", err, output)
	}
	for _, code := range []int{0, 27} {
		cliLog := filepath.Join(filepath.Dir(log), fmt.Sprintf("cli-%d.jsonl", code))
		signerTarget(t, target, cliLog, false, code)
		cmd := exec.Command(helper, "sign-windows", "--arch=amd64", "--path="+target)
		cmd.Env = os.Environ()
		for key, value := range env {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
		output, err := cmd.CombinedOutput()
		want := "Windows signing completed.\n"
		if code != 0 {
			want = "release tools: Windows signing failed\n"
		}
		if (err == nil) != (code == 0) || string(output) != want {
			t.Fatalf("CLI status error %v, output %q", err, output)
		}
	}
}

func TestSigningFailuresAreSanitizedAndNotRetried(t *testing.T) {
	target, log, env := signerFixture(t)
	getenv := func(k string) string { return env[k] }
	signerTarget(t, target, log, false, 27)
	var out, stderr bytes.Buffer
	if got := dispatch([]string{"sign-windows", "--arch=arm64", "--path=" + target}, getenv, &out, &stderr); got != 1 || out.Len() != 0 || stderr.String() != "release tools: Windows signing failed\n" {
		t.Fatalf("failure status %d output %q / %q", got, out.String(), stderr.String())
	}
	data, err := os.ReadFile(log)
	if err != nil || bytes.Count(data, []byte{'\n'}) != 1 {
		t.Fatalf("failure was retried: %v", err)
	}
	// A different log proves the timed-out child was launched once and reaped.
	deadlineLog := filepath.Join(filepath.Dir(log), "deadline.jsonl")
	signerTarget(t, target, deadlineLog, true, 0)
	old := signTimeout
	signTimeout = 2 * time.Second
	t.Cleanup(func() { signTimeout = old })
	start := time.Now()
	err = signWindows("amd64", target, getenv)
	if err == nil || err.Error() != "release tools: Windows signing deadline exceeded" || time.Since(start) > 10*time.Second {
		t.Fatalf("deadline: %v", err)
	}
	data, err = os.ReadFile(deadlineLog)
	if err != nil || bytes.Count(data, []byte{'\n'}) != 1 {
		t.Fatalf("deadline call count: %v", err)
	}
}

func TestSigningValidationAndPreparation(t *testing.T) {
	dir := t.TempDir()
	signer, target := filepath.Join(dir, "not executable"), filepath.Join(dir, "input.exe")
	for _, path := range []string{signer, target} {
		if err := os.WriteFile(path, []byte("synthetic"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	base := map[string]string{"IRIS_WINDOWS_SIGNER": signer, "CERTUM_EMAIL": "fake", "CERTUM_OTP": "fake", "XDG_RUNTIME_DIR": filepath.Join(dir, "runtime")}
	for _, tc := range []struct{ name, key, value, arch, path, want string }{
		{"architecture", "", "", "386", target, "unsupported Windows architecture"},
		{"email", "CERTUM_EMAIL", "", "amd64", target, "signing configuration is incomplete"},
		{"seed", "CERTUM_OTP", "", "amd64", target, "signing configuration is incomplete"},
		{"relative signer", "IRIS_WINDOWS_SIGNER", "ssign", "amd64", target, "signing configuration is incomplete"},
		{"relative runtime", "XDG_RUNTIME_DIR", "runtime", "amd64", target, "signing configuration is incomplete"},
		{"missing signer", "IRIS_WINDOWS_SIGNER", filepath.Join(dir, "absent"), "amd64", target, "signing input is unavailable"},
		{"directory signer", "IRIS_WINDOWS_SIGNER", dir, "amd64", target, "signing input is unavailable"},
		{"missing target", "", "", "amd64", filepath.Join(dir, "absent"), "signing input is unavailable"},
		{"directory target", "", "", "amd64", dir, "signing input is unavailable"},
		{"cache blocked", "XDG_RUNTIME_DIR", target, "amd64", target, "signing preparation failed"},
		{"invalid signer image", "", "", "amd64", target, "Windows signing failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(key string) string {
				if key == tc.key {
					return tc.value
				}
				return base[key]
			}
			err := signWindows(tc.arch, tc.path, getenv)
			if err == nil || err.Error() != "release tools: "+tc.want {
				t.Fatalf("got %v, expected %s", err, tc.want)
			}
		})
	}
	old := openNull
	openNull = func() (*os.File, error) { return nil, fmt.Errorf("synthetic private device error") }
	t.Cleanup(func() { openNull = old })
	if err := signWindows("amd64", target, func(k string) string { return base[k] }); err == nil || err.Error() != "release tools: signing preparation failed" {
		t.Fatalf("device failure: %v", err)
	}
}

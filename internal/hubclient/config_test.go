package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestConfigPrecedence(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "hub.token")
	env := map[string]string{}
	reads := 0
	e := EnvConfig{DefaultTokenFile: token, Getenv: func(k string) string { return env[k] }, ReadFile: func(p string) ([]byte, error) { reads++; return os.ReadFile(p) }}
	r := e.Load()
	if r.HubURL() != DefaultHubURL || r.Source != "default" {
		t.Fatal(r)
	}
	if err := WriteConfig(filepath.Join(dir, "client.json"), FileConfig{Hub: "https://file.example", TokenFile: "other.token"}); err != nil {
		t.Fatal(err)
	}
	reads = 0
	r = e.Load()
	if r.HubURL() != "https://file.example" || !strings.HasPrefix(r.Source, "file (") || r.TokenFile != filepath.Join(dir, "other.token") {
		t.Fatal(r)
	}
	r.HubURL()
	_, _ = r.TokenPath()
	r.HubURL()
	if reads != 1 {
		t.Fatal("configuration reread", reads)
	}
	env["IRIS_HUB_URL"] = "https://env.example"
	r = e.Load()
	if r.HubURL() != "https://env.example" || r.TokenFile != filepath.Join(dir, "other.token") {
		t.Fatal("URL override changed file token", r)
	}
	env["IRIS_TOKEN_FILE"] = token
	r = e.Load()
	if r.HubURL() != "https://env.example" || r.Source != "environment (IRIS_HUB_URL)" || r.TokenFile != token {
		t.Fatal(r)
	}
	if err := os.WriteFile(filepath.Join(dir, "client.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	loaded := e.Load()
	path, err := loaded.TokenPath()
	if err != nil || path != token {
		t.Fatalf("failure log lost its token path: %q %v", path, err)
	}
	if _, err = loaded.Token(); err == nil || loaded.HubURL() != "" {
		t.Fatal("malformed config silently ignored")
	}
}

func TestConfigErrReportsWhatTokenPathTolerates(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "hub.token")
	if err := os.WriteFile(filepath.Join(dir, "client.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := (EnvConfig{Getenv: func(string) string { return "" }, DefaultTokenFile: token}).Load()
	if got, err := r.TokenPath(); err != nil || got != token {
		t.Fatal(got, err)
	}
	if r.ConfigErr() == nil {
		t.Fatal("a malformed client.json was not reported")
	}
}

func TestBaseAndEnvTokenPathSkipClientConfig(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "hub.token")
	explicit := filepath.Join(dir, "explicit.token")
	noRead := func(path string) ([]byte, error) {
		t.Fatalf("unexpected read of %s", path)
		return nil, nil
	}
	withEnv := EnvConfig{
		DefaultTokenFile: token,
		Getenv: func(k string) string {
			if k == "IRIS_TOKEN_FILE" {
				return explicit
			}
			return ""
		},
		ReadFile: noRead,
	}
	if got, fromEnv, err := withEnv.BaseTokenPath(); err != nil || !fromEnv || got != explicit {
		t.Fatalf("BaseTokenPath() = (%q, %v, %v), want (%q, true, nil)", got, fromEnv, err, explicit)
	}
	if got, err := withEnv.TokenPath(); err != nil || got != explicit {
		t.Fatalf("TokenPath() = (%q, %v), want (%q, nil)", got, err, explicit)
	}
	withoutEnv := EnvConfig{
		DefaultTokenFile: token,
		Getenv:           func(string) string { return "" },
		ReadFile:         noRead,
	}
	if got, fromEnv, err := withoutEnv.BaseTokenPath(); err != nil || fromEnv || got != token {
		t.Fatalf("BaseTokenPath() = (%q, %v, %v), want (%q, false, nil)", got, fromEnv, err, token)
	}
	if err := WriteConfig(filepath.Join(dir, "client.json"), FileConfig{Hub: "https://file.example", TokenFile: "other.token"}); err != nil {
		t.Fatal(err)
	}
	withFile := EnvConfig{
		DefaultTokenFile: token,
		Getenv:           func(string) string { return "" },
		ReadFile:         os.ReadFile,
	}
	if got, err := withFile.TokenPath(); err != nil || got != filepath.Join(dir, "other.token") {
		t.Fatalf("TokenPath() with client.json = (%q, %v)", got, err)
	}
	if got, err := withEnv.BoundedTokenPath(15*time.Millisecond, nil); err != nil || got != explicit {
		t.Fatalf("BoundedTokenPath(env) = (%q, %v), want (%q, nil)", got, err, explicit)
	}
	if got, err := withFile.BoundedTokenPath(time.Second, nil); err != nil || got != filepath.Join(dir, "other.token") {
		t.Fatalf("BoundedTokenPath(file) = (%q, %v)", got, err)
	}
	entered := make(chan struct{})
	unblock := make(chan struct{})
	done := make(chan struct{})
	defer func() {
		close(unblock)
		<-done
	}()
	stalled := EnvConfig{
		DefaultTokenFile: token,
		Getenv:           func(string) string { return "" },
		ReadFile: func(string) ([]byte, error) {
			defer close(done)
			close(entered)
			<-unblock
			return nil, os.ErrNotExist
		},
	}
	got, err := stalled.BoundedTokenPath(15*time.Millisecond, func(time.Duration) <-chan time.Time {
		c := make(chan time.Time)
		go func() {
			<-entered
			close(c)
		}()
		return c
	})
	if err != nil || got != token {
		t.Fatalf("BoundedTokenPath(stalled) = (%q, %v), want (%q, nil)", got, err, token)
	}
}

func TestSessionSuccessionDefaultsOffAndParsesIndependently(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "hub.token")
	if err := os.WriteFile(token, []byte("tok-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "client.json")
	e := EnvConfig{DefaultTokenFile: token, Getenv: func(string) string { return "" }}

	// Missing client.json defaults OFF.
	r := e.Load()
	if r.SuccessionEnabled() || e.SuccessionEnabled() {
		t.Fatal("missing client.json enabled session_succession")
	}

	// Omitted session_succession preserves byte-identical JSON without the key.
	if err := WriteConfig(cfgPath, FileConfig{Hub: "https://hub.example", TokenFile: "hub.token"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("session_succession")) {
		t.Fatalf("omitted session_succession wrote key: %s", raw)
	}

	// Invalid session_succession values fail OFF without invalidating HubURL or Token.
	for _, bad := range []string{`"true"`, `1`, `null`, `{}`, `[]`} {
		body := `{"hub_url":"https://hub.example","token_file":"hub.token","session_succession":` + bad + `}`
		if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		loaded := e.Load()
		if loaded.ConfigErr() != nil {
			t.Fatalf("bad session_succession (%s) poisoned ConfigErr: %v", bad, loaded.ConfigErr())
		}
		if loaded.HubURL() != "https://hub.example" {
			t.Fatalf("bad session_succession (%s) broke HubURL: %q", bad, loaded.HubURL())
		}
		if gotTok, terr := loaded.Token(); terr != nil || gotTok != "tok-secret" {
			t.Fatalf("bad session_succession (%s) broke Token: (%q, %v)", bad, gotTok, terr)
		}
		if loaded.SuccessionEnabled() || e.SuccessionEnabled() {
			t.Fatalf("bad session_succession (%s) enabled succession", bad)
		}
	}

	// Non-object JSON fails parseSessionSuccession OFF cleanly.
	if err := os.WriteFile(cfgPath, []byte(`["not-an-object"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if e.SuccessionEnabled() {
		t.Fatal("array JSON enabled succession")
	}

	// Dynamic reload on the same Resolved reflects true <-> false <-> omitted without process restart.
	if err := os.WriteFile(cfgPath, []byte(`{"hub_url":"https://hub.example","session_succession":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r = e.Load()
	if r.SuccessionEnabled() {
		t.Fatal("explicit false enabled succession")
	}
	if err := os.WriteFile(cfgPath, []byte(`{"hub_url":"https://hub.example","session_succession":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !r.SuccessionEnabled() {
		t.Fatal("dynamic reload missed session_succession: true")
	}
	if !e.SuccessionEnabled() {
		t.Fatal("EnvConfig.SuccessionEnabled missed session_succession: true")
	}
	if err := os.WriteFile(cfgPath, []byte(`{"hub_url":"https://hub.example"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r.SuccessionEnabled() {
		t.Fatal("dynamic reload stayed ON after key removal")
	}
}

func TestSessionSuccessionBoundedReloadRejectsStallsAndSpecialFiles(t *testing.T) {
	// Empty ConfigFile, nil context, cancelled context, and failed BaseTokenPath all fail OFF.
	if (Resolved{}).SuccessionEnabled() {
		t.Fatal("empty Resolved enabled succession")
	}
	//nolint:staticcheck // Deliberately testing nil context defensive fail-OFF handling.
	if (Resolved{ConfigFile: "/nonexistent"}).SuccessionEnabledContext(nil) {
		t.Fatal("nil context enabled succession")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if (Resolved{ConfigFile: "/nonexistent"}).SuccessionEnabledContext(cancelled) {
		t.Fatal("cancelled context enabled succession")
	}
	if runtime.GOOS != "windows" {
		emptyHomeEnv := EnvConfig{Getenv: func(string) string { return "" }}
		t.Setenv("HOME", "")
		t.Setenv("XDG_CONFIG_HOME", "")
		if emptyHomeEnv.SuccessionEnabled() {
			t.Fatal("empty BaseTokenPath enabled succession")
		}
	}

	dir := t.TempDir()
	token := filepath.Join(dir, "hub.token")
	cfgPath := filepath.Join(dir, "client.json")

	// Stalled ReadFile fails OFF within bounded deadline.
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	t.Cleanup(func() {
		close(release)
		<-done
	})
	stalledEnv := EnvConfig{
		DefaultTokenFile: token,
		Getenv:           func(string) string { return "" },
		ReadFile: func(string) ([]byte, error) {
			defer close(done)
			close(entered)
			<-release
			return []byte(`{"session_succession":true}`), nil
		},
	}
	ctx, cancelTimeout := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancelTimeout()
	go func() {
		<-entered
	}()
	if stalledEnv.SuccessionEnabledContext(ctx) {
		t.Fatal("stalled config enabled succession")
	}

	// Oversized custom ReadFile response is rejected OFF.
	oversizedEnv := EnvConfig{
		DefaultTokenFile: token,
		Getenv:           func(string) string { return "" },
		ReadFile: func(string) ([]byte, error) {
			return append([]byte(`{"session_succession":true,"pad":"`), bytes.Repeat([]byte("a"), maxConfigBytes)...), nil
		},
	}
	if oversizedEnv.SuccessionEnabled() {
		t.Fatal("oversized config enabled succession")
	}

	// Oversized regular file on disk and non-regular files (FIFO / symlink) fail OFF.
	big := append([]byte(`{"session_succession":true,"pad":"`), bytes.Repeat([]byte("a"), maxConfigBytes)...)
	if err := os.WriteFile(cfgPath, append(big, `"}`...), 0o600); err != nil {
		t.Fatal(err)
	}
	e := EnvConfig{DefaultTokenFile: token, Getenv: func(string) string { return "" }}
	if e.SuccessionEnabled() {
		t.Fatal("oversized disk file enabled succession")
	}

	if runtime.GOOS != "windows" {
		realCfg := filepath.Join(dir, "real.json")
		if err := os.WriteFile(realCfg, []byte(`{"session_succession":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(cfgPath)
		if err := os.Symlink(realCfg, cfgPath); err != nil {
			t.Fatal(err)
		}
		if e.SuccessionEnabled() {
			t.Fatal("symlink client.json enabled succession")
		}
		_ = os.Remove(cfgPath)
	}

	// Internal regular-file boundary failures in readBoundedRegularConfig.
	if err := os.WriteFile(cfgPath, []byte(`{"session_succession":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	origOpen := configOpen
	origAfter := afterConfigStat
	t.Cleanup(func() {
		configOpen = origOpen
		afterConfigStat = origAfter
	})
	configOpen = func(string) (*os.File, error) { return nil, os.ErrPermission }
	if e.SuccessionEnabled() {
		t.Fatal("configOpen error enabled succession")
	}
	configOpen = func(p string) (*os.File, error) {
		f, err := os.Open(p)
		if err == nil {
			_ = f.Close()
		}
		return f, err
	}
	if e.SuccessionEnabled() {
		t.Fatal("Stat error on closed fd enabled succession")
	}
	configOpen = func(string) (*os.File, error) { return os.Open(dir) }
	if e.SuccessionEnabled() {
		t.Fatal("directory handle from configOpen enabled succession")
	}
	configOpen = func(p string) (*os.File, error) {
		return os.OpenFile(p, os.O_WRONLY, 0)
	}
	if e.SuccessionEnabled() {
		t.Fatal("write-only fd enabled succession")
	}
	configOpen = origOpen
	afterConfigStat = func() {
		_ = os.WriteFile(cfgPath, append([]byte(`{"session_succession":true,"pad":"`), append(bytes.Repeat([]byte("a"), maxConfigBytes), `"}`...)...), 0o600)
	}
	if e.SuccessionEnabled() {
		t.Fatal("grown >64KiB file after Stat enabled succession")
	}
}

func TestUpdateConfigPreservesDisableOverStaleTrue(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "client.json")

	// Validation & privateDir errors.
	if err := UpdateConfig(cfgPath, FileConfig{Hub: "http://example.com"}); err == nil {
		t.Fatal("expected ValidateURL error")
	}
	fileParent := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(fileParent, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UpdateConfig(filepath.Join(fileParent, "client.json"), FileConfig{Hub: "https://hub.example"}); err == nil {
		t.Fatal("expected privateDir error")
	}

	// Preserve true when untouched on disk.
	if err := WriteConfig(cfgPath, FileConfig{Hub: "https://old.example", SessionSuccession: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	if err := UpdateConfig(cfgPath, FileConfig{Hub: "https://new.example", TokenFile: "hub.token"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var got FileConfig
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Hub != "https://new.example" || got.SessionSuccession == nil || !*got.SessionSuccession {
		t.Fatalf("UpdateConfig failed to preserve true: %+v (%s)", got, raw)
	}

	// Invalid on-disk value is dropped cleanly.
	if err := os.WriteFile(cfgPath, []byte(`{"hub_url":"https://old.example","session_succession":"bad"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UpdateConfig(cfgPath, FileConfig{Hub: "https://new.example"}); err != nil {
		t.Fatal(err)
	}
	if raw, err = os.ReadFile(cfgPath); err != nil || bytes.Contains(raw, []byte("session_succession")) {
		t.Fatalf("invalid value not dropped cleanly: %s (%v)", raw, err)
	}

	// Concurrent owner disable (true -> false and true -> omitted) during beforeConfigRename always beats stale true.
	for _, tc := range []struct {
		name         string
		ownerContent string
		wantNil      bool
	}{
		{name: "explicit false wins", ownerContent: `{"hub_url":"https://old.example","session_succession":false}`, wantNil: false},
		{name: "key removal wins", ownerContent: `{"hub_url":"https://old.example"}`, wantNil: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := WriteConfig(cfgPath, FileConfig{Hub: "https://old.example", SessionSuccession: boolPtr(true)}); err != nil {
				t.Fatal(err)
			}
			origBefore := beforeConfigRename
			t.Cleanup(func() { beforeConfigRename = origBefore })
			beforeConfigRename = func(p string) {
				if err := os.WriteFile(p, []byte(tc.ownerContent), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := UpdateConfig(cfgPath, FileConfig{Hub: "https://updated.example"}); err != nil {
				t.Fatal(err)
			}
			beforeConfigRename = origBefore
			var merged FileConfig
			data, _ := os.ReadFile(cfgPath)
			if err := json.Unmarshal(data, &merged); err != nil {
				t.Fatal(err)
			}
			if merged.Hub != "https://updated.example" {
				t.Fatalf("hub not updated: %+v", merged)
			}
			if tc.wantNil && merged.SessionSuccession != nil {
				t.Fatalf("expected nil session_succession, got %v", *merged.SessionSuccession)
			}
			if !tc.wantNil && (merged.SessionSuccession == nil || *merged.SessionSuccession) {
				t.Fatalf("expected explicit false, got %v", merged.SessionSuccession)
			}
		})
	}

	// Initial stage failure and refresh-stage failure after concurrent disable.
	origWrite := prepareConfigWrite
	origBefore := beforeConfigRename
	t.Cleanup(func() {
		prepareConfigWrite = origWrite
		beforeConfigRename = origBefore
	})
	prepareConfigWrite = func(f *os.File) { _ = f.Close() }
	if err := UpdateConfig(cfgPath, FileConfig{Hub: "https://fail.example"}); err == nil {
		t.Fatal("expected initial stage failure")
	}
	prepareConfigWrite = origWrite

	if err := WriteConfig(cfgPath, FileConfig{Hub: "https://old.example", SessionSuccession: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	writes := 0
	prepareConfigWrite = func(f *os.File) {
		writes++
		if writes == 2 {
			_ = f.Close()
		}
	}
	beforeConfigRename = func(p string) {
		_ = os.WriteFile(p, []byte(`{"hub_url":"https://old.example","session_succession":false}`), 0o600)
	}
	if err := UpdateConfig(cfgPath, FileConfig{Hub: "https://new.example"}); err == nil {
		t.Fatal("expected refresh stage error")
	}

	// Fresh config without existing file and rename failure cleanup.
	prepareConfigWrite = origWrite
	beforeConfigRename = origBefore
	freshPath := filepath.Join(dir, "fresh.json")
	if err := UpdateConfig(freshPath, FileConfig{Hub: "https://fresh.example"}); err != nil {
		t.Fatal(err)
	}
	dirTarget := filepath.Join(dir, "dir-target")
	if err := os.MkdirAll(filepath.Join(dirTarget, "keep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := UpdateConfig(dirTarget, FileConfig{Hub: "https://fresh.example"}); err == nil {
		t.Fatal("expected rename failure when target is a directory")
	}
}

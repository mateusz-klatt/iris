package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/integrate"
)

// hubStub answers by path. A nil response is a transport failure.
type hubStub map[string]func() (*http.Response, error)

func (h hubStub) RoundTrip(r *http.Request) (*http.Response, error) {
	answer, ok := h[r.URL.Path]
	if !ok {
		return status(http.StatusNoContent)()
	}
	return answer()
}

func status(code int) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		return &http.Response{StatusCode: code, Status: http.StatusText(code), Body: io.NopCloser(strings.NewReader(""))}, nil
	}
}

func refused() (*http.Response, error) { return nil, errors.New("connection reset") }

func emptyLayout(t *testing.T) integrate.Layout {
	t.Helper()
	home := t.TempDir()
	return integrate.Layout{
		Home:     home,
		StateDir: filepath.Join(home, "state"),
		Getenv:   func(string) string { return "" },
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
	}
}

func runAgainst(t *testing.T, hub hubStub) Result {
	t.Helper()
	layout := emptyLayout(t)
	token := filepath.Join(layout.Home, "hub.token")
	if err := os.WriteFile(token, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Options{Layout: layout, HubURL: "https://hub.example", TokenFile: token, HTTP: &http.Client{Transport: hub}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestFailedTokenProbeIsNotAnInvalidToken(t *testing.T) {
	for name, answer := range map[string]func() (*http.Response, error){
		"transport":   refused,
		"hub failure": status(http.StatusBadGateway),
	} {
		t.Run(name, func(t *testing.T) {
			res := runAgainst(t, hubStub{"/v1/agents": answer})
			text := res.Format()
			if !strings.Contains(text, "token: not checked") || strings.Contains(text, "not valid") || strings.Contains(text, "token: ok") {
				t.Fatal(text)
			}
			if !res.HubUp || res.TokenOK || res.Stored || res.ExitCode() != 1 {
				t.Fatalf("%+v", res)
			}
		})
	}
}

func TestRejectedTokenStaysInvalid(t *testing.T) {
	res := runAgainst(t, hubStub{"/v1/agents": status(http.StatusUnauthorized)})
	if text := res.Format(); !strings.Contains(text, "token: not valid") || !strings.Contains(text, "token was rejected") {
		t.Fatal(text)
	}
}

func TestUnusableHub(t *testing.T) {
	for _, c := range []struct {
		name   string
		hub    hubStub
		detail string
	}{
		{"unreachable", hubStub{"/v1/status": refused}, "hub unreachable"},
		{"failing", hubStub{"/v1/status": status(http.StatusServiceUnavailable)}, "hub status"},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := runAgainst(t, c.hub)
			text := res.Format()
			if res.HubUp || !strings.Contains(text, "hub: unreachable") || !strings.Contains(text, c.detail) || !strings.Contains(text, "token: not checked") {
				t.Fatal(text)
			}
		})
	}
}

func TestRefusedHubAddress(t *testing.T) {
	layout := emptyLayout(t)
	res, err := Run(context.Background(), Options{Layout: layout, HubURL: "http://hub.example", TokenFile: filepath.Join(layout.Home, "hub.token")})
	if err != nil {
		t.Fatal(err)
	}
	if res.HubUp || !strings.Contains(res.HubDetail, "refusing cleartext") {
		t.Fatalf("%+v", res)
	}
}

func TestReportThatCannotBeStoredIsShown(t *testing.T) {
	for name, answer := range map[string]func() (*http.Response, error){
		"transport": refused,
		"rejected":  status(http.StatusForbidden),
	} {
		t.Run(name, func(t *testing.T) {
			res := runAgainst(t, hubStub{"/v1/ui/doctor": answer})
			if res.Stored || !strings.Contains(res.Format(), "stored: no (") {
				t.Fatal(res.Format())
			}
			if res.ExitCode() != 0 {
				t.Fatal("storing the report is not part of the verdict")
			}
		})
	}
}

func TestHubRequestReportsARequestItCannotBuild(t *testing.T) {
	if _, err := hubRequest(context.Background(), "not a method", "https://hub.example", "", nil, http.DefaultClient); err == nil {
		t.Fatal("no error")
	}
}

func TestUnreadableTokenKeepsItsReason(t *testing.T) {
	layout := emptyLayout(t)
	dir := filepath.Join(layout.Home, "token-is-a-directory")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Options{Layout: layout, HubURL: "https://hub.example", TokenFile: dir, HTTP: &http.Client{Transport: hubStub{}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.TokenOK || res.TokenDetail == "" || strings.Contains(res.TokenDetail, "missing or empty") {
		t.Fatalf("%+v", res)
	}
}

func TestMissingTokenIsReportedOnTheTokenLine(t *testing.T) {
	layout := emptyLayout(t)
	file := filepath.Join(layout.Home, "absent")
	res, err := Run(context.Background(), Options{Layout: layout, HubURL: "https://hub.example", TokenFile: file, HTTP: &http.Client{Transport: hubStub{}}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Format()
	for _, want := range []string{"\nhub: ok\n", "\ntoken: not available (token file is missing or empty)\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("want %q in\n%s", want, text)
		}
	}
	if strings.Contains(text, "not valid") {
		t.Fatalf("the hub never saw a token:\n%s", text)
	}
	if res.ExitCode() != 1 {
		t.Fatalf("exit %d", res.ExitCode())
	}
}

func TestReadToken(t *testing.T) {
	dir := t.TempDir()
	blank := filepath.Join(dir, "blank")
	if err := os.WriteFile(blank, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"  ":                          "not set",
		filepath.Join(dir, "missing"): "missing or empty",
		blank:                         "missing or empty",
	} {
		if _, err := readToken(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q: %v", path, err)
		}
	}
}

func TestDefaultTokenFile(t *testing.T) {
	env := func(pairs map[string]string) func(string) string {
		return func(k string) string { return pairs[k] }
	}
	home := filepath.Join("home", "a")
	for _, c := range []struct {
		name string
		l    integrate.Layout
		want string
	}{
		{"explicit", integrate.Layout{GOOS: "linux", Home: home, Getenv: env(map[string]string{"IRIS_TOKEN_FILE": "t", "XDG_CONFIG_HOME": "x"})}, "t"},
		{"xdg", integrate.Layout{GOOS: "linux", Home: home, Getenv: env(map[string]string{"XDG_CONFIG_HOME": "x"})}, filepath.Join("x", "iris", "hub.token")},
		{"linux", integrate.Layout{GOOS: "linux", Home: home, Getenv: env(nil)}, filepath.Join(home, ".config", "iris", "hub.token")},
		{"darwin", integrate.Layout{GOOS: "darwin", Home: home, Getenv: env(nil)}, filepath.Join(home, "Library", "Application Support", "iris", "hub.token")},
		{"appdata", integrate.Layout{GOOS: "windows", Home: home, Getenv: env(map[string]string{"AppData": "r", "XDG_CONFIG_HOME": "x"})}, filepath.Join("r", "iris", "hub.token")},
		{"windows", integrate.Layout{GOOS: "windows", Home: home, Getenv: env(nil)}, filepath.Join(home, "AppData", "Roaming", "iris", "hub.token")},
	} {
		if got := defaultTokenFile(c.l); got != c.want {
			t.Errorf("%s: %s", c.name, got)
		}
	}
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("home", "")
	if got := defaultTokenFile(integrate.Layout{Getenv: env(nil)}); got != "" {
		t.Fatalf("no home: %s", got)
	}
}

func TestBrokenClientConfigurationStopsTheRun(t *testing.T) {
	layout := emptyLayout(t)
	token := filepath.Join(layout.Home, "hub.token")
	layout.Getenv = func(k string) string {
		if k == "IRIS_TOKEN_FILE" {
			return token
		}
		return ""
	}
	if err := os.WriteFile(filepath.Join(layout.Home, "client.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), Options{Layout: layout, HTTP: &http.Client{Transport: hubStub{}}}); err == nil {
		t.Fatal("ran with a configuration it could not read")
	}
}

func TestClockOutsideTheReportFormatIsShown(t *testing.T) {
	layout := emptyLayout(t)
	token := filepath.Join(layout.Home, "hub.token")
	if err := os.WriteFile(token, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	far := func() time.Time { return time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) }
	res, err := Run(context.Background(), Options{Layout: layout, HubURL: "https://hub.example", TokenFile: token, HTTP: &http.Client{Transport: hubStub{}}, Now: far})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stored || res.StoreError == "" {
		t.Fatalf("%+v", res)
	}
}

func TestUnreadableClientFileSilencesAClient(t *testing.T) {
	t.Setenv("IRIS_FAKE_STDOUT", "{}")
	t.Setenv("IRIS_FAKE_EXIT", "0")
	layout := grokLayout(t, true)
	grok, _ := integrate.ByName(integrate.Grok)
	files, err := grok.Files(layout)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files[0], []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if row := grokRow(t, Options{Layout: layout}); row.Status != StatusSilent || row.Detail == "" {
		t.Fatalf("%+v", row)
	}
}

func TestHookThatCannotRunSilencesAClient(t *testing.T) {
	t.Setenv("IRIS_FAKE_STDOUT", "{}")
	t.Setenv("IRIS_FAKE_EXIT", "0")
	layout := grokLayout(t, true)
	iris, err := layout.LookPath("iris")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(iris); err != nil {
		t.Fatal(err)
	}
	row := grokRow(t, Options{Layout: layout})
	if row.Status != StatusSilent || row.Detail == "" {
		t.Fatalf("%+v", row)
	}
}

func TestCanaryPayloadNamesTheDirectory(t *testing.T) {
	cwd := `C:\temp "x"`
	for _, client := range integrate.Names {
		var got map[string]any
		raw := canaryPayload(client, cwd)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("%s: %v", client, err)
		}
		dir, _ := got["cwd"].(string)
		if client == integrate.Antigravity {
			paths, _ := got["workspacePaths"].([]any)
			if len(paths) == 1 {
				dir, _ = paths[0].(string)
			}
		}
		if dir != cwd {
			t.Fatalf("%s: %s", client, raw)
		}
	}
}

func TestValidHook(t *testing.T) {
	for _, c := range []struct {
		client string
		stdout string
		exit   int
		ok     bool
	}{
		{integrate.Claude, "{}", 0, true},
		{integrate.Claude, "{}", 2, false},
		{integrate.Claude, "", 0, false},
		{integrate.Claude, "plain text", 0, false},
		{integrate.Kimi, "  ", 0, true},
		{integrate.Kimi, "plain text", 0, false},
	} {
		if err := validHook(c.client, []byte(c.stdout), c.exit); (err == nil) != c.ok {
			t.Errorf("%+v: %v", c, err)
		}
	}
}

func TestCommandVersionReadsTheFirstLine(t *testing.T) {
	bin, err := integrate.PlaceFake(t.TempDir(), "client")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("IRIS_FAKE_EXIT", "0")
	t.Setenv("IRIS_FAKE_STDOUT", "  1.2.3 \nbuilt today\n")
	if got, err := commandVersion(context.Background(), bin, time.Minute); got != "1.2.3" || err != nil {
		t.Fatalf("%q %v", got, err)
	}
	t.Setenv("IRIS_FAKE_STDOUT", "1.2.3")
	if got, err := commandVersion(context.Background(), bin, time.Minute); got != "1.2.3" || err != nil {
		t.Fatalf("%q %v", got, err)
	}
	t.Setenv("IRIS_FAKE_EXIT", "1")
	if _, err := commandVersion(context.Background(), bin, time.Minute); err == nil {
		t.Fatal("failing client gave a version")
	}
}

func TestFailureLogProblemsAreShown(t *testing.T) {
	layout := emptyLayout(t)
	token := filepath.Join(layout.Home, "hub.token")
	if err := os.Mkdir(filepath.Join(layout.Home, "hook-failures.log"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Options{Layout: layout, HubURL: "https://hub.example", TokenFile: token, HTTP: &http.Client{Transport: hubStub{}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.HubDetail, "failure log:") {
		t.Fatalf("%+v", res)
	}
}

func TestFailuresIgnoreUnknownClientsAndAMissingTokenPath(t *testing.T) {
	res := Result{Report: Report{Clients: []ClientReport{{Client: integrate.Claude, Status: StatusOK}}}}
	if err := applyFailures(&res, " ", time.Now()); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(t.TempDir(), "hub.token")
	line := `{"time":"2026-11-06T12:00:00Z","client":"unheard-of","event":"e","stage":"s","error":"x"}` + "\n"
	if err := os.WriteFile(filepath.Join(filepath.Dir(token), "hook-failures.log"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 6, 13, 0, 0, 0, time.UTC)
	if err := applyFailures(&res, token, now); err != nil {
		t.Fatal(err)
	}
	if res.Report.Clients[0].Status != StatusOK {
		t.Fatalf("%+v", res.Report.Clients)
	}
}

func TestLiveArgsRefuseAnUnknownClient(t *testing.T) {
	if _, _, err := LiveArgs("unheard-of", "bin"); err == nil {
		t.Fatal("no error")
	}
	layout := integrate.Layout{LookPath: func(string) (string, error) { return "bin", nil }}
	if err := runLive(context.Background(), Options{Layout: layout}, "unheard-of"); err == nil {
		t.Fatal("ran a client with no binary list")
	}
}

func TestRunLiveReportsAFailure(t *testing.T) {
	bin, err := integrate.PlaceFake(t.TempDir(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	opt := Options{Layout: integrate.Layout{LookPath: func(string) (string, error) { return bin, nil }}}
	t.Setenv("IRIS_FAKE_EXIT", "1")
	t.Setenv("IRIS_FAKE_STDOUT", "short reason")
	if err := runLive(context.Background(), opt, integrate.Codex); err == nil || !strings.Contains(err.Error(), "short reason") {
		t.Fatal(err)
	}
	t.Setenv("IRIS_FAKE_STDOUT", strings.Repeat("x", 500))
	err = runLive(context.Background(), opt, integrate.Codex)
	if err == nil || strings.Contains(err.Error(), strings.Repeat("x", 401)) || !strings.Contains(err.Error(), strings.Repeat("x", 400)) {
		t.Fatal(err)
	}
	missing := Options{Layout: integrate.Layout{LookPath: func(string) (string, error) { return "", errors.New("not found") }}}
	if err := runLive(context.Background(), missing, integrate.Codex); err == nil {
		t.Fatal("ran a missing client")
	}
}

// grokLayout has an installed hook and, when placed, a client binary.
func grokLayout(t *testing.T, withClient bool) integrate.Layout {
	t.Helper()
	layout := doctorLayout(t)
	iris, err := layout.LookPath("iris")
	if err != nil {
		t.Fatal(err)
	}
	var client string
	if withClient {
		client, err = integrate.PlaceFake(t.TempDir(), "grok")
		if err != nil {
			t.Fatal(err)
		}
		// Defender scans a new executable on its first start, which can outlast the
		// version check on a CI runner. The exit status does not matter here.
		_ = exec.Command(client).Run()
	}
	layout.LookPath = func(name string) (string, error) {
		switch {
		case name == "iris":
			return iris, nil
		case name == "grok" && withClient:
			return client, nil
		}
		return "", errors.New("not found")
	}
	grok, _ := integrate.ByName(integrate.Grok)
	if err := grok.Install(layout, nil); err != nil {
		t.Fatal(err)
	}
	return layout
}

func grokRow(t *testing.T, opt Options) ClientReport {
	t.Helper()
	opt.HubURL = "https://hub.example"
	opt.HTTP = &http.Client{Transport: hubStub{}}
	res, err := Run(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range res.Report.Clients {
		if row.Client == integrate.Grok {
			return row
		}
	}
	t.Fatal("no grok row")
	return ClientReport{}
}

func TestHealthyClientIsOK(t *testing.T) {
	t.Setenv("IRIS_FAKE_STDOUT", "{}")
	t.Setenv("IRIS_FAKE_EXIT", "0")
	row := grokRow(t, Options{Layout: grokLayout(t, true)})
	if row.Status != StatusOK || row.Detail != "" {
		t.Fatalf("%+v", row)
	}
}

func TestHookWithoutAClientNeedsAHuman(t *testing.T) {
	t.Setenv("IRIS_FAKE_STDOUT", "{}")
	t.Setenv("IRIS_FAKE_EXIT", "0")
	row := grokRow(t, Options{Layout: grokLayout(t, false)})
	if row.Status != StatusNeedsHuman || row.Detail != "client binary not found" {
		t.Fatalf("%+v", row)
	}
}

func TestSlowVersionIsATimeoutNotAMissingBinary(t *testing.T) {
	t.Setenv("IRIS_FAKE_STDOUT", "{}")
	t.Setenv("IRIS_FAKE_EXIT", "0")
	row := grokRow(t, Options{Layout: grokLayout(t, true), VersionTimeout: time.Nanosecond})
	if row.Status != StatusOK || row.Detail != "version check timed out" || row.Version != "" {
		t.Fatalf("%+v", row)
	}
}

func TestBrokenClientBinaryIsNotCalledMissing(t *testing.T) {
	t.Setenv("IRIS_FAKE_STDOUT", "{}")
	t.Setenv("IRIS_FAKE_EXIT", "0")
	// A file that was never started: Windows keeps an executable's image mapped for a moment
	// after it exits, so overwriting the warmed fake could fail.
	layout := grokLayout(t, false)
	broken := filepath.Join(t.TempDir(), "grok.exe")
	if err := os.WriteFile(broken, []byte("not a program"), 0o755); err != nil {
		t.Fatal(err)
	}
	look := layout.LookPath
	layout.LookPath = func(name string) (string, error) {
		if name == "grok" {
			return broken, nil
		}
		return look(name)
	}
	row := grokRow(t, Options{Layout: layout})
	if row.Status != StatusNeedsHuman || !strings.HasPrefix(row.Detail, "version check failed: ") {
		t.Fatalf("%+v", row)
	}
}

func TestLiveFailureSilencesAClient(t *testing.T) {
	t.Setenv("IRIS_FAKE_STDOUT", "{}")
	t.Setenv("IRIS_FAKE_EXIT", "0")
	failed := func(context.Context, string, []string) error { return errors.New("model refused") }
	row := grokRow(t, Options{Layout: grokLayout(t, true), Live: true, LiveRun: failed})
	if row.Status != StatusSilent || !strings.Contains(row.Detail, "live run failed: model refused") {
		t.Fatalf("%+v", row)
	}
}

func TestCanaryRunsWithoutATemporaryDirectory(t *testing.T) {
	t.Setenv("IRIS_FAKE_STDOUT", "{}")
	t.Setenv("IRIS_FAKE_EXIT", "0")
	layout := grokLayout(t, true)
	missing := filepath.Join(layout.Home, "no-such-dir")
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, missing)
	}
	if row := grokRow(t, Options{Layout: layout}); row.Status != StatusOK {
		t.Fatalf("%+v", row)
	}
}

func TestOldKimiIsNamedBeforeItsConfiguration(t *testing.T) {
	bin, err := integrate.PlaceFake(t.TempDir(), "kimi")
	if err != nil {
		t.Fatal(err)
	}
	layout := emptyLayout(t)
	layout.LookPath = func(name string) (string, error) {
		if name == "kimi" {
			return bin, nil
		}
		return "", errors.New("not found")
	}
	t.Setenv("IRIS_FAKE_STDOUT", "0.38.0")
	t.Setenv("IRIS_FAKE_EXIT", "0")
	res, err := Run(context.Background(), Options{Layout: layout, HubURL: "https://hub.example", HTTP: &http.Client{Transport: hubStub{}}, TokenFile: filepath.Join(layout.Home, "hub.token")})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range res.Report.Clients {
		if row.Client == integrate.Kimi && (row.Status != StatusNeedsHuman || !strings.Contains(row.Detail, "older than version 2")) {
			t.Fatalf("%+v", row)
		}
	}
}

package hook

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

func agyJSON(t *testing.T, v map[string]any) *bytes.Reader {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(raw)
}

func agyTestEnv(t *testing.T) (configDir, tokenPath string, getenv func(string) string) {
	t.Helper()
	configDir = t.TempDir()
	tokenPath = filepath.Join(configDir, "hub.token")
	if err := os.WriteFile(tokenPath, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	origPinned := pinnedAntigravityConfigDir
	pinnedAntigravityConfigDir = func() (string, error) { return configDir, nil }
	t.Cleanup(func() { pinnedAntigravityConfigDir = origPinned })
	return configDir, tokenPath, func(k string) string {
		if k == "IRIS_TOKEN_FILE" {
			return tokenPath
		}
		return ""
	}
}

func enableAgyWake(t *testing.T, configDir string) {
	t.Helper()
	if err := os.WriteFile(AntigravityWakePath(configDir), []byte("on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAntigravityWakeMarkerAndDecode(t *testing.T) {
	if AntigravityWakeEnabled("") {
		t.Fatal("empty configDir reported enabled")
	}
	dir := t.TempDir()
	if AntigravityWakeEnabled(dir) {
		t.Fatal("missing marker reported enabled")
	}
	if err := os.WriteFile(AntigravityWakePath(dir), []byte("off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if AntigravityWakeEnabled(dir) {
		t.Fatal("off marker reported enabled")
	}
	if err := os.WriteFile(AntigravityWakePath(dir), []byte("  on \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !AntigravityWakeEnabled(dir) {
		t.Fatal("on marker reported disabled")
	}
	if err := os.WriteFile(AntigravityWakePath(dir), []byte("  default \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !AntigravityWakeEnabled(dir) {
		t.Fatal("default marker reported disabled")
	}
	if err := os.WriteFile(AntigravityWakePath(dir), []byte("custom\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if AntigravityWakeEnabled(dir) {
		t.Fatal("unrecognized marker reported enabled")
	}

	// Missing executionNum decodes as -1 so Stop never treats it as 0.
	ev, err := Decode(ClientAntigravity, "Stop", []byte(`{"conversationId":"conv-1","fullyIdle":true,"terminationReason":"NO_TOOL_CALL","continuationActive":true}`))
	if err != nil || ev.AgyStop == nil || ev.AgyStop.ExecutionNum != -1 || !ev.AgyStop.ContinuationActive || !ev.AgyStop.FullyIdle {
		t.Fatalf("Decode missing executionNum = %+v, %+v, %v", ev, ev.AgyStop, err)
	}
}

func TestAntigravityQuotingAndCommandMatching(t *testing.T) {
	for _, bad := range []string{"", "/bin/iris\nbad", "/bin/iris\rbad", "/bin/iris\x00bad", "/bin/iris\x7fbad", `/bin/"iris"`} {
		if got, ok := quoteAntigravityIrisPath(bad, "linux"); ok || got != "" {
			t.Fatalf("unsafe path %q accepted: %q", bad, got)
		}
	}
	if got, ok := quoteAntigravityIrisPath("/usr/local/bin/iris", "linux"); !ok || got != "/usr/local/bin/iris" {
		t.Fatalf("plain unix = %q, %v", got, ok)
	}
	if got, ok := quoteAntigravityIrisPath(`C:\Iris\iris.exe`, "windows"); !ok || got != `C:\Iris\iris.exe` {
		t.Fatalf("plain windows = %q, %v", got, ok)
	}
	shQuoted, ok := quoteAntigravityIrisPath("/opt/o'hara tools/iris", "linux")
	if !ok || shQuoted != `'/opt/o'\''hara tools/iris'` {
		t.Fatalf("sh quote = %q, %v", shQuoted, ok)
	}
	cmdQuoted, ok := quoteAntigravityIrisPath(`C:\Program Files\Iris\iris.exe`, "windows")
	if !ok || cmdQuoted != `"C:\Program Files\Iris\iris.exe"` {
		t.Fatalf("cmd quote = %q, %v", cmdQuoted, ok)
	}

	if !isAntigravityWakeCommand(shQuoted+" watch --client antigravity --wake", "/opt/o'hara tools/iris") {
		t.Fatalf("failed to match sh-quoted command: %s", shQuoted)
	}
	if !isAntigravityWakeCommand(cmdQuoted+" \t watch   --client antigravity --wake ", `C:\Program Files\Iris\iris.exe`) {
		t.Fatalf("failed to match cmd-quoted command: %s", cmdQuoted)
	}
	if !isAntigravityWakeCommand("iris watch --client antigravity --wake", "/custom/bin/iris-dev") {
		t.Fatal("failed to match bare iris binary")
	}
	if !isAntigravityWakeCommand("/custom/bin/iris-dev watch --client antigravity --wake", "/custom/bin/iris-dev") {
		t.Fatal("failed to match exact custom irisPath")
	}
	for _, badCmd := range []string{
		"",
		"iris watch --client antigravity",
		"iris watch --client antigravity --wake --extra",
		"iris watch --client grok --wake",
		"echo watch --client antigravity --wake",
		"'unclosed watch --client antigravity --wake",
		`"unclosed watch --client antigravity --wake`,
		"iris watch --client antigravity --wake && echo pwn",
		"iris watch --client antigravity --wake\necho pwn",
		"$IRIS watch --client antigravity --wake",
	} {
		if isAntigravityWakeCommand(badCmd, "/usr/local/bin/iris") {
			t.Fatalf("accepted bad wake command %q", badCmd)
		}
	}
}

func TestAntigravityStopGatesAndFailOpen(t *testing.T) {
	configDir, _, getenv := agyTestEnv(t)
	anc := func() (proc.Ancestor, error) {
		return proc.Ancestor{PID: 4001, StartMS: 9001, Name: "agy"}, nil
	}
	transcript := filepath.Join(t.TempDir(), "transcript_full.jsonl")
	if err := os.WriteFile(transcript, []byte("{\"step_index\":0}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	baseStop := func(extra map[string]any) *bytes.Reader {
		m := map[string]any{
			"conversationId":     "conv-gates",
			"fullyIdle":          true,
			"terminationReason":  "NO_TOOL_CALL",
			"executionNum":       0,
			"continuationActive": false,
			"transcriptPath":     transcript,
		}
		for k, v := range extra {
			m[k] = v
		}
		return agyJSON(t, m)
	}

	// 1. Marker off -> allow {}, records idle=true with file_id and idle_offset.
	r := Runner{
		ClientName: "antigravity",
		EventName:  "Stop",
		Stdin:      baseStop(nil),
		Getenv:     getenv,
		Ancestor:   anc,
		IrisPath:   func() string { return "/usr/local/bin/iris" },
	}
	if out := r.Run(); string(out.Stdout) != "{}\n" || out.Exit != 0 {
		t.Fatalf("marker off = %+v", out)
	}
	cs, ok := ReadAntigravityConvState(configDir, "conv-gates")
	if !ok || !cs.Idle || cs.FileID == "" || cs.IdleOffset != 17 {
		t.Fatalf("conv state = %+v, %v", cs, ok)
	}

	// Enable wake marker for remaining gate tests.
	enableAgyWake(t, configDir)

	// 2. Canary session is ignored completely.
	out := (Runner{
		ClientName: "antigravity",
		EventName:  "Stop",
		Stdin:      baseStop(map[string]any{"conversationId": CanarySession}),
		Getenv:     getenv,
		Ancestor:   anc,
	}).Run()
	if string(out.Stdout) != "{}\n" {
		t.Fatalf("canary = %q", out.Stdout)
	}

	// 3. Nil Getenv and broken TokenPath fail open.
	if out := (Runner{ClientName: "antigravity", EventName: "Stop", Stdin: baseStop(nil), Ancestor: anc}).Run(); string(out.Stdout) != "{}\n" {
		t.Fatalf("nil getenv = %q", out.Stdout)
	}
	origHome, origXDG, origAppData := os.Getenv("HOME"), os.Getenv("XDG_CONFIG_HOME"), os.Getenv("APPDATA")
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("APPDATA", "")
	if out := (Runner{
		ClientName: "antigravity",
		EventName:  "Stop",
		Stdin:      baseStop(nil),
		Getenv:     func(string) string { return "" },
		Ancestor:   anc,
	}).Run(); string(out.Stdout) != "{}\n" {
		t.Fatalf("missing token path = %q", out.Stdout)
	}
	t.Setenv("HOME", origHome)
	t.Setenv("XDG_CONFIG_HOME", origXDG)
	t.Setenv("APPDATA", origAppData)

	// 4. Non-agy ancestor, fullyIdle=false, wrong terminationReason, executionNum>0, continuationActive=true, unsafe IrisPath.
	if out := (Runner{
		ClientName: "antigravity",
		EventName:  "Stop",
		Stdin:      baseStop(nil),
		Getenv:     getenv,
		Ancestor:   func() (proc.Ancestor, error) { return proc.Ancestor{}, proc.ErrNotFound },
	}).Run(); string(out.Stdout) != "{}\n" {
		t.Fatalf("no ancestor = %q", out.Stdout)
	}
	if out := (Runner{
		ClientName: "antigravity",
		EventName:  "Stop",
		Stdin:      baseStop(map[string]any{"fullyIdle": false}),
		Getenv:     getenv,
		Ancestor:   anc,
	}).Run(); string(out.Stdout) != "{}\n" {
		t.Fatalf("not fullyIdle = %q", out.Stdout)
	}
	if cs, _ := ReadAntigravityConvState(configDir, "conv-gates"); cs.Idle {
		t.Fatal("fullyIdle=false left Idle=true")
	}
	if out := (Runner{
		ClientName: "antigravity",
		EventName:  "Stop",
		Stdin:      baseStop(map[string]any{"terminationReason": "USER_CANCELLED"}),
		Getenv:     getenv,
		Ancestor:   anc,
	}).Run(); string(out.Stdout) != "{}\n" {
		t.Fatalf("cancelled = %q", out.Stdout)
	}
	if out := (Runner{
		ClientName: "antigravity",
		EventName:  "Stop",
		Stdin:      baseStop(map[string]any{"executionNum": 1}),
		Getenv:     getenv,
		Ancestor:   anc,
	}).Run(); string(out.Stdout) != "{}\n" {
		t.Fatalf("executionNum=1 = %q", out.Stdout)
	}
	if out := (Runner{
		ClientName: "antigravity",
		EventName:  "Stop",
		Stdin:      baseStop(map[string]any{"continuationActive": true}),
		Getenv:     getenv,
		Ancestor:   anc,
	}).Run(); string(out.Stdout) != "{}\n" {
		t.Fatalf("continuationActive=true = %q", out.Stdout)
	}
	if out := (Runner{
		ClientName: "antigravity",
		EventName:  "Stop",
		Stdin:      baseStop(nil),
		Getenv:     getenv,
		Ancestor:   anc,
		IrisPath:   func() string { return "/bad/\"iris\"" },
	}).Run(); string(out.Stdout) != "{}\n" {
		t.Fatalf("unsafe IrisPath = %q", out.Stdout)
	}

	// Default IrisPath via irisExecutablePath error vs success.
	origExe := irisExecutablePath
	t.Cleanup(func() { irisExecutablePath = origExe })
	irisExecutablePath = func() (string, error) { return "", errors.New("no exe") }
	if out := (Runner{ClientName: "antigravity", EventName: "Stop", Stdin: baseStop(nil), Getenv: getenv, Ancestor: anc}).Run(); string(out.Stdout) != "{}\n" {
		t.Fatalf("exe err = %q", out.Stdout)
	}
	irisExecutablePath = origExe

	// Directory protection failure, lock contention, and write failure all fail open without looping.
	origProtect := protectPath
	protectPath = func(string) error { return errors.New("acl err") }
	if out := (Runner{ClientName: "antigravity", EventName: "Stop", Stdin: baseStop(nil), Getenv: getenv, Ancestor: anc}).Run(); string(out.Stdout) != "{}\n" {
		protectPath = origProtect
		t.Fatalf("protect err = %q", out.Stdout)
	}
	protectPath = origProtect

	hm := antigravityHelpersMemory(configDir)
	unlock, err := hm.lock(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	out = (Runner{ClientName: "antigravity", EventName: "Stop", Stdin: baseStop(nil), Getenv: getenv, Ancestor: anc}).Run()
	unlock()
	if string(out.Stdout) != "{}\n" {
		t.Fatalf("locked = %q", out.Stdout)
	}

	origCreate := createAntigravityTemp
	createAntigravityTemp = func(string, string) (*os.File, error) { return nil, errors.New("disk full") }
	out = (Runner{ClientName: "antigravity", EventName: "Stop", Stdin: baseStop(nil), Getenv: getenv, Ancestor: anc}).Run()
	createAntigravityTemp = origCreate
	if string(out.Stdout) != "{}\n" {
		t.Fatalf("write fail = %q", out.Stdout)
	}

	// Stop refuses to ask when the hook's configuration directory differs from the pinned OS-account
	// directory (both via default hubclient.PinnedConfigDir and via explicit PinnedConfigDir), or when
	// PinnedConfigDir fails, while accepting case-insensitive equality on Windows.
	origPinned := pinnedAntigravityConfigDir
	pinnedAntigravityConfigDir = hubclient.PinnedConfigDir
	if out := (Runner{ClientName: "antigravity", EventName: "Stop", Stdin: baseStop(nil), Getenv: getenv, Ancestor: anc}).Run(); string(out.Stdout) != "{}\n" {
		pinnedAntigravityConfigDir = origPinned
		t.Fatalf("mismatched default pinned dir = %q", out.Stdout)
	}
	pinnedAntigravityConfigDir = origPinned

	if out := (Runner{
		ClientName:      "antigravity",
		EventName:       "Stop",
		Stdin:           baseStop(nil),
		Getenv:          getenv,
		Ancestor:        anc,
		PinnedConfigDir: func() (string, error) { return "", errors.New("pinned lookup failed") },
	}).Run(); string(out.Stdout) != "{}\n" {
		t.Fatalf("pinned dir error = %q", out.Stdout)
	}
	if out := (Runner{
		ClientName:      "antigravity",
		EventName:       "Stop",
		Stdin:           baseStop(nil),
		Getenv:          getenv,
		Ancestor:        anc,
		PinnedConfigDir: func() (string, error) { return t.TempDir(), nil },
	}).Run(); string(out.Stdout) != "{}\n" {
		t.Fatalf("mismatched explicit pinned dir = %q", out.Stdout)
	}
	winOut := (Runner{
		ClientName:      "antigravity",
		EventName:       "Stop",
		Stdin:           baseStop(nil),
		Getenv:          getenv,
		Ancestor:        anc,
		GOOS:            "windows",
		IrisPath:        func() string { return `C:\Iris\iris.exe` },
		PinnedConfigDir: func() (string, error) { return strings.ToUpper(configDir), nil },
	}).Run()
	if !strings.Contains(string(winOut.Stdout), `"decision":"continue"`) {
		t.Fatalf("windows case-insensitive pinned dir did not ask: %q", winOut.Stdout)
	}
}

func TestAntigravityArmingLifecycleSupersededSessionsAndRearm(t *testing.T) {
	configDir, _, getenv := agyTestEnv(t)
	enableAgyWake(t, configDir)

	const agyPID, agyStart = 5500, int64(77000)
	anc := func() (proc.Ancestor, error) {
		return proc.Ancestor{PID: agyPID, StartMS: agyStart, Name: "agy"}, nil
	}

	liveProcs := map[int]int64{agyPID: agyStart}
	var terminated []int
	origLookup := lookupAntigravityProc
	origTerm := terminateAntigravityProc
	t.Cleanup(func() {
		lookupAntigravityProc = origLookup
		terminateAntigravityProc = origTerm
	})
	lookupAntigravityProc = func(pid int) (proc.Snapshot, bool) {
		start, ok := liveProcs[pid]
		if !ok {
			return proc.Snapshot{}, false
		}
		return proc.Snapshot{PID: pid, StartMS: start, Name: "iris"}, true
	}
	terminateAntigravityProc = func(pid int, startMS int64) error {
		if s, ok := liveProcs[pid]; !ok || s != startMS {
			return proc.ErrNotFound
		}
		delete(liveProcs, pid)
		terminated = append(terminated, pid)
		return nil
	}

	// SessionStart activates conv-1 and records transcript_path.
	(Runner{
		ClientName: "antigravity",
		EventName:  "SessionStart",
		Stdin:      agyJSON(t, map[string]any{"conversationId": "conv-1", "transcriptPath": "/tmp/t1.jsonl"}),
		Getenv:     getenv,
		Ancestor:   anc,
	}).Run()
	if cur, live := ReadAntigravitySession(configDir, agyPID, agyStart, nil); cur != "conv-1" || live {
		t.Fatalf("after SessionStart: cur=%q live=%v", cur, live)
	}

	// Stop on executionNum=0 asks once with Windows cmd quoting and keeps idle=false.
	stopOut := (Runner{
		ClientName: "antigravity",
		EventName:  "Stop",
		Stdin: agyJSON(t, map[string]any{
			"conversationId":     "conv-1",
			"fullyIdle":          true,
			"terminationReason":  "NO_TOOL_CALL",
			"executionNum":       0,
			"continuationActive": false,
		}),
		Getenv:   getenv,
		Ancestor: anc,
		GOOS:     "windows",
		IrisPath: func() string { return `C:\Program Files\Iris\iris.exe` },
	}).Run()
	if !strings.Contains(string(stopOut.Stdout), `"decision":"continue"`) || !strings.Contains(string(stopOut.Stdout), `\"C:\\Program Files\\Iris\\iris.exe\" watch --client antigravity --wake`) {
		t.Fatalf("Stop continue = %s", stopOut.Stdout)
	}
	if cs, _ := ReadAntigravityConvState(configDir, "conv-1"); cs.Idle {
		t.Fatal("continue Stop must leave Idle=false")
	}

	// Model ignores request without calling run_command -> next Stop latches failed and allows {}.
	stopOut2 := (Runner{
		ClientName: "antigravity",
		EventName:  "Stop",
		Stdin: agyJSON(t, map[string]any{
			"conversationId":     "conv-1",
			"fullyIdle":          true,
			"terminationReason":  "NO_TOOL_CALL",
			"executionNum":       1,
			"continuationActive": false,
		}),
		Getenv:   getenv,
		Ancestor: anc,
		IrisPath: func() string { return "/usr/local/bin/iris" },
	}).Run()
	if string(stopOut2.Stdout) != "{}\n" {
		t.Fatalf("ignored ask Stop = %s", stopOut2.Stdout)
	}
	// Turn 2 Stop sees failed latch and never asks again.
	stopOut3 := (Runner{
		ClientName: "antigravity",
		EventName:  "Stop",
		Stdin: agyJSON(t, map[string]any{
			"conversationId":     "conv-1",
			"fullyIdle":          true,
			"terminationReason":  "NO_TOOL_CALL",
			"executionNum":       0,
			"continuationActive": false,
		}),
		Getenv:   getenv,
		Ancestor: anc,
		IrisPath: func() string { return "/usr/local/bin/iris" },
	}).Run()
	if string(stopOut3.Stdout) != "{}\n" {
		t.Fatalf("failed latch Stop = %s", stopOut3.Stdout)
	}

	// Manual re-arm via PublishAntigravityHelper replaces failed with live helper.
	liveProcs[6101] = 88001
	owner1 := AntigravityHelperOwner{
		AgyPID: agyPID, AgyStartMS: agyStart,
		HelperPID: 6101, HelperStartMS: 88001,
		SessionID: "conv-1", AttemptID: "att-1",
	}
	already, err := PublishAntigravityHelper(configDir, owner1, nil)
	if err != nil || already {
		t.Fatalf("PublishAntigravityHelper = %v, %v", already, err)
	}
	// Duplicate publish preserves live helper without overwriting.
	already, err = PublishAntigravityHelper(configDir, AntigravityHelperOwner{
		AgyPID: agyPID, AgyStartMS: agyStart,
		HelperPID: 6102, HelperStartMS: 88002,
		SessionID: "conv-1", AttemptID: "att-2",
	}, nil)
	if err != nil || !already || !VerifyAntigravityHelper(configDir, owner1, nil) {
		t.Fatalf("duplicate publish clobbered owner1: already=%v err=%v", already, err)
	}

	// Duplicate failing command in PostToolUse and FailAntigravityHelper preserve live owner1!
	(Runner{
		ClientName: "antigravity",
		EventName:  "PostToolUse",
		Stdin: agyJSON(t, map[string]any{
			"conversationId": "conv-1",
			"error":          "permission denied",
			"toolCall": map[string]any{
				"name": "run_command",
				"args": map[string]any{"CommandLine": "/usr/local/bin/iris watch --client antigravity --wake"},
			},
		}),
		Getenv:   getenv,
		Ancestor: anc,
		IrisPath: func() string { return "/usr/local/bin/iris" },
	}).Run()
	if err := FailAntigravityHelper(configDir, agyPID, agyStart, "conv-1", nil); err != nil {
		t.Fatal(err)
	}
	if !VerifyAntigravityHelper(configDir, owner1, nil) {
		t.Fatal("live helper was clobbered by duplicate failure")
	}

	// Stop when live helper exists allows {} without asking.
	stopLive := (Runner{
		ClientName: "antigravity",
		EventName:  "Stop",
		Stdin: agyJSON(t, map[string]any{
			"conversationId":     "conv-1",
			"fullyIdle":          true,
			"terminationReason":  "NO_TOOL_CALL",
			"executionNum":       0,
			"continuationActive": false,
		}),
		Getenv:   getenv,
		Ancestor: anc,
		IrisPath: func() string { return "/usr/local/bin/iris" },
	}).Run()
	if string(stopLive.Stdout) != "{}\n" {
		t.Fatalf("Stop with live helper = %s", stopLive.Stdout)
	}

	// PreInvocation clears Idle; PostInvocation updates transcript_path.
	(Runner{
		ClientName: "antigravity",
		EventName:  "PreInvocation",
		Stdin:      agyJSON(t, map[string]any{"conversationId": "conv-1", "transcriptPath": "/tmp/t1b.jsonl"}),
		Getenv:     getenv,
		Ancestor:   anc,
	}).Run()
	if cs, _ := ReadAntigravityConvState(configDir, "conv-1"); cs.Idle || cs.TranscriptPath != "/tmp/t1b.jsonl" {
		t.Fatalf("after PreInvocation = %+v", cs)
	}
	(Runner{
		ClientName: "antigravity",
		EventName:  "PostInvocation",
		Stdin:      agyJSON(t, map[string]any{"conversationId": "conv-1", "transcriptPath": "/tmp/t1c.jsonl"}),
		Getenv:     getenv,
		Ancestor:   anc,
	}).Run()
	if cs, _ := ReadAntigravityConvState(configDir, "conv-1"); cs.TranscriptPath != "/tmp/t1c.jsonl" {
		t.Fatalf("after PostInvocation = %+v", cs)
	}

	// /clear starts conv-2 via SessionStart: conv-1 enters superseded_session_ids, owner1 fails Verify!
	(Runner{
		ClientName: "antigravity",
		EventName:  "SessionStart",
		Stdin:      agyJSON(t, map[string]any{"conversationId": "conv-2"}),
		Getenv:     getenv,
		Ancestor:   anc,
	}).Run()
	if VerifyAntigravityHelper(configDir, owner1, nil) {
		t.Fatal("superseded helper still verified after /clear")
	}
	// FailAntigravityHelper on superseded conv-1 is ignored.
	if err := FailAntigravityHelper(configDir, agyPID, agyStart, "conv-1", nil); err != nil {
		t.Fatal(err)
	}

	// Stale trailing Stop from retired conv-1 is ignored completely!
	staleOut := (Runner{
		ClientName: "antigravity",
		EventName:  "Stop",
		Stdin: agyJSON(t, map[string]any{
			"conversationId":     "conv-1",
			"fullyIdle":          true,
			"terminationReason":  "NO_TOOL_CALL",
			"executionNum":       0,
			"continuationActive": false,
		}),
		Getenv:   getenv,
		Ancestor: anc,
		IrisPath: func() string { return "/usr/local/bin/iris" },
	}).Run()
	if string(staleOut.Stdout) != "{}\n" {
		t.Fatalf("stale conv-1 Stop = %s", staleOut.Stdout)
	}
	if cur, _ := ReadAntigravitySession(configDir, agyPID, agyStart, nil); cur != "conv-2" {
		t.Fatalf("stale conv-1 overwrote current_session_id = %q", cur)
	}

	// conv-2 Stop terminates superseded live helper 6101 and asks once for conv-2!
	conv2Stop := (Runner{
		ClientName: "antigravity",
		EventName:  "Stop",
		Stdin: agyJSON(t, map[string]any{
			"conversationId":     "conv-2",
			"fullyIdle":          true,
			"terminationReason":  "NO_TOOL_CALL",
			"executionNum":       0,
			"continuationActive": false,
		}),
		Getenv:   getenv,
		GOOS:     "linux",
		Ancestor: anc,
		IrisPath: func() string { return "/opt/my iris/iris" },
	}).Run()
	if !strings.Contains(string(conv2Stop.Stdout), `'/opt/my iris/iris' watch --client antigravity --wake`) || len(terminated) != 1 || terminated[0] != 6101 {
		t.Fatalf("conv-2 Stop = %s, terminated=%v", conv2Stop.Stdout, terminated)
	}

	// Auto-denial in PostToolUse with empty "error": "" and no live helper latches failed!
	(Runner{
		ClientName: "antigravity",
		EventName:  "PostToolUse",
		Stdin: agyJSON(t, map[string]any{
			"conversationId": "conv-2",
			"error":          "",
			"toolCall": map[string]any{
				"name": "run_command",
				"args": map[string]any{"CommandLine": "'/opt/my iris/iris' watch --client antigravity --wake"},
			},
		}),
		Getenv:   getenv,
		GOOS:     "linux",
		Ancestor: anc,
		IrisPath: func() string { return "/opt/my iris/iris" },
	}).Run()
	// Next Stop for conv-2 sees failed and allows {}.
	if out := (Runner{
		ClientName: "antigravity",
		EventName:  "Stop",
		Stdin: agyJSON(t, map[string]any{
			"conversationId":     "conv-2",
			"fullyIdle":          true,
			"terminationReason":  "NO_TOOL_CALL",
			"executionNum":       0,
			"continuationActive": false,
		}),
		Getenv:   getenv,
		GOOS:     "linux",
		Ancestor: anc,
		IrisPath: func() string { return "/opt/my iris/iris" },
	}).Run(); string(out.Stdout) != "{}\n" {
		t.Fatalf("after PostToolUse denial Stop = %s", out.Stdout)
	}

	// Resuming conv-1 via SessionStart removes conv-1 from superseded_session_ids, and unseen conv-3
	// on PreInvocation advances current_session_id when SessionStart was skipped.
	(Runner{
		ClientName: "antigravity",
		EventName:  "SessionStart",
		Stdin:      agyJSON(t, map[string]any{"conversationId": "conv-1"}),
		Getenv:     getenv,
		Ancestor:   anc,
	}).Run()
	if cur, _ := ReadAntigravitySession(configDir, agyPID, agyStart, nil); cur != "conv-1" {
		t.Fatalf("resumed conv-1 = %q", cur)
	}
	(Runner{
		ClientName: "antigravity",
		EventName:  "PreInvocation",
		Stdin:      agyJSON(t, map[string]any{"conversationId": "conv-3"}),
		Getenv:     getenv,
		Ancestor:   anc,
	}).Run()
	if cur, _ := ReadAntigravitySession(configDir, agyPID, agyStart, nil); cur != "conv-3" {
		t.Fatalf("unseen conv-3 = %q", cur)
	}
}

func TestAntigravityHelperStoreEdgesCleanupAndTeardown(t *testing.T) {
	// Ring helpers.
	var ring []string
	ring = pushSupersededSession(ring, "")
	for i := range 20 {
		ring = pushSupersededSession(ring, "s-"+string(rune('a'+i)))
	}
	ring = pushSupersededSession(ring, "s-t")
	if len(ring) != maxSupersededSessions {
		t.Fatalf("ring len = %d", len(ring))
	}
	if removeSession(nil, "x") != nil || removeSession([]string{"only"}, "only") != nil {
		t.Fatal("removeSession empty cases")
	}
	if got := removeSession([]string{"a", "b"}, "a"); len(got) != 1 || got[0] != "b" {
		t.Fatalf("removeSession = %v", got)
	}

	// Guard checks on invalid args / missing dirs / corrupt files.
	if cur, live := ReadAntigravitySession("", 1, 1, nil); cur != "" || live {
		t.Fatal("ReadAntigravitySession empty dir")
	}
	if _, ok := ReadAntigravityConvState("", "conv-1"); ok {
		t.Fatal("ReadAntigravityConvState empty dir")
	}
	if _, err := PublishAntigravityHelper("", AntigravityHelperOwner{}, nil); err == nil {
		t.Fatal("PublishAntigravityHelper invalid owner")
	}
	if err := FailAntigravityHelper("", 0, 0, "", nil); err == nil {
		t.Fatal("FailAntigravityHelper invalid args")
	}
	if VerifyAntigravityHelper("", AntigravityHelperOwner{}, nil) {
		t.Fatal("VerifyAntigravityHelper disabled")
	}
	if err := CleanupAntigravityHelper("", AntigravityHelperOwner{}); err != nil {
		t.Fatal(err)
	}
	if err := StopAntigravityWakeHelpers(""); err != nil {
		t.Fatal(err)
	}

	configDir := t.TempDir()
	enableAgyWake(t, configDir)
	if err := StopAntigravityWakeHelpers(configDir); err != nil {
		t.Fatal("missing helper dir should be nil:", err)
	}
	if err := CleanupAntigravityHelper(configDir, AntigravityHelperOwner{AgyPID: 1, AgyStartMS: 1}); err != nil {
		t.Fatal("missing dir cleanup:", err)
	}
	if cur, live := ReadAntigravitySession(configDir, 99, 99, func(int) (proc.Snapshot, bool) { return proc.Snapshot{}, false }); cur != "" || live {
		t.Fatal("missing proc file")
	}

	hm := antigravityHelpersMemory(configDir)
	if err := hm.directory(); err != nil {
		t.Fatal(err)
	}
	// Corrupt proc and conv files degrade cleanly.
	if err := os.WriteFile(antigravityProcFile(hm.dir, 10, 20), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(antigravityConvFile(hm.dir, "bad-conv"), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadAntigravityConvState(configDir, "bad-conv"); ok {
		t.Fatal("corrupt conv file accepted")
	}
	if st := readAntigravityProcState(hm.dir, 10, 20); st.CurrentSessionID != "" {
		t.Fatal("corrupt proc file accepted")
	}
	if VerifyAntigravityHelper(configDir, AntigravityHelperOwner{AgyPID: 10, AgyStartMS: 20}, func(int) (proc.Snapshot, bool) {
		return proc.Snapshot{}, false
	}) {
		t.Fatal("dead agy verified")
	}

	// Publish fails when session is not active on this process, and terminates superseded live helper when it is.
	live := map[int]int64{10: 20, 700: 7000, 701: 7001}
	lookup := func(pid int) (proc.Snapshot, bool) {
		s, ok := live[pid]
		return proc.Snapshot{PID: pid, StartMS: s}, ok
	}
	origTerm := terminateAntigravityProc
	t.Cleanup(func() { terminateAntigravityProc = origTerm })
	var killed []int
	terminateAntigravityProc = func(pid int, start int64) error {
		killed = append(killed, pid)
		delete(live, pid)
		return nil
	}

	if _, err := PublishAntigravityHelper(configDir, AntigravityHelperOwner{
		AgyPID: 10, AgyStartMS: 20, HelperPID: 701, HelperStartMS: 7001, SessionID: "s-active", AttemptID: "a1",
	}, lookup); err == nil {
		t.Fatal("expected inactive session error")
	}

	if err := writeAntigravityJSON(hm.dir, antigravityProcFile(hm.dir, 10, 20), antigravityProcState{
		CurrentSessionID: "s-active",
		Helper:           &antigravityHelperRecord{Status: "live", PID: 700, StartMS: 7000, SessionID: "s-old", AttemptID: "a0"},
	}); err != nil {
		t.Fatal(err)
	}
	owner := AntigravityHelperOwner{AgyPID: 10, AgyStartMS: 20, HelperPID: 701, HelperStartMS: 7001, SessionID: "s-active", AttemptID: "a1"}
	if already, err := PublishAntigravityHelper(configDir, owner, lookup); err != nil || already || len(killed) != 1 || killed[0] != 700 {
		t.Fatalf("Publish superseding old helper = %v, %v, killed=%v", already, err, killed)
	}

	// FailAntigravityHelper for an unseen, non-current session must never overwrite s-active's live helper.
	if err := FailAntigravityHelper(configDir, 10, 20, "s-unseen-noncurrent", lookup); err != nil || !VerifyAntigravityHelper(configDir, owner, lookup) {
		t.Fatalf("unseen session failure latch overwrote active live owner: %v", err)
	}

	// Cleanup ignores mismatched attempt_id, then removes matching owner; Re-publish and test StopAntigravityWakeHelpers.
	if err := CleanupAntigravityHelper(configDir, AntigravityHelperOwner{
		AgyPID: 10, AgyStartMS: 20, HelperPID: 701, HelperStartMS: 7001, SessionID: "s-active", AttemptID: "wrong",
	}); err != nil || !VerifyAntigravityHelper(configDir, owner, lookup) {
		t.Fatal("mismatched cleanup removed live owner")
	}
	if err := CleanupAntigravityHelper(configDir, owner); err != nil || VerifyAntigravityHelper(configDir, owner, lookup) {
		t.Fatal("matching cleanup kept owner")
	}
	// FailAntigravityHelper latches failed when no live helper owns s-active.
	if err := FailAntigravityHelper(configDir, 10, 20, "s-active", lookup); err != nil {
		t.Fatal(err)
	}
	if st := readAntigravityProcState(hm.dir, 10, 20); st.Helper == nil || st.Helper.Status != "failed" {
		t.Fatalf("expected failed latch, got %+v", st.Helper)
	}

	// Re-publish and add subdirs / unreadable entries to cover StopAntigravityWakeHelpers branches.
	if _, err := PublishAntigravityHelper(configDir, owner, lookup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(hm.dir, "subdir.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hm.dir, "dead-proc.json"), []byte(`{"current_session_id":"s"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := StopAntigravityWakeHelpers(configDir); err != nil {
		t.Fatalf("StopAntigravityWakeHelpers = %v", err)
	}
	if VerifyAntigravityHelper(configDir, owner, lookup) {
		t.Fatal("helper still verified after StopAntigravityWakeHelpers")
	}
	// Second call when no live helpers remain exits early cleanly.
	if err := StopAntigravityWakeHelpers(configDir); err != nil {
		t.Fatal(err)
	}

	// StopAntigravityWakeHelpers error branches: ReadDir error (file at dir path), terminate error, lock error, write error.
	badConfig := t.TempDir()
	if err := os.WriteFile(filepath.Join(badConfig, antigravityHelperDirName), []byte("not-a-dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := StopAntigravityWakeHelpers(badConfig); err == nil {
		t.Fatal("expected ReadDir error")
	}

	// Publish again to test terminate error + lock error + vanished/changed file during StopAntigravityWakeHelpers.
	live[701] = 7001
	if _, err := PublishAntigravityHelper(configDir, owner, lookup); err != nil {
		t.Fatal(err)
	}
	ghostPath := antigravityProcFile(hm.dir, 11, 21)
	if err := writeAntigravityJSON(hm.dir, ghostPath, antigravityProcState{
		CurrentSessionID: "s2",
		Helper:           &antigravityHelperRecord{Status: "live", PID: 702, StartMS: 7002, SessionID: "s2", AttemptID: "a2"},
	}); err != nil {
		t.Fatal(err)
	}
	terminateAntigravityProc = func(pid int, _ int64) error {
		if pid == 702 {
			_ = os.Remove(ghostPath)
			return proc.ErrNotFound
		}
		return errors.New("kill denied")
	}
	unlock, err := hm.lock(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := StopAntigravityWakeHelpers(configDir); err == nil || !strings.Contains(err.Error(), "kill denied") {
		unlock()
		t.Fatalf("expected kill denied, got %v", err)
	}
	// With terminate succeeding while locked, lock error itself is returned.
	terminateAntigravityProc = func(int, int64) error { return proc.ErrNotFound }
	if err := StopAntigravityWakeHelpers(configDir); !errors.Is(err, ErrSessionsLocked) {
		unlock()
		t.Fatalf("expected lock error, got %v", err)
	}
	// Cleanup and Publish/Fail lock errors while locked.
	if err := CleanupAntigravityHelper(configDir, owner); !errors.Is(err, ErrSessionsLocked) {
		unlock()
		t.Fatalf("Cleanup lock err = %v", err)
	}
	if _, err := PublishAntigravityHelper(configDir, owner, lookup); !errors.Is(err, ErrSessionsLocked) {
		unlock()
		t.Fatalf("Publish lock err = %v", err)
	}
	if err := FailAntigravityHelper(configDir, 10, 20, "s-active", lookup); !errors.Is(err, ErrSessionsLocked) {
		unlock()
		t.Fatalf("Fail lock err = %v", err)
	}
	unlock()

	// Write failure inside StopAntigravityWakeHelpers + corrupt second-pass file.
	corruptSecond := antigravityProcFile(hm.dir, 12, 22)
	if err := writeAntigravityJSON(hm.dir, corruptSecond, antigravityProcState{
		CurrentSessionID: "s3",
		Helper:           &antigravityHelperRecord{Status: "live", PID: 703, StartMS: 7003, SessionID: "s3", AttemptID: "a3"},
	}); err != nil {
		t.Fatal(err)
	}
	terminateAntigravityProc = func(pid int, _ int64) error {
		if pid == 703 {
			_ = os.WriteFile(corruptSecond, []byte("{corrupt"), 0o600)
		}
		return proc.ErrNotFound
	}
	origCreate := createAntigravityTemp
	createAntigravityTemp = func(string, string) (*os.File, error) { return nil, errors.New("write err") }
	err = StopAntigravityWakeHelpers(configDir)
	createAntigravityTemp = origCreate
	if err == nil || !strings.Contains(err.Error(), "write err") {
		t.Fatalf("expected write err, got %v", err)
	}

	// writeAntigravityJSON error branches: Write err (read-only file), Sync err, Close err, protectPath err, Rename err.
	roPath := filepath.Join(t.TempDir(), "ro")
	if err := os.WriteFile(roPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	createAntigravityTemp = func(string, string) (*os.File, error) { return os.Open(roPath) }
	if err := writeAntigravityJSON(hm.dir, filepath.Join(hm.dir, "out.json"), antigravityProcState{}); err == nil {
		createAntigravityTemp = origCreate
		t.Fatal("expected Write error on read-only fd")
	}
	createAntigravityTemp = origCreate

	origSync := syncAntigravityTemp
	syncAntigravityTemp = func(*os.File) error { return errors.New("sync failed") }
	if err := writeAntigravityJSON(hm.dir, filepath.Join(hm.dir, "out.json"), antigravityProcState{}); err == nil {
		syncAntigravityTemp = origSync
		t.Fatal("expected Sync error")
	}
	syncAntigravityTemp = func(f *os.File) error { _ = f.Close(); return nil }
	if err := writeAntigravityJSON(hm.dir, filepath.Join(hm.dir, "out.json"), antigravityProcState{}); err == nil {
		syncAntigravityTemp = origSync
		t.Fatal("expected Close error")
	}
	syncAntigravityTemp = origSync

	origProtect := protectPath
	protectPath = func(string) error { return errors.New("protect failed") }
	if err := writeAntigravityJSON(hm.dir, filepath.Join(hm.dir, "out.json"), antigravityProcState{}); err == nil {
		protectPath = origProtect
		t.Fatal("expected protectPath error")
	}
	// Also cover PublishAntigravityHelper and FailAntigravityHelper directory() error via protectPath.
	if _, err := PublishAntigravityHelper(configDir, owner, lookup); err == nil {
		protectPath = origProtect
		t.Fatal("expected Publish directory error")
	}
	if err := FailAntigravityHelper(configDir, 10, 20, "s-active", lookup); err == nil {
		protectPath = origProtect
		t.Fatal("expected Fail directory error")
	}
	protectPath = origProtect

	nonEmptyDir := filepath.Join(hm.dir, "nonempty")
	if err := os.MkdirAll(filepath.Join(nonEmptyDir, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeAntigravityJSON(hm.dir, nonEmptyDir, antigravityProcState{}); err == nil {
		t.Fatal("expected Rename error onto directory")
	}

	// helperAlive with nil/non-live/invalid PID.
	if helperAlive(nil, lookup) || helperAlive(&antigravityHelperRecord{Status: "failed", PID: 10, StartMS: 20}, lookup) {
		t.Fatal("helperAlive non-live")
	}
}

func writeWakeTestDoc(t *testing.T, wakePath string, attempts []map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(wakePath), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"version":  1,
		"attempts": attempts,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wakePath, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func makeAgySystemStep(t *testing.T, source, typ, sender, msg string, withPreamble bool) string {
	t.Helper()
	prefix := ""
	if withPreamble {
		prefix = antigravitySystemPreamble
	}
	content := prefix + "<SYSTEM_MESSAGE>\n[Message] timestamp=2026-11-13T11:57:38Z sender=" + sender + " priority=MESSAGE_PRIORITY_HIGH content=" + msg + "\n</SYSTEM_MESSAGE>"
	raw, err := json.Marshal(map[string]any{
		"step_index": 4,
		"source":     source,
		"type":       typ,
		"status":     "DONE",
		"content":    content,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw) + "\n"
}

func TestAntigravityIncrementalScannerEdgesAndMerge(t *testing.T) {
	_, tokenPath, getenv := agyTestEnv(t)
	sessions := NewSessions(tokenPath)
	const convID = "e28a6164-6409-4810-b8ee-df8202a16a0a"
	const wakeMsg = "Iris notification [iris-wake:0123456789abcdef0123456789abcdef]: unread mail or freed claims are waiting. Call the Iris inbox tool to read them."

	// Strict line matcher rejects non-JSON, wrong source/type, missing tags/headers, background tasks, and partial matches.
	for _, badLine := range []string{
		"not json",
		`{"source":"USER_EXPLICIT","type":"USER_INPUT","content":"<SYSTEM_MESSAGE>\n[Message] timestamp=2026-11-13T11:57:38Z sender=` + convID + ` priority=HIGH content=` + wakeMsg + `\n</SYSTEM_MESSAGE>"}`,
		`{"source":"SYSTEM","type":"SYSTEM_MESSAGE","content":"no tags"}`,
		`{"source":"SYSTEM","type":"SYSTEM_MESSAGE","content":"<SYSTEM_MESSAGE>\nno closing"}`,
		`{"source":"SYSTEM","type":"SYSTEM_MESSAGE","content":"<SYSTEM_MESSAGE>\nStop hook blocked termination\n</SYSTEM_MESSAGE>"}`,
		`{"source":"SYSTEM","type":"SYSTEM_MESSAGE","content":"<SYSTEM_MESSAGE>\n[Message] timestamp= \nsender=x\n</SYSTEM_MESSAGE>"}`,
		`{"source":"SYSTEM","type":"SYSTEM_MESSAGE","content":"<SYSTEM_MESSAGE>\n[Message] timestamp=t1 nosender\n</SYSTEM_MESSAGE>"}`,
		`{"source":"SYSTEM","type":"SYSTEM_MESSAGE","content":"<SYSTEM_MESSAGE>\n[Message] timestamp=t1 sender=` + convID + `/task-1 priority=HIGH content=` + wakeMsg + `\n</SYSTEM_MESSAGE>"}`,
		`{"source":"SYSTEM","type":"SYSTEM_MESSAGE","content":"<SYSTEM_MESSAGE>\n[Message] timestamp=t1 sender=other priority=HIGH content=` + wakeMsg + `\n</SYSTEM_MESSAGE>"}`,
		`{"source":"SYSTEM","type":"SYSTEM_MESSAGE","content":"<SYSTEM_MESSAGE>\n[Message] timestamp=t1 sender=` + convID + ` noprio\n</SYSTEM_MESSAGE>"}`,
		`{"source":"SYSTEM","type":"SYSTEM_MESSAGE","content":"<SYSTEM_MESSAGE>\n[Message] timestamp=t1 sender=` + convID + ` priority=\tbad content=` + wakeMsg + `\n</SYSTEM_MESSAGE>"}`,
		`{"source":"SYSTEM","type":"SYSTEM_MESSAGE","content":"<SYSTEM_MESSAGE>\n[Message] timestamp=t1 sender=` + convID + ` priority=HIGH nocontent\n</SYSTEM_MESSAGE>"}`,
		`{"source":"SYSTEM","type":"SYSTEM_MESSAGE","content":"<SYSTEM_MESSAGE>\n[Message] timestamp=t1 sender=` + convID + ` priority=HIGH content=` + wakeMsg + ` extra\n</SYSTEM_MESSAGE>"}`,
	} {
		if matchesAntigravityWakeLine([]byte(badLine), convID, wakeMsg) {
			t.Fatalf("accepted invalid line: %s", badLine)
		}
	}
	// Valid without preamble and interactive sender=system both match.
	noPreamble := strings.TrimSpace(makeAgySystemStep(t, "SYSTEM", "SYSTEM_MESSAGE", convID, wakeMsg, false))
	if !matchesAntigravityWakeLine([]byte(noPreamble), convID, wakeMsg) {
		t.Fatal("rejected valid SYSTEM_MESSAGE without preamble")
	}
	interactiveSender := strings.TrimSpace(makeAgySystemStep(t, "SYSTEM", "SYSTEM_MESSAGE", "system", wakeMsg, true))
	if !matchesAntigravityWakeLine([]byte(interactiveSender), convID, wakeMsg) {
		t.Fatal("rejected valid interactive sender=system SYSTEM_MESSAGE")
	}

	// Nil sessions / invalid session / missing wake file / consumed attempt / missing fields.
	if c, more, err := ScanAntigravityWakeSession(nil, convID, "", time.Now(), time.Second); c || more || err != nil {
		t.Fatal("nil sessions")
	}
	if c, more, err := ScanAntigravityWakeSession(sessions, "", "", time.Now(), time.Second); c || more || err != nil {
		t.Fatal("empty session")
	}
	if c, more, err := ScanAntigravityWakeSession(sessions, convID, "", time.Now(), time.Second); c || more || err != nil {
		t.Fatal("missing wake file")
	}

	wakePath := sessions.WakePath("antigravity", convID)
	writeWakeTestDoc(t, wakePath, []map[string]any{{"id": "a0", "message": wakeMsg, "consumed_ms": 123}})
	if c, more, err := ScanAntigravityWakeSession(sessions, convID, "", time.Now(), time.Second); c || more || err != nil {
		t.Fatal("already consumed")
	}
	writeWakeTestDoc(t, wakePath, []map[string]any{{"id": "a0f", "message": wakeMsg, "answer": "failed"}})
	if c, more, err := ScanAntigravityWakeSession(sessions, convID, "/tmp/hint.jsonl", time.Now(), time.Second); c || more || err != nil {
		t.Fatal("already failed")
	}
	writeWakeTestDoc(t, wakePath, []map[string]any{{"id": "", "message": wakeMsg}})
	if c, more, err := ScanAntigravityWakeSession(sessions, convID, "", time.Now(), time.Second); c || more || err != nil {
		t.Fatal("missing id")
	}
	writeWakeTestDoc(t, wakePath, []map[string]any{{"id": "a1", "message": wakeMsg}})
	if c, more, err := ScanAntigravityWakeSession(sessions, convID, "", time.Now(), time.Second); c || more || err != nil {
		t.Fatal("missing transcript path")
	}

	// Missing transcript file on disk is a clean no-op.
	tDir := t.TempDir()
	missingTranscript := filepath.Join(tDir, "missing.jsonl")
	if c, more, err := ScanAntigravityWakeSession(sessions, convID, missingTranscript, time.Now(), time.Second); c || more || err != nil {
		t.Fatal("missing transcript file")
	}

	// Use small scan/line limits in one sub-section to test multi-pass budget cuts, >64KiB line skipping,
	// busy-race helper continuation, rotation, truncation, and stale-attempt / monotonic merges deterministically.
	origScanLimit := antigravityScanLimit
	origLineLimit := antigravityLineLimit
	t.Cleanup(func() {
		antigravityScanLimit = origScanLimit
		antigravityLineLimit = origLineLimit
	})
	antigravityScanLimit = 900
	antigravityLineLimit = 800

	transcript := filepath.Join(tDir, "transcript_full.jsonl")
	// Build content:
	// 1. An oversized line (> 800 bytes, spanning > 2 passes of 900 bytes = 2100 bytes) + "\n"
	// 2. Padding lines so that the real SYSTEM_MESSAGE (~540 bytes) straddles a 900-byte pass boundary!
	oversized := `{"step_index":1,"source":"MODEL","type":"GENERIC","content":"` + strings.Repeat("X", 2100) + "\"}\n"
	padLine := `{"step_index":2,"source":"MODEL","type":"GENERIC","content":"` + strings.Repeat("Y", 500) + "\"}\n"
	wakeLine := makeAgySystemStep(t, "SYSTEM", "SYSTEM_MESSAGE", convID, wakeMsg, true)
	fullText := oversized + padLine + wakeLine
	if err := os.WriteFile(transcript, []byte(fullText), 0o600); err != nil {
		t.Fatal(err)
	}

	now := time.UnixMilli(200_000_000_000)
	oldConsumed := now.Add(-31 * 24 * time.Hour).UnixMilli()
	writeWakeTestDoc(t, wakePath, []map[string]any{
		{
			"id":          "old-attempt",
			"token":       "old-tok",
			"message":     "old-msg",
			"answer":      "submitted",
			"consumed_ms": oldConsumed,
			"scan":        map[string]any{"transcript_path": transcript, "scanned_offset": 10},
		},
		{
			"id":      "att-live",
			"message": wakeMsg,
			"scan":    map[string]any{"transcript_path": transcript, "scanned_offset": 0},
		},
	})

	// Pass 1 (simulating PostInvocation hitting pass budget inside oversized line): skipping_line becomes true, hasMore=true, not consumed yet.
	consumed, hasMore, err := ScanAntigravityWakeSession(sessions, convID, transcript, now, time.Second)
	if err != nil || consumed || !hasMore {
		t.Fatalf("pass 1 = consumed=%v hasMore=%v err=%v", consumed, hasMore, err)
	}

	// Continuation passes (simulating helper poll loop catching up after session returned to idle):
	// walks across remaining oversized line, hits half-line budget cut on wakeLine without advancing past line start,
	// then reads complete wakeLine cleanly and consumes att-live!
	passes := 1
	for !consumed && hasMore && passes < 10 {
		passes++
		consumed, hasMore, err = ScanAntigravityWakeSession(sessions, convID, "", now, time.Second)
		if err != nil {
			t.Fatalf("pass %d err = %v", passes, err)
		}
	}
	if !consumed || passes < 3 {
		t.Fatalf("expected multi-pass consumption, got consumed=%v passes=%d", consumed, passes)
	}

	// Verify att-live has consumed_ms set AND old-attempt had token/message/answer/scan forgotten!
	d, ok := readWake(wakePath)
	if !ok || attemptTime(d.attempts[1], "consumed_ms") != now.UnixMilli() {
		t.Fatalf("wake doc after consume = %+v", d)
	}
	if _, hasScan := d.attempts[0]["scan"]; hasScan {
		t.Fatal("old consumed attempt kept scan cursor past memoryAge")
	}

	// Also verify PostInvocation Runner wiring consumes an outstanding guard directly.
	antigravityScanLimit = origScanLimit
	antigravityLineLimit = origLineLimit
	writeWakeTestDoc(t, wakePath, []map[string]any{{
		"id":      "att-hook",
		"message": wakeMsg,
		"scan":    map[string]any{"transcript_path": "/stale/path.jsonl", "file_id": "stale", "scanned_offset": 999999},
	}})
	(Runner{
		ClientName: "antigravity",
		EventName:  "PostInvocation",
		Stdin:      agyJSON(t, map[string]any{"conversationId": convID, "transcriptPath": transcript}),
		Getenv:     getenv,
		Now:        func() time.Time { return now },
	}).Run()
	d, _ = readWake(wakePath)
	if attemptTime(d.attempts[0], "consumed_ms") != now.UnixMilli() {
		t.Fatal("PostInvocation did not consume wake guard after transcriptHint reset")
	}

	// Rotation & truncation reset + EOF no-op when already caught up.
	smallTranscript := filepath.Join(tDir, "rot.jsonl")
	if err := os.WriteFile(smallTranscript, []byte("{\"step_index\":0}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, fid, sz, err := openRegularTranscript(smallTranscript)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	writeWakeTestDoc(t, wakePath, []map[string]any{{
		"id":      "att-rot",
		"message": wakeMsg,
		"scan":    map[string]any{"transcript_path": smallTranscript, "file_id": fid, "scanned_offset": sz},
	}})
	// At EOF with matching FileID -> no-op without locking.
	if c, more, err := ScanAntigravityWakeSession(sessions, convID, "", now, time.Second); c || more || err != nil {
		t.Fatalf("EOF no-op = %v, %v, %v", c, more, err)
	}
	// Truncation (stored ScannedOffset > current file size) resets cursor to current size.
	writeWakeTestDoc(t, wakePath, []map[string]any{{
		"id":      "att-rot",
		"message": wakeMsg,
		"scan":    map[string]any{"transcript_path": smallTranscript, "file_id": fid, "scanned_offset": sz + 500},
	}})
	if c, more, err := ScanAntigravityWakeSession(sessions, convID, "", now, time.Second); c || more || err != nil {
		t.Fatalf("truncation reset = %v, %v, %v", c, more, err)
	}

	// Multi-pass same-inode truncation: stored offset is ahead of truncated size, pass 1 resets and persists
	// intermediate progress on the same FileID, and continuation passes reach and consume the wake line.
	antigravityScanLimit = 900
	antigravityLineLimit = 800
	truncTranscript := filepath.Join(tDir, "same_inode_trunc.jsonl")
	if err := os.WriteFile(truncTranscript, []byte(strings.Repeat(padLine, 6)), 0o600); err != nil {
		t.Fatal(err)
	}
	tfOpen, truncFID, truncInitSz, err := openRegularTranscript(truncTranscript)
	if err != nil {
		t.Fatal(err)
	}
	_ = tfOpen.Close()
	writeWakeTestDoc(t, wakePath, []map[string]any{{
		"id":      "att-trunc-multipass",
		"message": wakeMsg,
		"scan":    map[string]any{"transcript_path": truncTranscript, "file_id": truncFID, "scanned_offset": truncInitSz + 2000},
	}})
	// Truncate the same file in place so FileID stays identical while size drops below stored ScannedOffset,
	// placing the wake line beyond the first 900-byte pass budget.
	tfWrite, err := os.OpenFile(truncTranscript, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tfWrite.WriteString(padLine + padLine + wakeLine); err != nil {
		_ = tfWrite.Close()
		t.Fatal(err)
	}
	if err := tfWrite.Close(); err != nil {
		t.Fatal(err)
	}
	tfAfter, afterFID, afterSz, err := openRegularTranscript(truncTranscript)
	if err != nil {
		t.Fatal(err)
	}
	_ = tfAfter.Close()
	if afterFID != truncFID || afterSz >= truncInitSz+2000 {
		t.Fatalf("expected in-place truncation keeping FileID %q (got %q) and smaller size %d", truncFID, afterFID, afterSz)
	}
	c1, more1, err := ScanAntigravityWakeSession(sessions, convID, "", now, time.Second)
	if err != nil || c1 || !more1 {
		t.Fatalf("same-inode truncation pass 1 = consumed=%v hasMore=%v err=%v", c1, more1, err)
	}
	truncDoc, ok := readWake(wakePath)
	if !ok || len(truncDoc.attempts) != 1 {
		t.Fatal("missing wake doc after same-inode truncation pass 1")
	}
	var savedTrunc AntigravityScanCursor
	if err := json.Unmarshal(truncDoc.attempts[0]["scan"], &savedTrunc); err != nil {
		t.Fatal(err)
	}
	if savedTrunc.FileID != truncFID || savedTrunc.ScannedOffset <= 0 || savedTrunc.ScannedOffset >= afterSz {
		t.Fatalf("same-inode truncation pass 1 did not persist reset cursor: %+v (afterSz=%d)", savedTrunc, afterSz)
	}
	truncConsumed := false
	for step := range 5 {
		var errStep error
		truncConsumed, more1, errStep = ScanAntigravityWakeSession(sessions, convID, "", now, time.Second)
		if errStep != nil {
			t.Fatalf("same-inode truncation continuation %d err = %v", step, errStep)
		}
		if truncConsumed || !more1 {
			break
		}
	}
	if !truncConsumed {
		t.Fatal("same-inode truncation continuation passes never consumed wake")
	}
	antigravityScanLimit = origScanLimit
	antigravityLineLimit = origLineLimit

	// Read error, Protect error, Lock error, vanished wake file, stale attempt ID, and non-advancing cursor merge.
	origRead := readTranscriptChunk
	readTranscriptChunk = func(*os.File, int64, int64) ([]byte, error) { return nil, errors.New("io err") }
	writeWakeTestDoc(t, wakePath, []map[string]any{{"id": "att-io", "message": wakeMsg, "scan": map[string]any{"transcript_path": smallTranscript}}})
	if c, more, err := ScanAntigravityWakeSession(sessions, convID, "", now, time.Second); c || more || err != nil {
		readTranscriptChunk = origRead
		t.Fatalf("read chunk err = %v, %v, %v", c, more, err)
	}
	readTranscriptChunk = origRead

	badSess := &Sessions{memory: fileMemory{dir: filepath.Join(tDir, "sessions"), now: time.Now, restrict: func(string) error { return errors.New("restrict err") }}}
	writeWakeTestDoc(t, badSess.WakePath("antigravity", convID), []map[string]any{{"id": "att-p", "message": wakeMsg, "scan": map[string]any{"transcript_path": smallTranscript}}})
	if _, _, err := ScanAntigravityWakeSession(badSess, convID, "", now, time.Second); err == nil {
		t.Fatal("expected Protect error")
	}

	unlock, err := sessions.Lock(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, _, lockErr := ScanAntigravityWakeSession(sessions, convID, "", now, 5*time.Millisecond)
	unlock()
	if !errors.Is(lockErr, ErrSessionsLocked) {
		t.Fatalf("expected ErrSessionsLocked, got %v", lockErr)
	}

	// Monotonic cursor check unit assertions: same FileID regression rejected, equal offset setting SkippingLine accepted, stale rotation rejected.
	stored := AntigravityScanCursor{TranscriptPath: smallTranscript, FileID: "f1", ScannedOffset: 100, SkippingLine: false}
	if shouldAdvanceAntigravityCursor(stored, antigravityScanPass{Next: AntigravityScanCursor{TranscriptPath: smallTranscript, FileID: "f1", ScannedOffset: 50}}) {
		t.Fatal("accepted backward offset on same file_id")
	}
	if !shouldAdvanceAntigravityCursor(stored, antigravityScanPass{Next: AntigravityScanCursor{TranscriptPath: smallTranscript, FileID: "f1", ScannedOffset: 100, SkippingLine: true}}) {
		t.Fatal("rejected equal-offset SkippingLine transition")
	}
	if shouldAdvanceAntigravityCursor(stored, antigravityScanPass{
		ObservedStored:     AntigravityScanCursor{TranscriptPath: smallTranscript, FileID: "f0"},
		RotatedOrTruncated: true,
		Next:               AntigravityScanCursor{TranscriptPath: smallTranscript, FileID: "f2", ScannedOffset: 10},
	}) {
		t.Fatal("accepted stale rotation from old ObservedStored")
	}

	// Hinted transcript_path naming a different file from stored cursor saves the hinted cursor so subsequent passes do not rescan from 0.
	hintedTranscript := filepath.Join(t.TempDir(), "hinted.jsonl")
	if err := os.WriteFile(hintedTranscript, []byte(`{"source":"USER_EXPLICIT","type":"USER_INPUT","content":"hi"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeWakeTestDoc(t, wakePath, []map[string]any{{
		"id":      "att-hint",
		"message": wakeMsg,
		"scan":    map[string]any{"transcript_path": smallTranscript, "file_id": fid, "scanned_offset": sz},
	}})
	if c, more, err := ScanAntigravityWakeSession(sessions, convID, hintedTranscript, now, time.Second); c || more || err != nil {
		t.Fatalf("hinted scan = %v, %v, %v", c, more, err)
	}
	hintDoc, ok := readWake(wakePath)
	if !ok || len(hintDoc.attempts) != 1 {
		t.Fatal("missing hinted wake doc")
	}
	var savedHint AntigravityScanCursor
	if err := json.Unmarshal(hintDoc.attempts[0]["scan"], &savedHint); err != nil {
		t.Fatal(err)
	}
	if savedHint.TranscriptPath != hintedTranscript || savedHint.FileID == "" || savedHint.ScannedOffset == 0 {
		t.Fatalf("hinted cursor was not saved: %+v", savedHint)
	}

	// Concurrent wake file removal / attempt replacement / cursor advance before merge lock is acquired.
	readTranscriptChunk = func(f *os.File, off, limit int64) ([]byte, error) {
		_ = os.Remove(wakePath)
		return origRead(f, off, limit)
	}
	writeWakeTestDoc(t, wakePath, []map[string]any{{"id": "att-vanish", "message": wakeMsg, "scan": map[string]any{"transcript_path": smallTranscript}}})
	if c, _, err := ScanAntigravityWakeSession(sessions, convID, "", now, time.Second); c || err != nil {
		readTranscriptChunk = origRead
		t.Fatalf("vanished wake file = %v, %v", c, err)
	}

	readTranscriptChunk = func(f *os.File, off, limit int64) ([]byte, error) {
		writeWakeTestDoc(t, wakePath, []map[string]any{{"id": "att-newer", "message": wakeMsg, "scan": map[string]any{"transcript_path": smallTranscript}}})
		return origRead(f, off, limit)
	}
	writeWakeTestDoc(t, wakePath, []map[string]any{{"id": "att-stale", "message": wakeMsg, "scan": map[string]any{"transcript_path": smallTranscript}}})
	if c, _, err := ScanAntigravityWakeSession(sessions, convID, "", now, time.Second); c || err != nil {
		readTranscriptChunk = origRead
		t.Fatalf("replaced attempt = %v, %v", c, err)
	}

	readTranscriptChunk = func(f *os.File, off, limit int64) ([]byte, error) {
		writeWakeTestDoc(t, wakePath, []map[string]any{{"id": "att-ahead", "message": wakeMsg, "scan": map[string]any{"transcript_path": smallTranscript, "file_id": fid, "scanned_offset": sz + 999}}})
		return origRead(f, off, limit)
	}
	writeWakeTestDoc(t, wakePath, []map[string]any{{"id": "att-ahead", "message": wakeMsg, "scan": map[string]any{"transcript_path": smallTranscript}}})
	if c, _, err := ScanAntigravityWakeSession(sessions, convID, "", now, time.Second); c || err != nil {
		readTranscriptChunk = origRead
		t.Fatalf("ahead cursor = %v, %v", c, err)
	}
	readTranscriptChunk = origRead
}

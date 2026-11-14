package wake

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	ossignal "os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/proc"
)

const (
	antigravityActive          = "Antigravity session is active"
	antigravityExeUnavailable  = "Antigravity executable is unavailable"
	antigravitySendStartFailed = "Antigravity send-message command could not start"
	antigravityRejectedWake    = "Antigravity rejected the wake message"
	antigravityHelperEnv       = "IRIS_AGY_HELPER"
)

var (
	errAntigravityDeferred = errors.New("antigravity wake deferred")
	osExecutable           = os.Executable
	openDevNull            = func() (*os.File, error) { return os.OpenFile(os.DevNull, os.O_RDWR, 0) }
)

type antigravityOptions struct {
	args             []string
	getenv           func(string) string
	pinned           hubclient.PinnedConfig
	runtimeEnv       func() ([]string, error)
	ancestor         func() (proc.Ancestor, error)
	lookup           func(int) (proc.Snapshot, bool)
	executable       func(int, int64) (string, error)
	terminate        func(int, int64) error
	selfPID          func() int
	irisExecutable   func() (string, error)
	dialLS           func(context.Context, string, string) (net.Conn, error)
	spawnDetached    func(string, []string, []string, proc.Ancestor) (int, func(), func(), error)
	startupBudget    time.Duration
	handshakeTimeout time.Duration
	handshakePoll    time.Duration
	maintainInterval time.Duration
	targetPoll       time.Duration
	settle           time.Duration
	filePoll         time.Duration
	reconnectBase    time.Duration
	reconnectMax     time.Duration
	queueTimeout     time.Duration
	waitDelay        time.Duration
	now              func() time.Time
	host             string
	cwd              string
}

func defaultAntigravityOptions() antigravityOptions {
	host, _ := os.Hostname()
	cwd, _ := os.Getwd()
	return antigravityOptions{
		args:             os.Args[1:],
		getenv:           os.Getenv,
		pinned:           hubclient.PinnedConfig{},
		runtimeEnv:       hubclient.PinnedRuntimeEnv,
		ancestor:         proc.Nearest,
		lookup:           proc.Lookup,
		executable:       proc.Executable,
		terminate:        proc.Terminate,
		selfPID:          os.Getpid,
		irisExecutable:   verifiedSelfExecutable,
		dialLS:           (&net.Dialer{}).DialContext,
		spawnDetached:    spawnDetachedProcess,
		startupBudget:    2 * time.Second,
		handshakeTimeout: 2 * time.Second,
		handshakePoll:    10 * time.Millisecond,
		maintainInterval: 20 * time.Second,
		targetPoll:       time.Second,
		settle:           10 * time.Second,
		filePoll:         15 * time.Second,
		reconnectBase:    time.Second,
		reconnectMax:     30 * time.Second,
		queueTimeout:     30 * time.Second,
		waitDelay:        time.Second,
		now:              time.Now,
		host:             host,
		cwd:              cwd,
	}
}

// RunAntigravity executes `iris watch --client antigravity --wake`, acting as the
// short-lived host launcher in run_command or as the detached wake helper child.
func RunAntigravity() error {
	return runAntigravity(context.Background(), defaultAntigravityOptions())
}

func runAntigravity(ctx context.Context, opts antigravityOptions) error {
	debug.SetTraceback("none")
	if !slices.Equal(opts.args, []string{"watch", "--client", "antigravity", "--wake"}) {
		return errors.New("usage: iris watch --client antigravity --wake")
	}
	if handshake := opts.getenv(antigravityHelperEnv); handshake != "" {
		return runAntigravityChild(ctx, opts, handshake)
	}
	return runAntigravityParent(ctx, opts)
}

func verifiedSelfExecutable() (string, error) {
	exe, err := osExecutable()
	if err != nil || !filepath.IsAbs(exe) {
		return "", errors.New("iris executable is unavailable")
	}
	info, err := os.Stat(exe)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("iris executable is unavailable")
	}
	return exe, nil
}

// spawnDetachedProcess starts the helper under parent, the agy process the launcher has
// already verified. The parent is opened here, only for the spawn, and released after it.
func spawnDetachedProcess(exe string, args, env []string, parent proc.Ancestor) (int, func(), func(), error) {
	devNull, err := openDevNull()
	if err != nil {
		return 0, nil, nil, err
	}
	defer func() { _ = devNull.Close() }()
	cmd := exec.Command(exe, args...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, devNull, devNull
	releaseParent, err := proc.DetachUnder(cmd, parent.PID, parent.StartMS)
	if err != nil {
		return 0, nil, nil, err
	}
	defer releaseParent()
	if err := cmd.Start(); err != nil {
		return 0, nil, nil, err
	}
	handle := cmd.Process
	pid := handle.Pid
	release := func() { _ = handle.Release() }
	kill := func() {
		_ = handle.Kill()
		_, _ = handle.Wait()
	}
	return pid, release, kill, nil
}

func canonicalAntigravitySession(raw string) (string, bool) {
	id, ok := canonicalSession(raw)
	return id, ok && id == raw
}

func validAntigravitySecret(s string) bool {
	return s != "" && len(s) <= 4096 && strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0
}

func parseAntigravityHandshake(raw string) (int, int64, string, bool) {
	parts := strings.Split(raw, ":")
	if len(parts) != 3 {
		return 0, 0, "", false
	}
	pid, err1 := strconv.Atoi(parts[0])
	startMS, err2 := strconv.ParseInt(parts[1], 10, 64)
	idBytes, err3 := hex.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil || pid <= 0 || startMS <= 0 || len(idBytes) != 16 || hex.EncodeToString(idBytes) != parts[2] {
		return 0, 0, "", false
	}
	return pid, startMS, parts[2], true
}

func validateLoopbackLS(ctx context.Context, raw string, dial func(context.Context, string, string) (net.Conn, error)) (string, error) {
	host, portText, err := net.SplitHostPort(raw)
	if err != nil {
		return "", errors.New("invalid language server address")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", errors.New("invalid language server port")
	}
	portCanon := strconv.Itoa(port)
	var candidates []string
	if strings.EqualFold(host, "localhost") {
		// agy 1.5.8 exports ANTIGRAVITY_LS_ADDRESS=localhost:<port>; pin directly to literal
		// loopback IPs without consulting DNS or /etc/hosts.
		candidates = []string{net.JoinHostPort("127.0.0.1", portCanon), net.JoinHostPort("::1", portCanon)}
	} else {
		addr, err := netip.ParseAddr(host)
		if err != nil || addr.Is4In6() || addr.Zone() != "" || !addr.IsLoopback() {
			return "", errors.New("language server address must be loopback")
		}
		candidates = []string{net.JoinHostPort(addr.String(), portCanon)}
	}
	for _, canonical := range candidates {
		dialCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		conn, err := dial(dialCtx, "tcp", canonical)
		cancel()
		if err == nil {
			_ = conn.Close()
			return canonical, nil
		}
	}
	return "", errors.New("language server is unreachable")
}

func runAntigravityParent(parentCtx context.Context, opts antigravityOptions) error {
	ctx, cancel := context.WithTimeout(parentCtx, opts.startupBudget)
	defer cancel()

	resolved, configDir, cfgErr := opts.pinned.Load()
	anc, ancErr := opts.ancestor()
	hasAnc := ancErr == nil && anc.PID > 0 && anc.StartMS > 0 && proc.SingleSession(anc, "antigravity")
	convID, validConv := canonicalAntigravitySession(opts.getenv("ANTIGRAVITY_CONVERSATION_ID"))

	latchFailed := func() {
		if configDir != "" && hasAnc && validConv {
			_ = hook.FailAntigravityHelper(configDir, anc.PID, anc.StartMS, convID, opts.lookup)
		}
	}

	if cfgErr != nil || hubclient.ValidateURL(resolved.URL) != nil {
		latchFailed()
		return errors.New("antigravity wake configuration is unavailable")
	}
	runtimeEnv, err := opts.runtimeEnv()
	if err != nil {
		latchFailed()
		return errors.New("antigravity wake runtime environment is unavailable")
	}
	if !hook.AntigravityWakeEnabled(configDir) {
		latchFailed()
		return errors.New("antigravity wake is not enabled")
	}
	if !hasAnc || !validConv {
		latchFailed()
		return errors.New("antigravity session ancestor is unavailable")
	}
	if _, err := opts.executable(anc.PID, anc.StartMS); err != nil {
		latchFailed()
		return errors.New("antigravity executable is unavailable")
	}
	csrf := opts.getenv("ANTIGRAVITY_CSRF_TOKEN")
	if !validAntigravitySecret(csrf) {
		latchFailed()
		return errors.New("antigravity language server credentials are unavailable")
	}
	currentSession, alreadyLive := hook.ReadAntigravitySession(configDir, anc.PID, anc.StartMS, opts.lookup)
	if currentSession != convID {
		latchFailed()
		return errors.New("antigravity conversation is not active on this process")
	}
	lsAddr, err := validateLoopbackLS(ctx, opts.getenv("ANTIGRAVITY_LS_ADDRESS"), opts.dialLS)
	if err != nil {
		latchFailed()
		return err
	}
	if alreadyLive {
		return nil
	}
	selfExe, err := opts.irisExecutable()
	if err != nil {
		latchFailed()
		return err
	}
	id := attemptID()
	childEnv := append([]string{
		"ANTIGRAVITY_CONVERSATION_ID=" + convID,
		"ANTIGRAVITY_LS_ADDRESS=" + lsAddr,
		"ANTIGRAVITY_CSRF_TOKEN=" + csrf,
		antigravityHelperEnv + "=" + fmt.Sprintf("%d:%d:%s", anc.PID, anc.StartMS, id),
	}, runtimeEnv...)

	helperPID, release, kill, err := opts.spawnDetached(selfExe, []string{"watch", "--client", "antigravity", "--wake"}, childEnv, anc)
	if err != nil {
		latchFailed()
		return errors.New("antigravity wake helper could not start")
	}
	snap, ok := opts.lookup(helperPID)
	if !ok || snap.StartMS <= 0 || ctx.Err() != nil {
		kill()
		latchFailed()
		return errors.New("antigravity wake helper exited during startup")
	}
	owner := hook.AntigravityHelperOwner{
		AgyPID:        anc.PID,
		AgyStartMS:    anc.StartMS,
		HelperPID:     helperPID,
		HelperStartMS: snap.StartMS,
		SessionID:     convID,
		AttemptID:     id,
	}
	alreadyLive, err = hook.PublishAntigravityHelper(configDir, owner, opts.lookup)
	if err != nil {
		_ = opts.terminate(helperPID, snap.StartMS)
		kill()
		latchFailed()
		return errors.New("antigravity wake helper state could not be recorded")
	}
	if alreadyLive {
		_ = opts.terminate(helperPID, snap.StartMS)
		kill()
		return nil
	}
	after, ok := opts.lookup(helperPID)
	if !ok || after.StartMS != snap.StartMS || ctx.Err() != nil {
		_ = hook.CleanupAntigravityHelper(configDir, owner)
		kill()
		latchFailed()
		return errors.New("antigravity wake helper did not remain alive")
	}
	release()
	return nil
}

func waitAntigravityHandshake(ctx context.Context, configDir string, owner hook.AntigravityHelperOwner, opts antigravityOptions) bool {
	deadline := time.Now().Add(opts.handshakeTimeout)
	for {
		if hook.VerifyAntigravityHelper(configDir, owner, opts.lookup) {
			return true
		}
		if time.Now().After(deadline) || !pause(ctx, opts.handshakePoll) {
			return false
		}
	}
}

func runAntigravityChild(parentCtx context.Context, opts antigravityOptions, rawHandshake string) error {
	agyPID, agyStartMS, id, ok := parseAntigravityHandshake(rawHandshake)
	if !ok {
		return errors.New("invalid antigravity helper handshake")
	}
	resolved, configDir, err := opts.pinned.Load()
	if err != nil || hubclient.ValidateURL(resolved.URL) != nil {
		return errors.New("antigravity wake configuration is unavailable")
	}
	runtimeEnv, err := opts.runtimeEnv()
	if err != nil {
		return errors.New("antigravity wake runtime environment is unavailable")
	}
	convID, validConv := canonicalAntigravitySession(opts.getenv("ANTIGRAVITY_CONVERSATION_ID"))
	csrf := opts.getenv("ANTIGRAVITY_CSRF_TOKEN")
	if !validConv || !validAntigravitySecret(csrf) {
		return errors.New("antigravity language server credentials are unavailable")
	}
	lsAddr, err := validateLoopbackLS(parentCtx, opts.getenv("ANTIGRAVITY_LS_ADDRESS"), opts.dialLS)
	if err != nil {
		return err
	}
	selfSnap, ok := opts.lookup(opts.selfPID())
	if !ok || selfSnap.StartMS <= 0 {
		return errors.New("antigravity helper process identity is unavailable")
	}
	owner := hook.AntigravityHelperOwner{
		AgyPID:        agyPID,
		AgyStartMS:    agyStartMS,
		HelperPID:     selfSnap.PID,
		HelperStartMS: selfSnap.StartMS,
		SessionID:     convID,
		AttemptID:     id,
	}
	if !waitAntigravityHandshake(parentCtx, configDir, owner, opts) {
		return errors.New("antigravity helper ownership was not confirmed")
	}
	defer func() { _ = hook.CleanupAntigravityHelper(configDir, owner) }()

	ctx, cancel := ossignal.NotifyContext(parentCtx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	alive := func() bool {
		return hook.VerifyAntigravityHelper(configDir, owner, opts.lookup)
	}

	hubClient := &hubclient.Client{Config: resolved}
	resolver := binding.NewAntigravityWake(hubClient, convID, proc.Ancestor{PID: agyPID, StartMS: agyStartMS, Name: "agy"})
	resolver.Host, resolver.CWD = opts.host, opts.cwd
	resolver.Observe = hook.Observer(func(k string) string {
		if k == "IRIS_TOKEN_FILE" {
			return resolved.TokenFile
		}
		return ""
	}, "antigravity")

	bindCtx, bindCancel := context.WithTimeout(ctx, 5*time.Second)
	resolver.LookupWake(bindCtx)
	bindCancel()

	agentEnv := append([]string{
		"ANTIGRAVITY_CONVERSATION_ID=" + convID,
		"ANTIGRAVITY_LS_ADDRESS=" + lsAddr,
		"ANTIGRAVITY_CSRF_TOKEN=" + csrf,
	}, runtimeEnv...)

	guarded := &antigravityTargetResolver{
		base:   resolver,
		alive:  alive,
		cancel: cancel,
	}
	cmds := &antigravityCommands{
		configDir:    configDir,
		owner:        owner,
		agentEnv:     agentEnv,
		executable:   opts.executable,
		now:          opts.now,
		queueTimeout: opts.queueTimeout,
		waitDelay:    opts.waitDelay,
	}
	runner := &Runner{
		resolver:      guarded,
		source:        hubSource{client: hubClient},
		sessions:      hook.NewSessions(resolved.TokenFile),
		commands:      cmds,
		clientVersion: func() string { return "" },
		observe:       resolver.Observe,
		now:           opts.now,
		targetPoll:    opts.targetPoll,
		settle:        opts.settle,
		filePoll:      opts.filePoll,
		reconnectBase: opts.reconnectBase,
		reconnectMax:  opts.reconnectMax,
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resolver.Maintain(ctx, "shim", opts.maintainInterval, alive, cancel)
	}()
	runner.Run(ctx)
	cancel()
	wg.Wait()
	return nil
}

type antigravityTargetResolver struct {
	base   *binding.Resolver
	alive  func() bool
	cancel context.CancelFunc
}

func (r *antigravityTargetResolver) Primary() (binding.WakeTarget, bool) {
	if !r.alive() {
		r.cancel()
		return binding.WakeTarget{}, false
	}
	return r.base.Primary()
}

func (r *antigravityTargetResolver) StartIfPrimary(expected binding.WakeTarget, start func() error) (bool, error) {
	return r.base.StartIfPrimary(expected, func() error {
		if !r.alive() {
			r.cancel()
			return errAntigravityDeferred
		}
		return start()
	})
}

type antigravityCommands struct {
	configDir    string
	owner        hook.AntigravityHelperOwner
	agentEnv     []string
	executable   func(int, int64) (string, error)
	now          func() time.Time
	queueTimeout time.Duration
	waitDelay    time.Duration
}

func (c *antigravityCommands) check(_ context.Context, _ string) (binaryInfo, string, bool) {
	info := binaryInfo{CheckedMS: c.now().UnixMilli()}
	exe, err := c.executable(c.owner.AgyPID, c.owner.AgyStartMS)
	if err != nil {
		return info, antigravityExeUnavailable, false
	}
	info.Path = exe
	return info, "", false
}

func (c *antigravityCommands) idle(_ context.Context, session string) (bool, string) {
	conv, ok := hook.ReadAntigravityConvState(c.configDir, session)
	if !ok || !conv.Idle {
		return false, antigravityActive
	}
	return true, ""
}

func (c *antigravityCommands) initialScan(session string) *hook.AntigravityScanCursor {
	conv, _ := hook.ReadAntigravityConvState(c.configDir, session)
	offset := conv.IdleOffset
	if conv.FileID == "" {
		offset = 0
	}
	return &hook.AntigravityScanCursor{
		TranscriptPath: conv.TranscriptPath,
		FileID:         conv.FileID,
		ScannedOffset:  offset,
	}
}

func (c *antigravityCommands) queue(ctx context.Context, session, message string, start func(func() error) (bool, error)) commandAnswer {
	ctx, cancel := context.WithTimeout(ctx, c.queueTimeout)
	defer cancel()
	out, errs := &limitedOutput{limit: 64 << 10}, &limitedOutput{limit: 64 << 10}
	var cmd *exec.Cmd
	abortDetail := "Session binding changed before message submission"
	started, err := start(func() error {
		conv, ok := hook.ReadAntigravityConvState(c.configDir, session)
		if !ok || !conv.Idle {
			abortDetail = antigravityActive
			return errAntigravityDeferred
		}
		exe, exeErr := c.executable(c.owner.AgyPID, c.owner.AgyStartMS)
		if exeErr != nil {
			abortDetail = antigravityExeUnavailable
			return errAntigravityDeferred
		}
		cmd = exec.CommandContext(ctx, exe, "agentapi", "send-message", session, message)
		cmd.Env = c.agentEnv
		proc.NoConsole(cmd)
		cmd.Stdout, cmd.Stderr = out, errs
		cmd.WaitDelay = c.waitDelay
		return cmd.Start()
	})
	if !started {
		if err == nil || errors.Is(err, errAntigravityDeferred) {
			return commandAnswer{answer: "aborted", detail: abortDetail}
		}
		return commandAnswer{answer: "failed", detail: antigravitySendStartFailed}
	}
	err = cmd.Wait()
	if ctx.Err() != nil || out.overflow || errs.overflow {
		return commandAnswer{answer: "unknown", detail: "Antigravity send-message command exceeded its bound", started: true}
	}
	answer, detail := classifyAntigravitySend(session, out.data, errs.data, err)
	return commandAnswer{answer: answer, detail: detail, started: true}
}

func classifyAntigravitySend(session string, stdout, stderr []byte, result error) (string, string) {
	if len(stderr) != 0 {
		return "unknown", "Antigravity send-message response was not a known outcome"
	}
	var envelope struct {
		Response *struct {
			SendMessage *struct {
				RecipientID string `json:"recipientId"`
			} `json:"sendMessage"`
		} `json:"response"`
		Error string `json:"error"`
	}
	if json.Unmarshal(stdout, &envelope) != nil {
		return "unknown", "Antigravity send-message response was not a known outcome"
	}
	if result == nil {
		if envelope.Error == "" && envelope.Response != nil && envelope.Response.SendMessage != nil && envelope.Response.SendMessage.RecipientID == session {
			return "submitted", ""
		}
		return "unknown", "Antigravity send-message response was not a known outcome"
	}
	var status *exec.ExitError
	if errors.As(result, &status) && status.ExitCode() == 1 && envelope.Error != "" && isDeterministicAntigravityRejection(envelope.Error) {
		return "failed", antigravityRejectedWake
	}
	return "unknown", "Antigravity send-message response was not a known outcome"
}

// isDeterministicAntigravityRejection names failures that happen before anything is queued. A
// refused connection reads `connect: connection refused` on Unix and, from agy on Windows,
// `connectex: No connection could be made because the target machine actively refused it.`
// (measured with agy 1.5.8). Go formats Windows errors in English first, so that text does not
// follow the system language.
func isDeterministicAntigravityRejection(errText string) bool {
	return strings.Contains(errText, "code = Unauthenticated") ||
		strings.Contains(errText, "code = InvalidArgument") ||
		strings.Contains(errText, "code = NotFound") ||
		(strings.Contains(errText, "code = Unavailable") &&
			(strings.Contains(errText, "connect: connection refused") ||
				strings.Contains(errText, "connectex: No connection could be made because the target machine actively refused it")))
}

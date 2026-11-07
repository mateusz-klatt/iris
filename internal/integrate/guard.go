package integrate

import "strings"

// Guard is the D4 command for clients that treat a missing binary as a deny.
// Copilot on Windows uses PowerShell. Antigravity on Windows uses cmd and where.
// Other platforms use a POSIX shell. The allow text is that client's fail-open output.
func Guard(goos, client, iris, event string) string {
	allow := allowText(client)
	if goos == "windows" && client == Copilot {
		return guardPowerShell(iris, client, event, allow)
	}
	if goos == "windows" {
		return guardCmd(iris, client, event, allow)
	}
	return guardBash(iris, client, event, allow)
}

func allowText(client string) string {
	switch client {
	case Antigravity:
		return `{"decision":"allow"}`
	case Cursor:
		return `{"permission":"allow"}`
	default:
		return "{}"
	}
}

// The guards run the absolute path the integration recorded, then iris from PATH, then give the
// allow text. Checking only PATH skipped the hook silently whenever the client's PATH lacked the
// directory of the recorded binary: measured with Copilot on macOS, no session reached the hub and
// nothing was logged, because iris never started.
func guardBash(iris, client, event, allow string) string {
	return "if [ -x " + shellQuote(iris) + " ]; then " + hookShell(iris, client, event) +
		"; elif command -v iris >/dev/null 2>&1; then " + hookShell("iris", client, event) +
		"; else false; fi || echo " + shellQuote(allow)
}

func guardPowerShell(iris, client, event, allow string) string {
	// Test-Path sees a missing recorded binary, and Get-Command finds iris on PATH instead.
	// $LASTEXITCODE catches a crash after it starts.
	// exit 0 keeps a crash from becoming the hook's status; the allow text is the result.
	find := "$i = " + psQuote(iris) + "; if (-not (Test-Path -LiteralPath $i -PathType Leaf)) { $i = (Get-Command iris -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1).Source }; "
	run := "& $i hook --client " + client + " --event " + event
	out := "Write-Output " + psQuote(allow)
	return find + "if ($i) { " + run + "; if ($LASTEXITCODE -ne 0) { " + out + " } } else { " + out + " }; exit 0"
}

func guardCmd(iris, client, event, allow string) string {
	run := iris
	if strings.ContainsAny(iris, " \t\"") {
		run = `"` + strings.ReplaceAll(iris, `"`, `""`) + `"`
	}
	hook := " hook --client " + client + " --event " + event
	// cmd echo prints quote characters literally. Do not wrap or double them. The closing
	// parenthesis ends each echo, so the allow text must not contain one.
	return "if exist " + quoted(run) + " (" + run + hook + " || echo " + allow + ") else (where iris >NUL 2>&1 && iris" + hook + " || echo " + allow + ")"
}

// quoted gives cmd's if exist one argument, whether or not run already carries quotes.
func quoted(run string) string {
	if strings.HasPrefix(run, `"`) {
		return run
	}
	return `"` + run + `"`
}

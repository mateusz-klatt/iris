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

func guardBash(iris, client, event, allow string) string {
	return "command -v iris >/dev/null 2>&1 && " + hookShell(iris, client, event) + " || echo " + shellQuote(allow)
}

func guardPowerShell(iris, client, event, allow string) string {
	// Get-Command sees a missing iris. $LASTEXITCODE catches a crash after it starts.
	// exit 0 keeps a crash from becoming the hook's status; the allow text is the result.
	run := "& " + psQuote(iris) + " hook --client " + client + " --event " + event
	out := "Write-Output " + psQuote(allow)
	return "if (Get-Command iris -ErrorAction SilentlyContinue) { " + run + "; if ($LASTEXITCODE -ne 0) { " + out + " } } else { " + out + " }; exit 0"
}

func guardCmd(iris, client, event, allow string) string {
	run := iris
	if strings.ContainsAny(iris, " \t\"") {
		run = `"` + strings.ReplaceAll(iris, `"`, `""`) + `"`
	}
	// cmd echo prints quote characters literally. Do not wrap or double them.
	return "where iris >NUL 2>&1 && " + run + " hook --client " + client + " --event " + event + " || echo " + allow
}

package doctor

import (
	"errors"
	"fmt"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/integrate"
	"github.com/mateusz-klatt/iris/internal/wake"
)

var wakeConfiguration = integrate.CodexWake
var inspectWakes = wake.Inspect

const wakeRecovery = "Resume a live session so its exact queued prompt hook can consume its guard. Remove the exact guard file only after permanently closing that session and stopping its MCP child; removing a live guard can admit duplicate queue entries."
const antigravityWakeRecovery = "Continue that agy conversation so a hook can scan its transcript. If the notification never reached it, the guard stays. Remove the exact guard file only after that conversation is permanently closed and its wake helper has stopped; removing a live guard can admit a duplicate notification."
const neutralWakeRecovery = "Remove the exact guard file only after the session that owns it is permanently closed and its helper or MCP child has stopped; removing a live guard can admit duplicate queue entries."
const wakeLimits = "Configured idle wake allows one automatic wake per attention identity, with one shared retry after a known queue failure. Retained legacy attempts count toward that same allowance. Outstanding queue guards have no expiry. Existing doctor --live Codex exec probes do not prove idle wake delivery."

func wakeReport(layout integrate.Layout, tokenFile string, now time.Time) []string {
	configured, enabled, err := wakeConfiguration(layout)
	var tier string
	switch {
	case err != nil:
		tier = "configured tier: unknown; Codex configuration could not be read"
	case !configured:
		tier = "idle wake off; no Codex Iris MCP entry is configured"
	case !enabled:
		tier = "configured tier: hooks; idle wake explicitly disabled"
	default:
		tier = "configured tier: hooks and idle wake (default); current arming and delivery are unverified"
	}
	lines := []string{"codex wake: " + tier}
	guards, inspectErr := inspectWakes(tokenFile)
	for _, client := range []string{"codex", "antigravity"} {
		for _, guard := range guards {
			if guard.Client == client {
				lines = append(lines, wakeGuardLine(guard, now))
			}
		}
		if client == "codex" && enabled {
			lines = append(lines, "codex wake: "+wakeLimits)
		}
	}
	for _, client := range []string{"", "unrecognized"} {
		for _, guard := range guards {
			if guard.Client == client {
				lines = append(lines, wakeGuardLine(guard, now))
			}
		}
	}
	if inspectErr != nil {
		detail := "guard inspection unavailable; retained guards may block submissions"
		if errors.Is(inspectErr, hook.ErrSessionsLocked) {
			detail = "guard inspection busy; retained guards may block submissions"
		}
		lines = append(lines, "wake guard: "+detail)
	}
	return lines
}

func wakeGuardLine(guard wake.Inspection, now time.Time) string {
	// A guard that cannot be read records no client it can be trusted for. Codex and Antigravity
	// share the runner, which waits on such a guard without submitting, so either way it blocks.
	if guard.Status != "usable" {
		state := "unusable"
		if guard.Status == "unreadable" {
			state = "unreadable"
		}
		return fmt.Sprintf("wake guard: %q: %s; client not recorded; submissions remain blocked. %s", guard.Path, state, neutralWakeRecovery)
	}
	prefix, recovery, clientNote := "wake guard:", neutralWakeRecovery, "; client not recorded"
	switch guard.Client {
	case "codex":
		prefix, recovery, clientNote = "codex wake: guard", wakeRecovery, ""
	case "antigravity":
		prefix, recovery, clientNote = "antigravity wake: guard", antigravityWakeRecovery, ""
	case "unrecognized":
		clientNote = "; client not recognized"
	}
	state := "no attempts"
	if guard.Kind != "" {
		switch {
		case guard.ConsumedMS > 0:
			state = "consumed queue wake; its automatic wake allowance remains spent"
		case guard.Outstanding:
			age := now.Sub(time.UnixMilli(guard.StartedMS))
			if age < 0 {
				age = 0
			}
			label := "outstanding"
			if age >= 10*time.Minute {
				label = "old outstanding"
			}
			state = fmt.Sprintf("%s queue wake (%s), age %s", label, guard.Outcome, age.Round(time.Second))
		case guard.Kind == "binary_check" && guard.Outcome == "failed":
			state = "known binary check failure; bounded retry accounting retained"
		default:
			state = fmt.Sprintf("%s attempt (%s); bounded accounting retained", guard.Kind, guard.Outcome)
		}
	}
	line := fmt.Sprintf("%s %q: %s%s; retained attempts %d/5; initial accounting %s, retained legacy accounting %s; automatic wake allowance %s", prefix, guard.Path, state, clientNote, guard.Attempts, wakeAccounting(guard.Slot0), wakeAccounting(guard.Slot1), wakeAllowance(min(2, guard.Slot0+guard.Slot1)))
	if guard.CheckedMS > 0 {
		line += fmt.Sprintf("; last binary check %s, path %q, binary version %q, client version %q", time.UnixMilli(guard.CheckedMS).UTC().Format(time.RFC3339), guard.BinaryPath, guard.BinaryVersion, guard.ClientVersion)
	}
	if guard.FailureMS > 0 {
		reason := guard.FailureReason
		if reason == "" {
			reason = "Unrecognized wake failure"
		}
		line += "; last recorded failure " + time.UnixMilli(guard.FailureMS).UTC().Format(time.RFC3339) + ": " + reason
	}
	if guard.Outstanding {
		line += ". " + recovery
	}
	return line
}

func wakeAccounting(state int) string {
	switch state {
	case 0:
		return "unused"
	case 1:
		return "one known failed attempt"
	default:
		return "spent"
	}
}

func wakeAllowance(state int) string {
	switch state {
	case 0:
		return "unused"
	case 1:
		return "has one retry"
	default:
		return "spent"
	}
}

package doctor

import (
	"fmt"
	"time"

	"github.com/mateusz-klatt/iris/internal/integrate"
	"github.com/mateusz-klatt/iris/internal/kimiwake"
)

// kimiWakeRoot is where Kimi's wake state lives. Tests replace it.
var kimiWakeRoot = integrate.KimiWakeRoot

// kimiWakeReport is Kimi's wake preference and its watcher records (ADR 030): what is configured
// and what each session's record says now. It reads local files only and proves no delivery.
func kimiWakeReport(layout integrate.Layout, now time.Time) []string {
	root, err := kimiWakeRoot(layout)
	if err != nil {
		return []string{"preference unknown: the OS account's Iris directory could not be found"}
	}
	const asks = "a user prompt of an interactive session asks the model to start `iris watch --client kimi --once`; Iris cannot verify the command it runs"
	var lines []string
	switch choice, err := kimiwake.Preference(root); {
	case err != nil:
		lines = append(lines, "preference unreadable, so wake is off; `iris integrate kimi --wake` or `--no-wake` rewrites it")
	case choice == "":
		lines = append(lines, "not integrated, so wake is off")
	case choice == kimiwake.PreferenceOff:
		lines = append(lines, "preference off")
	case choice == kimiwake.PreferenceDefault && !kimiwake.DefaultOn:
		lines = append(lines, "preference default: off on this platform until Kimi's native acceptance passes here (ADR 030)")
	case choice == kimiwake.PreferenceDefault:
		lines = append(lines, "preference default, on here after Kimi's native acceptance (ADR 030): "+asks)
	default:
		lines = append(lines, "preference on: "+asks)
	}
	entries, err := (&kimiwake.Store{Root: root}).Records()
	if err != nil {
		return append(lines, "watcher records unreadable")
	}
	for _, e := range entries {
		lines = append(lines, kimiWakeLine(e, now))
	}
	return lines
}

// kimiWakeLine is one session's record.
func kimiWakeLine(e kimiwake.Entry, now time.Time) string {
	head := fmt.Sprintf("session %s of kimi pid %d", e.Key.Session, e.Key.PID)
	if !e.KimiAlive {
		head += ", which has ended"
	}
	ago := func(ms int64) time.Duration { return now.Sub(time.UnixMilli(ms)).Round(time.Second) }
	switch r := e.Record; {
	case e.Unreadable != nil:
		return head + ": record unreadable"
	case e.Watching:
		return fmt.Sprintf("%s: watcher pid %d running", head, r.Attempt.PID)
	case r.Latch != nil:
		return fmt.Sprintf("%s: latched %s %s ago; SessionStart or a watcher started by hand clears it", head, r.Latch.State, ago(r.Latch.AtMS))
	case r.ArmRequestedMS != 0:
		return fmt.Sprintf("%s: watcher requested %s ago, not started yet", head, ago(r.ArmRequestedMS))
	default:
		return head + ": no watcher"
	}
}

package doctor

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/mateusz-klatt/iris/internal/integrate"
)

// Measured versions establish the tested baseline, not a compatibility promise.
var measuredVersions = map[string]string{
	integrate.Claude:      "2.1.318",
	integrate.Codex:       "0.166.0",
	integrate.Copilot:     "1.0.105",
	integrate.Cursor:      "2026.11.05",
	integrate.Antigravity: "1.5.1",
	integrate.Grok:        "1.0.77",
	integrate.Kimi:        "2.5.1",
}
var versionPattern = regexp.MustCompile(`\b([0-9]+)\.([0-9]+)\.([0-9]+)\b`)

func parsedVersion(text string) ([3]uint64, bool) {
	var version [3]uint64
	parts := versionPattern.FindStringSubmatch(text)
	if len(parts) != 4 {
		return version, false
	}
	for i := range version {
		n, err := strconv.ParseUint(parts[i+1], 10, 64)
		if err != nil {
			return version, false
		}
		version[i] = n
	}
	return version, true
}
func legacyKimi(row ClientReport) bool {
	version, ok := parsedVersion(row.Version)
	return row.Client == integrate.Kimi && ok && version[0] < 2
}
func applyVersion(row *ClientReport) {
	if legacyKimi(*row) {
		row.Status = StatusNeedsHuman
		row.Detail = "legacy Kimi CLI; Iris needs Kimi Code 2 or later"
		return
	}
	version, ok := parsedVersion(row.Version)
	if !ok {
		return
	}
	tested := measuredVersions[row.Client]
	baseline, ok := parsedVersion(tested)
	if !ok {
		return
	}
	for i := range version {
		if version[i] > baseline[i] {
			return
		}
		if version[i] < baseline[i] {
			row.Detail = strings.TrimSpace(row.Detail)
			if row.Detail != "" {
				row.Detail += "; "
			}
			row.Detail += "older than the tested " + tested
			return
		}
	}
}

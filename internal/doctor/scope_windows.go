package doctor

import "time"

// scopeReport is empty on Windows, where hooks keep no breaker and always run full scope
// (ADR 018).
func scopeReport(string, time.Time) []string { return nil }

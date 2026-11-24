package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mateusz-klatt/iris/internal/hook"
)

const (
	endMarkerScanEntryBudget  = 4096
	endMarkerScanTimeBudget   = time.Second
	endMarkerScanReadDirChunk = 64
)

type endMarkerScanConfig struct {
	entryLimit int
	timeout    time.Duration
}

func defaultEndMarkerScanConfig() endMarkerScanConfig {
	return endMarkerScanConfig{entryLimit: endMarkerScanEntryBudget, timeout: endMarkerScanTimeBudget}
}

var (
	endMarkerLstat          = os.Lstat
	endMarkerStat           = os.Stat
	endMarkerOpen           = openEndMarkerDirectory
	endMarkerReadGeneration = readEndMarkerGeneration
	endMarkerScanFlight     = make(chan struct{}, 1)
)

type endMarkerDirectory interface {
	ReadDir(int) ([]os.DirEntry, error)
	Close() error
}

func openEndMarkerDirectory(path string) (endMarkerDirectory, error) {
	return os.Open(path)
}

type endMarkerScanSnapshot struct {
	damaged    []string
	states     map[string]string
	examined   int
	incomplete string
}

func (s endMarkerScanSnapshot) note() string {
	if s.incomplete == "" {
		return ""
	}
	return fmt.Sprintf("end-marker scan incomplete (%s): examined %d entries; found %d damaged end marker(s) so far, and more may be damaged", s.incomplete, s.examined, len(s.damaged))
}

type endMarkerScanProgress struct {
	mu   sync.Mutex
	data endMarkerScanSnapshot
}

func newEndMarkerScanProgress() *endMarkerScanProgress {
	return &endMarkerScanProgress{data: endMarkerScanSnapshot{states: map[string]string{}}}
}

func (p *endMarkerScanProgress) snapshot(reason string) endMarkerScanSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.data
	out.damaged = append([]string(nil), p.data.damaged...)
	out.states = make(map[string]string, len(p.data.states))
	for path, state := range p.data.states {
		out.states[path] = state
	}
	if out.incomplete == "" {
		out.incomplete = reason
	}
	return out
}

func (p *endMarkerScanProgress) examine() {
	p.mu.Lock()
	p.data.examined++
	p.mu.Unlock()
}

func (p *endMarkerScanProgress) damage(path string) {
	p.mu.Lock()
	p.data.damaged = append(p.data.damaged, path)
	p.data.states[path] = "still damaged"
	p.mu.Unlock()
}

func (p *endMarkerScanProgress) state(path, state string) {
	p.mu.Lock()
	p.data.states[path] = state
	p.mu.Unlock()
}

func (p *endMarkerScanProgress) stop(reason string) {
	p.mu.Lock()
	p.data.incomplete = reason
	p.mu.Unlock()
}

func (p *endMarkerScanProgress) damageSet() map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]bool, len(p.data.damaged))
	for _, path := range p.data.damaged {
		out[path] = true
	}
	return out
}

func endMarkerScanWait(ctx context.Context, budget time.Duration) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < budget {
			return remaining
		}
	}
	return budget
}

func endMarkerScanTimeoutReason(ctx context.Context) string {
	if ctx.Err() != nil {
		return "context cancelled"
	}
	return "time budget exceeded"
}

// scanDamagedEndMarkers abandons blocked I/O after one outer deadline. The worker is read-only
// and publishes snapshots, so a late result cannot mutate the caller's report.
func scanDamagedEndMarkers(ctx context.Context, tokenFile, fallbackTokenFile string, observed []string, config endMarkerScanConfig) endMarkerScanSnapshot {
	if ctx.Err() != nil {
		return endMarkerScanSnapshot{states: map[string]string{}, incomplete: "context cancelled"}
	}
	select {
	case endMarkerScanFlight <- struct{}{}:
	default:
		return endMarkerScanSnapshot{states: map[string]string{}, incomplete: "previous end-marker scan still running"}
	}
	progress := newEndMarkerScanProgress()
	done := make(chan endMarkerScanSnapshot, 1)
	go func() {
		runEndMarkerScan(tokenFile, fallbackTokenFile, observed, config.entryLimit, progress)
		result := progress.snapshot("")
		<-endMarkerScanFlight
		done <- result
	}()
	timer := time.NewTimer(endMarkerScanWait(ctx, config.timeout))
	defer timer.Stop()
	select {
	case result := <-done:
		return result
	case <-timer.C:
		return progress.snapshot(endMarkerScanTimeoutReason(ctx))
	case <-ctx.Done():
		return progress.snapshot("context cancelled")
	}
}

func runEndMarkerScan(tokenFile, fallbackTokenFile string, observed []string, entryLimit int, progress *endMarkerScanProgress) {
	scanner := endMarkerScanner{entryLimit: entryLimit, entriesLeft: entryLimit, progress: progress}
	for _, dir := range endMarkerSessionDirs(tokenFile, fallbackTokenFile) {
		if reason := scanner.readDirectory(dir); reason != "" {
			progress.stop(reason)
		}
		if scanner.incomplete != "" {
			progress.stop(scanner.incomplete)
			break
		}
	}
	current := progress.damageSet()
	for _, path := range observed {
		if current[path] {
			progress.state(path, "still damaged")
			continue
		}
		progress.state(path, markerDamageStatus(path))
	}
}

type endMarkerScanner struct {
	entryLimit  int
	entriesLeft int
	incomplete  string
	progress    *endMarkerScanProgress
}

func (s *endMarkerScanner) readDirectory(dir string) string {
	f, err := endMarkerOpen(dir)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		return "could not scan for damaged end markers: " + err.Error()
	}
	defer func() { _ = f.Close() }()
	for {
		entries, readErr := f.ReadDir(min(endMarkerScanReadDirChunk, s.entriesLeft+1))
		if len(entries) > s.entriesLeft {
			s.incomplete = fmt.Sprintf("entry budget of %d exceeded", s.entryLimit)
			return ""
		}
		for _, entry := range entries {
			s.entriesLeft--
			s.progress.examine()
			if !strings.HasSuffix(strings.ToLower(entry.Name()), ".ended") {
				continue
			}
			path, canonical := canonicalEndMarkerPath(dir, entry.Name())
			if !canonical {
				continue
			}
			switch endMarkerPathClass(path) {
			case endMarkerFileAbsent:
				continue
			case endMarkerFileDamaged:
				s.progress.damage(path)
				continue
			}
			if _, err := endMarkerReadGeneration(path); err != nil {
				s.progress.damage(path)
			}
		}
		if errors.Is(readErr, io.EOF) {
			return ""
		}
		if readErr != nil {
			return "could not scan for damaged end markers: " + readErr.Error()
		}
	}
}

func readEndMarkerGeneration(path string) (int, error) {
	return hook.ReadGeneration(path)
}

func canonicalEndMarkerPath(dir, name string) (string, bool) {
	canonicalName := strings.ToLower(name)
	if name == canonicalName {
		return filepath.Join(dir, name), true
	}
	path := filepath.Join(dir, name)
	canonicalPath := filepath.Join(dir, canonicalName)
	pathInfo, err := endMarkerStat(path)
	if err != nil {
		return "", false
	}
	canonicalInfo, err := endMarkerStat(canonicalPath)
	if err != nil || !os.SameFile(pathInfo, canonicalInfo) {
		return "", false
	}
	return canonicalPath, true
}

type endMarkerFileClass uint8

const (
	endMarkerFileAbsent endMarkerFileClass = iota
	endMarkerFileRegular
	endMarkerFileDamaged
)

// endMarkerRegularFile classifies metadata without filesystem I/O. A nil target is a dangling link.
func endMarkerRegularFile(entry, target fs.FileInfo) bool {
	if entry == nil {
		return false
	}
	if entry.Mode()&fs.ModeSymlink != 0 {
		return target != nil && target.Mode().IsRegular()
	}
	return entry.Mode().IsRegular()
}

func endMarkerPathClass(path string) endMarkerFileClass {
	info, err := endMarkerLstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return endMarkerFileAbsent
	}
	if err != nil {
		return endMarkerFileDamaged
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		if endMarkerRegularFile(info, nil) {
			return endMarkerFileRegular
		}
		return endMarkerFileDamaged
	}
	target, targetErr := endMarkerStat(path)
	if errors.Is(targetErr, fs.ErrNotExist) {
		if _, linkErr := endMarkerLstat(path); errors.Is(linkErr, fs.ErrNotExist) {
			return endMarkerFileAbsent
		}
	} else if targetErr == nil && endMarkerRegularFile(info, target) {
		return endMarkerFileRegular
	}
	return endMarkerFileDamaged
}

// markerDamageStatus rechecks a named marker without publishing its path or contents.
func markerDamageStatus(path string) string {
	switch endMarkerPathClass(path) {
	case endMarkerFileAbsent:
		return "archived since"
	case endMarkerFileDamaged:
		return "still damaged"
	}
	generation, err := endMarkerReadGeneration(path)
	if err != nil {
		return "still damaged"
	}
	if generation > 0 {
		return "repaired since"
	}
	return "archived since"
}

func endMarkerSessionDirs(tokenFile, fallbackTokenFile string) []string {
	var dirs []string
	var infos []fs.FileInfo
	seen := map[string]struct{}{}
	for _, token := range []string{tokenFile, fallbackTokenFile} {
		if strings.TrimSpace(token) == "" {
			continue
		}
		dir := hook.NewSessions(token).Dir()
		absolute, _ := filepath.Abs(dir) // Abs fails only when the working directory is gone.
		key := filepath.Clean(absolute)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		info, err := endMarkerStat(dir)
		duplicate := false
		if err == nil {
			for i := range dirs {
				if infos[i] != nil && os.SameFile(info, infos[i]) {
					duplicate = true
					break
				}
			}
		}
		if duplicate {
			continue
		}
		dirs = append(dirs, dir)
		infos = append(infos, info)
	}
	return dirs
}

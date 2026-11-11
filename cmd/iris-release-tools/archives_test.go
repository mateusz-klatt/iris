package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"debug/pe"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func syntheticPE(machine uint16) []byte {
	image := make([]byte, 152)
	copy(image, "MZ")
	binary.LittleEndian.PutUint32(image[60:], 128)
	copy(image[128:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(image[132:], machine)
	return image
}

type zipMember struct {
	name    string
	content []byte
	mode    os.FileMode
	method  uint16
	badCRC  bool
}

func syntheticZIP(t *testing.T, members []zipMember) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for _, member := range members {
		header := &zip.FileHeader{Name: member.name, Method: member.method}
		mode := member.mode
		if mode == 0 {
			mode = 0600
		}
		header.SetMode(mode)
		var out io.Writer
		var err error
		if member.badCRC || member.method == 99 {
			header.CRC32 = 1
			header.CompressedSize64, header.UncompressedSize64 = uint64(len(member.content)), uint64(len(member.content))
			out, err = writer.CreateRaw(header)
		} else {
			out, err = writer.CreateHeader(header)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := out.Write(member.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func archiveMembers(machine uint16) []zipMember {
	return []zipMember{{name: "iris.exe", content: syntheticPE(machine)}, {name: "LICENSE", content: []byte("license")}, {name: "docs/guard.md", content: []byte("guard")}}
}

func handoffFixture(t *testing.T, amd64, arm64 []byte, manifest func(string) string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	var sums strings.Builder
	for i, data := range [][]byte{amd64, arm64} {
		arch := []string{"amd64", "arm64"}[i]
		name := "iris_1.2.3-rc.1_windows_" + arch + ".zip"
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(data), name)
	}
	// The untouched release manifest also contains artifacts outside this handoff.
	fmt.Fprintf(&sums, "%064x  iris_1.2.3-rc.1_linux_amd64.tar.gz\n", 1)
	text := sums.String()
	if manifest != nil {
		text = manifest(text)
	}
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return dir, filepath.Join(t.TempDir(), "extracted")
}

func validHandoff(t *testing.T) (string, string) {
	t.Helper()
	return handoffFixture(t, syntheticZIP(t, archiveMembers(pe.IMAGE_FILE_MACHINE_AMD64)), syntheticZIP(t, archiveMembers(pe.IMAGE_FILE_MACHINE_ARM64)), nil)
}

func TestVerifyWindowsArchivesEvidenceAndFixedExtraction(t *testing.T) {
	dir, dest := validHandoff(t)
	var out, stderr bytes.Buffer
	status := dispatch([]string{"verify-windows-archives", "--version=1.2.3-rc.1", "--dir=" + dir, "--out=" + dest}, func(string) string { return "" }, &out, &stderr)
	if status != 0 || stderr.Len() != 0 {
		t.Fatalf("verify status %d: %s", status, stderr.String())
	}
	for i, arch := range []string{"amd64", "arm64"} {
		machine := []uint16{pe.IMAGE_FILE_MACHINE_AMD64, pe.IMAGE_FILE_MACHINE_ARM64}[i]
		data, err := os.ReadFile(filepath.Join(dest, arch, "iris.exe"))
		if err != nil || !bytes.Equal(data, syntheticPE(machine)) {
			t.Fatalf("extracted %s: %v", arch, err)
		}
		entries, err := os.ReadDir(filepath.Join(dest, arch))
		if err != nil || len(entries) != 1 {
			t.Fatalf("unexpected extracted files: %v", err)
		}
		zipData, err := os.ReadFile(filepath.Join(dir, "iris_1.2.3-rc.1_windows_"+arch+".zip"))
		if err != nil {
			t.Fatal(err)
		}
		for _, hash := range []string{fmt.Sprintf("%x", sha256.Sum256(data)), fmt.Sprintf("%x", sha256.Sum256(zipData))} {
			if !strings.Contains(out.String(), hash) {
				t.Fatalf("missing %s evidence", arch)
			}
		}
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "checksums.txt"))
	if err != nil || !strings.Contains(out.String(), fmt.Sprintf("%x", sha256.Sum256(manifest))) {
		t.Fatalf("manifest evidence: %v", err)
	}
	if err := verifyWindowsArchives("1.2.3-rc.1", dir, dest, io.Discard); err == nil {
		t.Fatal("existing extraction was overwritten")
	}
	if err := verifyWindowsArchives("1.2.3-rc.1", dir, filepath.Join(t.TempDir(), "extract"), failingExtraction{}); err == nil || err.Error() != "release tools: cannot write handoff evidence" {
		t.Fatalf("summary output failure: %v", err)
	}
}

func TestManifestRejectionBeforeAnyExtraction(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(string) string
	}{
		{"empty", func(string) string { return "" }},
		{"missing hash", func(string) string { return "iris.zip\n" }},
		{"short hash", func(string) string { return "aa  iris.zip\n" }},
		{"invalid hex", func(string) string { return strings.Repeat("z", 64) + "  iris.zip\n" }},
		{"duplicate", func(s string) string { return s + strings.Split(s, "\n")[0] + "\n" }},
		{"missing expected", func(s string) string { return strings.Join(strings.Split(s, "\n")[1:], "\n") }},
		{"first mismatch", func(s string) string { return strings.Repeat("0", 64) + s[64:] }},
		{"second mismatch", func(s string) string {
			lines := strings.Split(s, "\n")
			lines[1] = strings.Repeat("0", 64) + lines[1][64:]
			return strings.Join(lines, "\n")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, dest := handoffFixture(t, syntheticZIP(t, archiveMembers(pe.IMAGE_FILE_MACHINE_AMD64)), syntheticZIP(t, archiveMembers(pe.IMAGE_FILE_MACHINE_ARM64)), tc.change)
			var out bytes.Buffer
			if err := verifyWindowsArchives("1.2.3-rc.1", dir, dest, &out); err == nil || out.Len() != 0 {
				t.Fatalf("manifest accepted: %v", err)
			}
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Fatalf("extracted before manifest passed: %v", err)
			}
		})
	}
	entries, err := checksumEntries([]byte(strings.Repeat("A", 64) + "  archive.zip\r\n"))
	if err != nil || entries["archive.zip"] != strings.Repeat("a", 64) {
		t.Fatalf("valid uppercase/CRLF manifest: %v", err)
	}
}

func TestArchiveMemberPolicyAndMachine(t *testing.T) {
	base := archiveMembers(pe.IMAGE_FILE_MACHINE_AMD64)
	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"corrupt ZIP", []byte("not a zip"), "invalid Windows ZIP"},
		{"duplicate", syntheticZIP(t, append(append([]zipMember{}, base...), base[0])), "invalid Windows ZIP member"},
		{"traversal", syntheticZIP(t, append(append([]zipMember{}, base...), zipMember{name: "../escape", content: []byte("bad")})), "invalid Windows ZIP member"},
		{"absolute", syntheticZIP(t, []zipMember{{name: "/iris.exe"}}), "invalid Windows ZIP member"},
		{"backslash", syntheticZIP(t, []zipMember{{name: `docs\guard.md`}}), "invalid Windows ZIP member"},
		{"drive path", syntheticZIP(t, []zipMember{{name: `C:/iris.exe`}}), "invalid Windows ZIP member"},
		{"unexpected", syntheticZIP(t, []zipMember{{name: "extra"}}), "invalid Windows ZIP member"},
		{"symlink", syntheticZIP(t, []zipMember{{name: "iris.exe", mode: os.ModeSymlink | 0700}}), "invalid Windows ZIP member"},
		{"directory", syntheticZIP(t, []zipMember{{name: "docs/", mode: os.ModeDir | 0700}}), "invalid Windows ZIP member"},
		{"unsupported compression", syntheticZIP(t, []zipMember{{name: "iris.exe", content: []byte("raw"), method: 99}}), "unreadable Windows ZIP member"},
		{"bad CRC", syntheticZIP(t, []zipMember{{name: "iris.exe", content: []byte("raw"), badCRC: true}}), "corrupt Windows ZIP member"},
		{"empty", syntheticZIP(t, nil), "incomplete Windows ZIP"},
		{"missing license", syntheticZIP(t, []zipMember{base[0], base[2]}), "incomplete Windows ZIP"},
		{"missing guard", syntheticZIP(t, base[:2]), "incomplete Windows ZIP"},
		{"invalid PE", syntheticZIP(t, []zipMember{{name: "iris.exe", content: []byte("not PE")}, base[1], base[2]}), "invalid Windows executable"},
		{"wrong machine", syntheticZIP(t, archiveMembers(pe.IMAGE_FILE_MACHINE_ARM64)), "wrong Windows executable architecture"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remaining := int64(maxTotalBytes)
			_, err := archiveExecutable(tc.data, "amd64", maxMemberBytes, &remaining)
			if err == nil || err.Error() != "release tools: "+tc.want {
				t.Fatalf("got %v, expected %s", err, tc.want)
			}
		})
	}
	dir, dest := handoffFixture(t, syntheticZIP(t, base), []byte("bad second ZIP"), nil)
	if err := verifyWindowsArchives("1.2.3-rc.1", dir, dest, io.Discard); err == nil {
		t.Fatal("corrupt second archive accepted")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("extracted before both archive policies passed")
	}
}

func TestHandoffConfigurationAndFileSet(t *testing.T) {
	dir, dest := validHandoff(t)
	for _, tc := range []struct{ version, dir, dest string }{
		{"", dir, dest}, {"../escape", dir, dest}, {"1\n2", dir, dest}, {"1.2.3", "", dest}, {"1.2.3", dir, ""}, {"1.2.3", filepath.Join(dir, "absent"), dest},
	} {
		if err := verifyWindowsArchives(tc.version, tc.dir, tc.dest, io.Discard); err == nil {
			t.Fatalf("invalid configuration accepted: %v", tc)
		}
	}
	for _, name := range []string{"extra.txt", "subdir"} {
		t.Run(name, func(t *testing.T) {
			dir, dest := validHandoff(t)
			if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := verifyWindowsArchives("1.2.3-rc.1", dir, dest, io.Discard); err == nil {
				t.Fatal("extra handoff accepted")
			}
		})
	}
	wrongDir := t.TempDir()
	for _, name := range []string{"checksums.txt", "iris_1.2.3-rc.1_windows_amd64.zip", "unexpected.zip"} {
		if err := os.WriteFile(filepath.Join(wrongDir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := verifyWindowsArchives("1.2.3-rc.1", wrongDir, dest, io.Discard); err == nil {
		t.Fatal("wrong handoff name accepted")
	}
	directoryMember := t.TempDir()
	for _, name := range []string{"checksums.txt", "iris_1.2.3-rc.1_windows_amd64.zip", "iris_1.2.3-rc.1_windows_arm64.zip"} {
		if err := os.Mkdir(filepath.Join(directoryMember, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := verifyWindowsArchives("1.2.3-rc.1", directoryMember, dest, io.Discard); err == nil {
		t.Fatal("directory handoff accepted")
	}
}

type failingExtraction struct{}

func (failingExtraction) Write([]byte) (int, error) {
	return 0, fmt.Errorf("synthetic private storage error")
}
func (failingExtraction) Close() error { return nil }

func TestHandoffStorageFailuresAreSanitized(t *testing.T) {
	dir, dest := validHandoff(t)
	oldOpen := openHandoff
	t.Cleanup(func() { openHandoff = oldOpen })
	for _, name := range []string{"checksums.txt", "iris_1.2.3-rc.1_windows_arm64.zip"} {
		openHandoff = func(path string) (io.ReadCloser, error) {
			if filepath.Base(path) == name {
				return nil, fmt.Errorf("synthetic private read error")
			}
			return os.Open(path)
		}
		err := verifyWindowsArchives("1.2.3-rc.1", dir, dest, io.Discard)
		if err == nil || strings.Contains(err.Error(), "private") {
			t.Fatalf("read failure: %v", err)
		}
	}
	openHandoff = oldOpen
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyWindowsArchives("1.2.3-rc.1", dir, blocked, io.Discard); err == nil || err.Error() != "release tools: cannot create extraction directory" {
		t.Fatalf("directory failure: %v", err)
	}
	oldCreate := createExtracted
	t.Cleanup(func() { createExtracted = oldCreate })
	createExtracted = func(string) (io.WriteCloser, error) { return nil, fmt.Errorf("synthetic private create error") }
	if err := verifyWindowsArchives("1.2.3-rc.1", dir, dest, io.Discard); err == nil || err.Error() != "release tools: cannot create extracted executable" {
		t.Fatalf("create failure: %v", err)
	}
	createExtracted = func(string) (io.WriteCloser, error) { return failingExtraction{}, nil }
	if err := verifyWindowsArchives("1.2.3-rc.1", dir, filepath.Join(t.TempDir(), "extract"), io.Discard); err == nil || err.Error() != "release tools: cannot write extracted executable" {
		t.Fatalf("write failure: %v", err)
	}
}

func declaredMemberSize(t *testing.T, data []byte, size uint32) []byte {
	t.Helper()
	modified := append([]byte(nil), data...)
	central := bytes.Index(modified, []byte("PK\x01\x02"))
	if central < 0 {
		t.Fatal("missing fixture ZIP directory")
	}
	binary.LittleEndian.PutUint32(modified[central+24:], size)
	return modified
}

func TestHandoffSizeLimitsBeforeExtraction(t *testing.T) {
	amd64 := syntheticZIP(t, archiveMembers(pe.IMAGE_FILE_MACHINE_AMD64))
	arm64 := syntheticZIP(t, archiveMembers(pe.IMAGE_FILE_MACHINE_ARM64))
	dir, dest := handoffFixture(t, amd64, arm64, nil)
	manifest, err := os.ReadFile(filepath.Join(dir, "checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// The configured archive consists of the executable, LICENSE and docs/guard.md.
	// Tiny test budgets exercise exact boundaries without allocating production limits.
	limits := archiveLimits{manifest: int64(len(manifest)), zip: int64(max(len(amd64), len(arm64))), member: int64(len(syntheticPE(pe.IMAGE_FILE_MACHINE_AMD64))), total: 2 * (152 + 7 + 5)}
	if err := verifyWindowsArchivesLimited("1.2.3-rc.1", dir, dest, io.Discard, limits); err != nil {
		t.Fatalf("exact size boundaries rejected: %v", err)
	}
	for _, tc := range []struct {
		name, want string
		change     func(*archiveLimits)
	}{
		{"manifest", "cannot read checksum manifest", func(l *archiveLimits) { l.manifest-- }},
		{"ZIP", "cannot read Windows archive", func(l *archiveLimits) { l.zip-- }},
		{"member", "Windows ZIP exceeds decompression limits", func(l *archiveLimits) { l.member-- }},
		{"total across both ZIPs", "Windows ZIP exceeds decompression limits", func(l *archiveLimits) { l.total-- }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testLimits := limits
			tc.change(&testLimits)
			dest := filepath.Join(t.TempDir(), "rejected")
			var summary bytes.Buffer
			err := verifyWindowsArchivesLimited("1.2.3-rc.1", dir, dest, &summary, testLimits)
			if err == nil || err.Error() != "release tools: "+tc.want || summary.Len() != 0 {
				t.Fatalf("oversized %s result: %v, summary %q", tc.name, err, summary.String())
			}
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Fatalf("extracted oversized handoff: %v", err)
			}
		})
	}
	for _, tc := range []struct {
		name     string
		declared uint32
		want     string
	}{
		{"oversized declared header", uint32(maxMemberBytes + 1), "Windows ZIP exceeds decompression limits"},
		{"dishonest small header", 1, "corrupt Windows ZIP member"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, dest := handoffFixture(t, declaredMemberSize(t, amd64, tc.declared), arm64, nil)
			err := verifyWindowsArchives("1.2.3-rc.1", dir, dest, io.Discard)
			if err == nil || err.Error() != "release tools: "+tc.want {
				t.Fatalf("header accepted: %v", err)
			}
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Fatal("extracted handoff with dishonest header")
			}
		})
	}
}

type failingHandoffRead struct{}

func (failingHandoffRead) Read([]byte) (int, error) {
	return 0, fmt.Errorf("synthetic private disk error")
}

func TestBoundedReadsCheckActualBytesAndSanitizeReadErrors(t *testing.T) {
	// The same bounded reader serves files and decompressed members. It must read
	// at most limit+1 actual bytes, regardless of a source's claimed size.
	for _, limit := range []int64{0, 1, 8} {
		reader := strings.NewReader(strings.Repeat("x", 1024))
		data, err := readLimited(reader, limit)
		if err == nil || data != nil || 1024-reader.Len() != int(limit+1) {
			t.Fatalf("unbounded read at %d: data %d, remaining %d, error %v", limit, len(data), reader.Len(), err)
		}
	}
	dir, dest := validHandoff(t)
	oldOpen := openHandoff
	t.Cleanup(func() { openHandoff = oldOpen })
	openHandoff = func(string) (io.ReadCloser, error) { return io.NopCloser(failingHandoffRead{}), nil }
	if err := verifyWindowsArchives("1.2.3-rc.1", dir, dest, io.Discard); err == nil || err.Error() != "release tools: cannot read checksum manifest" {
		t.Fatalf("unsanitized read failure: %v", err)
	}
}

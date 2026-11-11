package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"debug/pe"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var releaseVersion = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+-]*$`)

// Bound artifact reads and decompression, with the total shared by both archives.
// The configured archives each contain iris.exe, LICENSE and docs/guard.md.
const (
	maxManifestBytes = 1 << 20
	maxZIPBytes      = 128 << 20
	maxMemberBytes   = 128 << 20
	maxTotalBytes    = 256 << 20
)

type archiveLimits struct {
	manifest, zip, member, total int64
}

// Filesystem boundaries also exercise storage failures without changing host permissions.
var openHandoff = func(path string) (io.ReadCloser, error) { return os.Open(path) }
var createExtracted = func(path string) (io.WriteCloser, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
}

type windowsArchive struct {
	arch, name string
	zipHash    [sha256.Size]byte
	exe        []byte
}

func verifyWindowsArchives(version, dir, dest string, out io.Writer) error {
	return verifyWindowsArchivesLimited(version, dir, dest, out, archiveLimits{
		manifest: maxManifestBytes, zip: maxZIPBytes, member: maxMemberBytes, total: maxTotalBytes,
	})
}

func verifyWindowsArchivesLimited(version, dir, dest string, out io.Writer, limits archiveLimits) error {
	if !releaseVersion.MatchString(version) || dir == "" || dest == "" {
		return fmt.Errorf("release tools: invalid archive configuration")
	}
	archives := []windowsArchive{
		{arch: "amd64", name: "iris_" + version + "_windows_amd64.zip"},
		{arch: "arm64", name: "iris_" + version + "_windows_arm64.zip"},
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 3 {
		return fmt.Errorf("release tools: invalid handoff file set")
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || (entry.Name() != "checksums.txt" && entry.Name() != archives[0].name && entry.Name() != archives[1].name) {
			return fmt.Errorf("release tools: invalid handoff file set")
		}
	}
	manifest, err := readHandoff(filepath.Join(dir, "checksums.txt"), limits.manifest)
	if err != nil {
		return fmt.Errorf("release tools: cannot read checksum manifest")
	}
	hashes, err := checksumEntries(manifest)
	if err != nil {
		return err
	}
	// Validate both ZIP digests before opening or extracting either archive.
	zipData := make([][]byte, len(archives))
	for i := range archives {
		data, err := readHandoff(filepath.Join(dir, archives[i].name), limits.zip)
		if err != nil {
			return fmt.Errorf("release tools: cannot read Windows archive")
		}
		archives[i].zipHash = sha256.Sum256(data)
		if hashes[archives[i].name] != hex.EncodeToString(archives[i].zipHash[:]) {
			return fmt.Errorf("release tools: Windows archive checksum mismatch")
		}
		zipData[i] = data
	}
	remaining := limits.total
	for i := range archives {
		archives[i].exe, err = archiveExecutable(zipData[i], archives[i].arch, limits.member, &remaining)
		if err != nil {
			return err
		}
	}
	for _, archive := range archives {
		path := filepath.Join(dest, archive.arch)
		if os.MkdirAll(dest, 0700) != nil || os.Mkdir(path, 0700) != nil {
			return fmt.Errorf("release tools: cannot create extraction directory")
		}
		file, err := createExtracted(filepath.Join(path, "iris.exe"))
		if err != nil {
			return fmt.Errorf("release tools: cannot create extracted executable")
		}
		_, err = file.Write(archive.exe)
		_ = file.Close()
		if err != nil {
			return fmt.Errorf("release tools: cannot write extracted executable")
		}
	}
	var summary strings.Builder
	_, _ = fmt.Fprintf(&summary, "Windows handoff SHA-256 evidence\n\nchecksums.txt: `%x`\n\n", sha256.Sum256(manifest))
	for _, archive := range archives {
		_, _ = fmt.Fprintf(&summary, "%s: `%x`\n\n%s/iris.exe: `%x`\n\n", archive.name, archive.zipHash, archive.arch, sha256.Sum256(archive.exe))
	}
	if _, err := io.WriteString(out, summary.String()); err != nil {
		return fmt.Errorf("release tools: cannot write handoff evidence")
	}
	return nil
}

func readHandoff(path string, limit int64) ([]byte, error) {
	file, err := openHandoff(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return readLimited(file, limit)
}

// The extra byte detects excess without trusting file metadata or ZIP size headers.
func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("release tools: input exceeds size limit")
	}
	return data, err
}

func checksumEntries(manifest []byte) (map[string]string, error) {
	hashes := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(manifest)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != 64 {
			return nil, fmt.Errorf("release tools: malformed checksum manifest")
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			return nil, fmt.Errorf("release tools: malformed checksum manifest")
		}
		if _, exists := hashes[fields[1]]; exists {
			return nil, fmt.Errorf("release tools: duplicate checksum entry")
		}
		hashes[fields[1]] = strings.ToLower(fields[0])
	}
	return hashes, nil
}

func archiveExecutable(data []byte, arch string, memberLimit int64, remaining *int64) ([]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("release tools: invalid Windows ZIP")
	}
	seen := make(map[string]bool)
	var executable []byte
	for _, file := range reader.File {
		if seen[file.Name] || !file.Mode().IsRegular() || (file.Name != "iris.exe" && file.Name != "LICENSE" && file.Name != "docs/guard.md") {
			return nil, fmt.Errorf("release tools: invalid Windows ZIP member")
		}
		seen[file.Name] = true
		if file.UncompressedSize64 > uint64(memberLimit) || file.UncompressedSize64 > uint64(*remaining) {
			return nil, fmt.Errorf("release tools: Windows ZIP exceeds decompression limits")
		}
		member, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("release tools: unreadable Windows ZIP member")
		}
		content, err := readLimited(member, min(memberLimit, *remaining))
		_ = member.Close()
		if err != nil {
			return nil, fmt.Errorf("release tools: corrupt Windows ZIP member")
		}
		*remaining -= int64(len(content))
		if file.Name == "iris.exe" {
			executable = content
		}
	}
	if !seen["iris.exe"] || !seen["LICENSE"] || !seen["docs/guard.md"] {
		return nil, fmt.Errorf("release tools: incomplete Windows ZIP")
	}
	image, err := pe.NewFile(bytes.NewReader(executable))
	if err != nil {
		return nil, fmt.Errorf("release tools: invalid Windows executable")
	}
	expected := map[string]uint16{"amd64": pe.IMAGE_FILE_MACHINE_AMD64, "arm64": pe.IMAGE_FILE_MACHINE_ARM64}
	if image.Machine != expected[arch] {
		return nil, fmt.Errorf("release tools: wrong Windows executable architecture")
	}
	return executable, nil
}

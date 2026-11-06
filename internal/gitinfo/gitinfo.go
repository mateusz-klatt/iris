package gitinfo

import (
	"bytes"
	"compress/zlib"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrNotRepo means start is not inside a git worktree.
var ErrNotRepo = errors.New("not a git worktree")

// Repo is the identity a hook can learn without executing git.
type Repo struct {
	Worktree   string // absolute worktree root
	GitDir     string // this worktree's git directory
	CommonDir  string // shared git directory
	RemoteURL  string // origin, or the first remote
	RootCommit string // empty when the root is not cheap to read
	Branch     string // empty when HEAD is detached
}

// Find walks up from start and reads .git. A linked worktree whose .git is a
// file is followed. RootCommit is set only when a short loose-object walk
// reaches a parentless commit.
func Find(start string) (Repo, error) {
	if start == "" {
		return Repo{}, ErrNotRepo
	}
	dir, err := filepath.Abs(start)
	if err != nil {
		return Repo{}, ErrNotRepo
	}
	for {
		gitPath := filepath.Join(dir, ".git")
		info, statErr := os.Stat(gitPath)
		if statErr == nil {
			gitDir := gitPath
			if !info.IsDir() {
				gitDir, err = readGitdirFile(gitPath)
				if err != nil {
					return Repo{}, err
				}
			}
			return readRepo(dir, gitDir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return Repo{}, ErrNotRepo
		}
		dir = parent
	}
}

func readRepo(worktree, gitDir string) (Repo, error) {
	gitDir = filepath.Clean(gitDir)
	common := gitDir
	if b, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		p := strings.TrimSpace(string(b))
		if p != "" {
			if !filepath.IsAbs(p) {
				p = filepath.Join(gitDir, p)
			}
			common = filepath.Clean(p)
		}
	}
	branch, sha := readHEAD(gitDir, common)
	return Repo{
		Worktree:   worktree,
		GitDir:     gitDir,
		CommonDir:  common,
		RemoteURL:  readRemote(common),
		RootCommit: rootCommit(common, sha),
		Branch:     branch,
	}, nil
}

func readGitdirFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", ErrNotRepo
	}
	line := strings.TrimSpace(string(b))
	const prefix = "gitdir:"
	if !strings.HasPrefix(line, prefix) {
		return "", ErrNotRepo
	}
	gitDir := strings.TrimSpace(line[len(prefix):])
	if gitDir == "" {
		return "", ErrNotRepo
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(filepath.Dir(path), gitDir)
	}
	return filepath.Clean(gitDir), nil
}

func readHEAD(gitDir, common string) (branch, sha string) {
	b, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", ""
	}
	text := strings.TrimSpace(string(b))
	const refPrefix = "ref:"
	if strings.HasPrefix(text, refPrefix) {
		ref := strings.TrimSpace(text[len(refPrefix):])
		branch = strings.TrimPrefix(ref, "refs/heads/")
		return branch, resolveRef(gitDir, common, ref)
	}
	if isSHA(text) {
		return "", text
	}
	return "", ""
}

func resolveRef(gitDir, common, ref string) string {
	for _, dir := range []string{gitDir, common} {
		if b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ref))); err == nil {
			sha := strings.TrimSpace(string(b))
			if isSHA(sha) {
				return sha
			}
		}
		if sha := packedRef(filepath.Join(dir, "packed-refs"), ref); sha != "" {
			return sha
		}
	}
	return ""
}

func packedRef(path, ref string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		sha, name, ok := strings.Cut(line, " ")
		if !ok || name != ref || !isSHA(sha) {
			continue
		}
		return sha
	}
	return ""
}

func readRemote(common string) string {
	b, err := os.ReadFile(filepath.Join(common, "config"))
	if err != nil {
		return ""
	}
	var section string
	var origin, first string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(line)
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "url" {
			continue
		}
		val = unquote(strings.TrimSpace(val))
		if val == "" || !strings.Contains(section, "[remote ") {
			continue
		}
		if first == "" {
			first = val
		}
		if strings.Contains(section, `"origin"`) {
			origin = val
		}
	}
	if origin != "" {
		return origin
	}
	return first
}

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

const maxRootWalk = 128

// rootCommit follows first parents through loose objects only.
// A pack, a missing object, or a long history means the root is not cheap.
func rootCommit(common, sha string) string {
	if !isSHA(sha) {
		return ""
	}
	cur := sha
	for range maxRootWalk {
		parents, ok := looseParents(common, cur)
		if !ok {
			return ""
		}
		if len(parents) == 0 {
			return cur
		}
		cur = parents[0]
	}
	return ""
}

func looseParents(common, sha string) ([]string, bool) {
	if len(sha) < 3 {
		return nil, false
	}
	f, err := os.Open(filepath.Join(common, "objects", sha[:2], sha[2:]))
	if err != nil {
		return nil, false
	}
	defer f.Close()
	r, err := zlib.NewReader(f)
	if err != nil {
		return nil, false
	}
	defer r.Close()
	body, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return nil, false
	}
	header, rest, ok := bytes.Cut(body, []byte{0})
	if !ok || !bytes.HasPrefix(header, []byte("commit ")) {
		return nil, false
	}
	var parents []string
	for _, line := range bytes.Split(rest, []byte("\n")) {
		if len(line) == 0 {
			break
		}
		if bytes.HasPrefix(line, []byte("parent ")) {
			parents = append(parents, string(line[len("parent "):]))
		}
	}
	return parents, true
}

func isSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

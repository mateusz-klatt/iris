// Command refresh brings the repository's dependencies up to date, the way snapper's
// `make refresh` does: Go modules and web packages, the tool pins in the Makefile that
// Dependabot cannot see, every workflow action re-pinned to the full SHA of its newest release,
// and the Docker base image digests. It then runs `make test` and `make check-all` and prints
// what changed. It never commits: the person reads the summary and opens the pull request.
//
// Every lookup that fails is reported and makes the exit status 1. A refresh that could not reach
// a registry must not look like one that found nothing to update.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Sources are where refresh asks for the newest versions. Tests point them at fake servers.
type Sources struct {
	GitHub   string                   // GitHub's REST API, without a trailing slash
	GoProxy  string                   // a Go module proxy
	Registry func(host string) string // base URL of a container registry, by its host name
	Token    string                   // an optional GitHub token, for the rate limit
	HTTP     *http.Client
}

// Runner runs a command in the repository and returns its combined output.
type Runner func(dir, name string, args ...string) (string, error)

// pinnedTools maps each Makefile pin to the Go module that versions it.
var pinnedTools = []struct{ variable, module string }{
	{"GOLANGCI_LINT_VERSION", "github.com/golangci/golangci-lint/v2"},
	{"GOVULNCHECK_VERSION", "golang.org/x/vuln"},
	{"GITLEAKS_VERSION", "github.com/zricethezav/gitleaks/v8"},
}

// Report is what one refresh did.
type Report struct {
	Changes  []string
	Problems []string
	Checks   []string
}

func main() {
	src := Sources{
		GitHub:   "https://api.github.com",
		GoProxy:  firstProxy(os.Getenv("GOPROXY")),
		Registry: func(host string) string { return "https://" + host },
		Token:    os.Getenv("GITHUB_TOKEN"),
		HTTP:     &http.Client{Timeout: 30 * time.Second},
	}
	report := Refresh(".", src, run)
	os.Exit(Print(os.Stdout, report))
}

// run is the Runner for a real refresh.
func run(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// firstProxy is the first proxy GOPROXY names, or the public one.
func firstProxy(goproxy string) string {
	for _, p := range strings.FieldsFunc(goproxy, func(r rune) bool { return r == ',' || r == '|' }) {
		if p != "direct" && p != "off" {
			return strings.TrimRight(p, "/")
		}
	}
	return "https://proxy.golang.org"
}

// Refresh updates everything in the repository at root and verifies the result.
func Refresh(root string, src Sources, runner Runner) Report {
	var r Report
	r.pins(root, src)
	r.actions(root, src)
	r.images(root, src)
	r.goModules(root, runner)
	r.webPackages(root, runner)
	for _, target := range []string{"test", "check-all"} {
		if out, err := runner(root, "make", target); err != nil {
			r.Problems = append(r.Problems, fmt.Sprintf("make %s failed: %v\n%s", target, err, tail(out)))
		} else {
			r.Checks = append(r.Checks, "make "+target+": ok")
		}
	}
	return r
}

// Print writes the report and returns the exit status: 1 if anything failed.
func Print(w io.Writer, r Report) int {
	section := func(title string, lines []string) {
		fmt.Fprintf(w, "%s:\n", title)
		if len(lines) == 0 {
			fmt.Fprintln(w, "  none")
		}
		for _, l := range lines {
			fmt.Fprintf(w, "  %s\n", l)
		}
	}
	section("Changed", r.Changes)
	section("Checks", r.Checks)
	section("Problems", r.Problems)
	fmt.Fprintln(w, "Nothing was committed. Review the diff and open a pull request. A major action bump is proven only by CI, and pushing workflow changes needs a token with the workflow scope.")
	if len(r.Problems) > 0 {
		return 1
	}
	return 0
}

func tail(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return strings.Join(lines, "\n")
}

// edit rewrites a file through change, and records nothing when the content is the same.
func (r *Report) edit(path string, change func(string) string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		r.Problems = append(r.Problems, err.Error())
		return
	}
	next := change(string(raw))
	if next == string(raw) {
		return
	}
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		r.Problems = append(r.Problems, err.Error())
	}
}

var pinLine = regexp.MustCompile(`(?m)^([A-Z_]+) \?= (v[0-9][^\s]*)\r?$`)

// pins bumps the Makefile's tool versions to the newest the module proxy knows.
func (r *Report) pins(root string, src Sources) {
	r.edit(filepath.Join(root, "Makefile"), func(text string) string {
		for _, tool := range pinnedTools {
			var latest struct{ Version string }
			if err := src.getJSON(src.GoProxy+"/"+escapeModule(tool.module)+"/@latest", false, &latest); err != nil {
				r.Problems = append(r.Problems, fmt.Sprintf("%s: %v", tool.variable, err))
				continue
			}
			text = pinLine.ReplaceAllStringFunc(text, func(line string) string {
				m := pinLine.FindStringSubmatch(line)
				if m[1] != tool.variable || m[2] == latest.Version {
					return line
				}
				r.Changes = append(r.Changes, fmt.Sprintf("Makefile %s %s -> %s", m[1], m[2], latest.Version))
				return strings.Replace(line, m[2], latest.Version, 1)
			})
		}
		return text
	})
}

// escapeModule writes a module path as the proxy protocol wants it: capitals as !lower.
func escapeModule(path string) string {
	var b strings.Builder
	for _, c := range path {
		if c >= 'A' && c <= 'Z' {
			b.WriteByte('!')
			c += 'a' - 'A'
		}
		b.WriteRune(c)
	}
	return b.String()
}

var usesLine = regexp.MustCompile(`(uses:\s*)([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)((?:/[^@\s]*)?)@([0-9a-f]{40})(\s*#\s*\S+)?`)

// actions re-pins every workflow action to the commit of its repository's newest release.
func (r *Report) actions(root string, src Sources) {
	files, _ := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.y*ml")) // a fixed pattern cannot be malformed
	sort.Strings(files)
	latest := map[string][2]string{} // repository -> tag, commit
	for _, file := range files {
		said := map[string]bool{} // a change is reported once per file, however often it is used
		r.edit(file, func(text string) string {
			return usesLine.ReplaceAllStringFunc(text, func(use string) string {
				m := usesLine.FindStringSubmatch(use)
				repo := m[2]
				found, ok := latest[repo]
				if !ok {
					tag, sha, err := src.release(repo)
					if err != nil {
						r.Problems = append(r.Problems, fmt.Sprintf("%s: %v", repo, err))
						latest[repo] = [2]string{}
						return use
					}
					found = [2]string{tag, sha}
					latest[repo] = found
				}
				if found[1] == "" || found[1] == m[4] {
					return use
				}
				change := fmt.Sprintf("%s %s%s -> %s (%s)", filepath.Base(file), repo, m[3], found[0], found[1][:12])
				if was := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(m[5]), "#")); was != "" && major(was) != major(found[0]) {
					change += ", major from " + was
				}
				if !said[change] {
					said[change] = true
					r.Changes = append(r.Changes, change)
				}
				return m[1] + repo + m[3] + "@" + found[1] + " # " + found[0]
			})
		})
	}
}

// release is the newest release of repo and the commit its tag points at. A repository that
// publishes tags but no GitHub releases answers releases/latest with 404; its highest version tag
// is used instead.
func (s Sources) release(repo string) (string, string, error) {
	var rel struct {
		Tag string `json:"tag_name"`
	}
	if err := s.getJSON(s.GitHub+"/repos/"+repo+"/releases/latest", true, &rel); err != nil {
		var status *statusError
		if !errors.As(err, &status) || status.code != http.StatusNotFound {
			return "", "", err
		}
		return s.newestTag(repo)
	}
	var ref struct {
		Object struct{ Type, SHA string } `json:"object"`
	}
	if err := s.getJSON(s.GitHub+"/repos/"+repo+"/git/ref/tags/"+escapeTag(rel.Tag), true, &ref); err != nil {
		return "", "", err
	}
	// An annotated tag points at a tag object, which points at the commit. A chain longer than a
	// few links, or one that loops, is refused rather than followed for ever.
	for hops := 0; ref.Object.Type == "tag"; hops++ {
		if hops == 5 {
			return "", "", fmt.Errorf("tag %s does not lead to a commit within 5 tag objects", rel.Tag)
		}
		var tag struct {
			Object struct{ Type, SHA string } `json:"object"`
		}
		if err := s.getJSON(s.GitHub+"/repos/"+repo+"/git/tags/"+ref.Object.SHA, true, &tag); err != nil {
			return "", "", err
		}
		ref.Object = tag.Object
	}
	if !commitSHA.MatchString(ref.Object.SHA) {
		return "", "", fmt.Errorf("tag %s names no commit", rel.Tag)
	}
	return rel.Tag, ref.Object.SHA, nil
}

// escapeTag escapes each part of a tag for a URL path. The slashes in a tag such as pkg/v1 are
// kept, since GitHub's ref path takes them as written.
func escapeTag(tag string) string {
	parts := strings.Split(tag, "/")
	for i, part := range parts {
		parts[i] = neturl.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

// newestTag is the highest version tag on the first page of repo's tags, and its commit. The
// tags list names the commit directly, so no tag object is followed. Only plain versions count:
// a prerelease such as v2.0.0-rc.1 is never chosen over a release.
func (s Sources) newestTag(repo string) (string, string, error) {
	var tags []struct {
		Name   string `json:"name"`
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := s.getJSON(s.GitHub+"/repos/"+repo+"/tags?per_page=100", true, &tags); err != nil {
		return "", "", fmt.Errorf("no release, and the tags could not be listed: %w", err)
	}
	best, sha := "", ""
	for _, tag := range tags {
		if versionTag.MatchString(tag.Name) && (best == "" || newer(tag.Name, best)) {
			best, sha = tag.Name, tag.Commit.SHA
		}
	}
	if best == "" {
		return "", "", fmt.Errorf("no release and no version tag")
	}
	if !commitSHA.MatchString(sha) {
		return "", "", fmt.Errorf("tag %s names no commit", best)
	}
	return best, sha, nil
}

var (
	versionTag = regexp.MustCompile(`^v?\d+(\.\d+)*$`)
	commitSHA  = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// newer reports whether version tag a is higher than b, comparing each number in turn.
func newer(a, b string) bool {
	x, y := numbers(a), numbers(b)
	for i := 0; i < len(x) || i < len(y); i++ {
		var p, q int
		if i < len(x) {
			p = x[i]
		}
		if i < len(y) {
			q = y[i]
		}
		if p != q {
			return p > q
		}
	}
	return false
}

func numbers(tag string) []int {
	var out []int
	for _, part := range strings.Split(strings.TrimPrefix(tag, "v"), ".") {
		n, _ := strconv.Atoi(part) // a part that is not a number counts as 0
		out = append(out, n)
	}
	return out
}

// major is a tag's first number, as written: v4.2.1 and v4 are both "4". A major change is
// flagged in the summary, since only CI proves the action still takes the same inputs.
func major(tag string) string {
	return strings.SplitN(strings.TrimPrefix(tag, "v"), ".", 2)[0]
}

var fromLine = regexp.MustCompile(`(FROM\s+(?:--platform=\S+\s+)?)([a-z0-9./_-]+):([A-Za-z0-9._-]+)@(sha256:[0-9a-f]{64})`)

// images refreshes each Dockerfile base image to the digest its tag names now.
func (r *Report) images(root string, src Sources) {
	files, _ := filepath.Glob(filepath.Join(root, "Dockerfile*")) // a fixed pattern cannot be malformed
	for _, file := range files {
		r.edit(file, func(text string) string {
			return fromLine.ReplaceAllStringFunc(text, func(from string) string {
				m := fromLine.FindStringSubmatch(from)
				digest, err := src.digest(m[2], m[3])
				if err != nil {
					r.Problems = append(r.Problems, fmt.Sprintf("%s:%s: %v", m[2], m[3], err))
					return from
				}
				if digest == m[4] {
					return from
				}
				r.Changes = append(r.Changes, fmt.Sprintf("%s %s:%s %s -> %s", filepath.Base(file), m[2], m[3], m[4][:19], digest[:19]))
				return m[1] + m[2] + ":" + m[3] + "@" + digest
			})
		})
	}
}

// digest asks image's registry which manifest tag names now, with an anonymous token if the
// registry asks for one.
func (s Sources) digest(image, tag string) (string, error) {
	host, repo := "registry-1.docker.io", image
	if first, rest, ok := strings.Cut(image, "/"); ok && strings.ContainsAny(first, ".:") {
		host, repo = first, rest
	} else if !strings.Contains(image, "/") {
		repo = "library/" + image
	}
	url := s.Registry(host) + "/v2/" + repo + "/manifests/" + tag
	res, err := s.manifest(url, "")
	if err == nil && res.StatusCode == http.StatusUnauthorized {
		challenge := res.Header.Get("Www-Authenticate")
		_ = res.Body.Close()
		token, terr := s.registryToken(challenge)
		if terr != nil {
			return "", terr
		}
		res, err = s.manifest(url, token)
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()
	digest := res.Header.Get("Docker-Content-Digest")
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(digest, "sha256:") || len(digest) != 71 {
		return "", fmt.Errorf("registry answered %s without a digest", res.Status)
	}
	return digest, nil
}

func (s Sources) manifest(url, token string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return s.HTTP.Do(req)
}

var challengePart = regexp.MustCompile(`(\w+)="([^"]*)"`)

// registryToken follows a Bearer challenge to the anonymous token it offers.
func (s Sources) registryToken(challenge string) (string, error) {
	params := map[string]string{}
	for _, m := range challengePart.FindAllStringSubmatch(challenge, -1) {
		params[m[1]] = m[2]
	}
	if params["realm"] == "" {
		return "", fmt.Errorf("registry asked for credentials refresh cannot give: %q", challenge)
	}
	realm, err := neturl.Parse(params["realm"])
	if err != nil {
		return "", fmt.Errorf("registry gave a bad token realm %q: %v", params["realm"], err)
	}
	query := realm.Query()
	for _, key := range []string{"service", "scope"} {
		if params[key] != "" {
			query.Set(key, params[key])
		}
	}
	realm.RawQuery = query.Encode()
	url := realm.String()
	var out struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := s.getJSON(url, false, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		out.Token = out.AccessToken
	}
	if out.Token == "" {
		return "", fmt.Errorf("the token endpoint gave no token")
	}
	return out.Token, nil
}

// statusError is an answer other than 200, kept typed so a 404 can be told apart.
type statusError struct {
	url, status string
	code        int
}

func (e *statusError) Error() string { return "GET " + e.url + ": " + e.status }

// getJSON fetches url into dest. A GitHub call carries the token when there is one.
func (s Sources) getJSON(url string, github bool, dest any) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if github {
		req.Header.Set("Accept", "application/vnd.github+json")
		if s.Token != "" {
			req.Header.Set("Authorization", "Bearer "+s.Token)
		}
	}
	res, err := s.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return &statusError{url: url, code: res.StatusCode, status: res.Status}
	}
	return json.NewDecoder(res.Body).Decode(dest)
}

var requireLine = regexp.MustCompile(`(?m)^\s*(?:require\s+)?([a-z0-9./_-]+\.[a-z]+/\S*)\s+(v\S+)(\s+// indirect)?\s*$`)

// goModules updates every module and records which versions moved.
func (r *Report) goModules(root string, runner Runner) {
	before := modules(root)
	for _, args := range [][]string{{"get", "-u", "./..."}, {"mod", "tidy"}} {
		if out, err := runner(root, "go", args...); err != nil {
			r.Problems = append(r.Problems, fmt.Sprintf("go %s failed: %v\n%s", strings.Join(args, " "), err, tail(out)))
			return
		}
	}
	after := modules(root)
	r.Changes = append(r.Changes, moved("go.mod", before, after)...)
}

func modules(root string) map[string]string {
	raw, _ := os.ReadFile(filepath.Join(root, "go.mod")) // a missing go.mod lists nothing
	out := map[string]string{}
	for _, m := range requireLine.FindAllStringSubmatch(string(raw), -1) {
		out[m[1]] = m[2]
	}
	return out
}

// webPackages updates the console's packages within package.json's ranges and the lockfile.
func (r *Report) webPackages(root string, runner Runner) {
	lock := filepath.Join(root, "web", "package-lock.json")
	before := packages(lock)
	if out, err := runner(filepath.Join(root, "web"), "npm", "update"); err != nil {
		r.Problems = append(r.Problems, fmt.Sprintf("npm update failed: %v\n%s", err, tail(out)))
		return
	}
	r.Changes = append(r.Changes, moved("web", before, packages(lock))...)
}

func packages(lock string) map[string]string {
	raw, _ := os.ReadFile(lock) // a missing lockfile lists nothing
	var doc struct {
		Packages map[string]struct {
			Version string `json:"version"`
		} `json:"packages"`
	}
	_ = json.Unmarshal(raw, &doc) // a broken lockfile lists nothing, and npm reports it
	out := map[string]string{}
	for path, p := range doc.Packages {
		if name, ok := strings.CutPrefix(path, "node_modules/"); ok && !strings.Contains(name, "/node_modules/") {
			out[name] = p.Version
		}
	}
	return out
}

func moved(where string, before, after map[string]string) []string {
	var out []string
	for name, v := range after {
		if old, ok := before[name]; ok && old != v {
			out = append(out, fmt.Sprintf("%s %s %s -> %s", where, name, old, v))
		} else if !ok {
			out = append(out, fmt.Sprintf("%s %s added at %s", where, name, v))
		}
	}
	for name, v := range before {
		if _, ok := after[name]; !ok {
			out = append(out, fmt.Sprintf("%s %s %s removed", where, name, v))
		}
	}
	sort.Strings(out)
	return out
}

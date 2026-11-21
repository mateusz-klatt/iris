package store

import (
	"strconv"
	"strings"
)

// outsidePrefix begins the alias that marks a host's context: the one record of every session on
// that host outside git (ADR 034). The host is the whole rest of the alias, so it needs no quoting.
const outsidePrefix = "outside:"

func outsideAlias(host string) string { return outsidePrefix + host }

// outsideShape is a registration that names no checkout at all. A new client sends it on the
// outside route only; an older one sent it on the session route for a directory outside git, and
// no shipped client sends it after a successful checkout discovery (ADR 034, decision 2).
func outsideShape(r Request) bool {
	return r.Worktree == "" && r.CommonDir == "" && r.WorktreeID == "" && r.RemoteURL == "" && r.RootCommit == ""
}

// sessionOutside registers a session outside git, on the route a new client uses for one.
func (t *transaction) sessionOutside(r Request) (map[string]any, error) {
	if !outsideShape(r) {
		return nil, fail("bad_request", "a session outside git names no worktree, git directory, worktree id, remote or root")
	}
	return t.register(r, true)
}

// outsideContext is host's context, made in this transaction when there is none. Two records with
// the marker are refused rather than one chosen by row order; only the owner makes a second one.
func (t *transaction) outsideContext(host string) (string, error) {
	rows, err := t.rows("SELECT repository_id FROM RepositoryAlias WHERE alias=?", outsideAlias(host))
	if err != nil {
		return "", err
	}
	var found []string
	for rows.Next() {
		var repo string
		rows.Scan(&repo)
		found = append(found, repo)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return "", err
	}
	if len(found) > 1 {
		return "", fail("conflict", "more than one record holds this host's sessions outside git; ask the owner to repair it")
	}
	if len(found) == 1 {
		return found[0], nil
	}
	repo := id("rp_")
	if _, err = t.exec("INSERT INTO Repository(id,case_policy,root_commit,name) VALUES(?,'folded','',?)", repo, "outside git on "+host); err != nil {
		return "", err
	}
	_, err = t.exec("INSERT INTO RepositoryAlias VALUES(?,?)", outsideAlias(host), repo)
	return repo, err
}

// outsideHost is the host whose context repo is, or "" when repo is a repository.
func (t *transaction) outsideHost(repo string) (string, error) {
	var host string
	err := t.row("SELECT COALESCE(MAX(substr(alias,?)),'') FROM RepositoryAlias WHERE repository_id=? AND substr(alias,1,?)=?", len(outsidePrefix)+1, repo, len(outsidePrefix), outsidePrefix).Scan(&host)
	return host, err
}

// noClaims is the answer to a claim from an Execution in a context (ADR 034, decision 5).
func noClaims() error {
	return fail("outside_git", "this session runs outside any git checkout, so it takes no claims; start a session in the checkout to claim its files")
}

// folderAlias says whether alias is one a folder project of host learns: its path, its checkout
// continuity with no git directory and no remote, or its own isolation marker (ADR 034).
func folderAlias(alias, host, repo string) bool {
	if strings.HasPrefix(alias, "local:"+host+":") || alias == isolatedAlias(repo) {
		return true
	}
	// checkoutRepositoryAlias quotes host, worktree, git directory and remote in that order.
	worktree, prefixed := strings.CutPrefix(alias, "checkout:"+strconv.Quote(host)+":")
	worktree, suffixed := strings.CutSuffix(worktree, `:"":"literal:"`)
	_, err := strconv.Unquote(worktree)
	return prefixed && suffixed && err == nil
}

// folderProject refuses a source that is not a quiet folder project of host (ADR 034, decision 9).
func (t *transaction) folderProject(repo, host, root string) error {
	if root != "" {
		return fail("conflict", "the source has a root commit; only a folder project moves into a host's context")
	}
	rows, err := t.rows("SELECT alias FROM RepositoryAlias WHERE repository_id=? ORDER BY alias", repo)
	if err != nil {
		return err
	}
	var foreign []string
	for rows.Next() {
		var alias string
		rows.Scan(&alias)
		if !folderAlias(alias, host, repo) {
			foreign = append(foreign, alias)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	if len(foreign) > 0 {
		return &Error{"conflict", "the source has aliases of a checkout or of another host; only a folder project of this host moves into its context", map[string]any{"aliases": foreign}}
	}
	var elsewhere, active, held int
	// Only the console's person, the operator's human mailbox with no worktree, is no host's: an
	// ordinary session whose client calls itself human is checked like any other.
	if err = t.row(`SELECT (SELECT COUNT(*) FROM Agent WHERE repository_id=?1 AND host<>?2 AND NOT (client='human' AND host='operator' AND worktree=''))
 +(SELECT COUNT(*) FROM Execution WHERE repository_id=?1 AND host_id<>?2 AND NOT (client='human' AND host_id='operator' AND worktree=''))`, repo, host).Scan(&elsewhere); err != nil {
		return err
	}
	if elsewhere > 0 {
		return fail("conflict", "the source has sessions of another host")
	}
	if err = t.row("SELECT COUNT(*) FROM Execution WHERE repository_id=? AND state='active'", repo).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return fail("conflict", "the source still has an active or recovering session; repair it after the session ends")
	}
	// The source's own sessions are not active, so a live claim here is another repository's
	// session holding a path in the source.
	if err = t.row("SELECT COUNT(*) FROM Claim c JOIN Execution e ON e.id=c.execution_id WHERE c.repository_id=? AND c.revoked_ms IS NULL AND e.state='active' AND (c.expires_ms IS NULL OR c.expires_ms>?)", repo, t.s.clock.Now().UnixMilli()).Scan(&held); err != nil {
		return err
	}
	if held > 0 {
		return fail("conflict", "a live or recovering claim is held in the source; release it before the repair")
	}
	return nil
}

// folderLeftovers counts what a repair drops: the source's waits and shell baselines.
func (t *transaction) folderLeftovers(repo string) (map[string]any, error) {
	var waiters, snapshots int
	err := t.row(`SELECT (SELECT COUNT(*) FROM Waiter WHERE repository_id=?1 OR execution_id IN (SELECT id FROM Execution WHERE repository_id=?1)),
 (SELECT COUNT(*) FROM ShellSnapshot WHERE execution_id IN (SELECT id FROM Execution WHERE repository_id=?1))+(SELECT COUNT(*) FROM ReducedShellSnapshot WHERE execution_id IN (SELECT id FROM Execution WHERE repository_id=?1))`, repo).Scan(&waiters, &snapshots)
	return map[string]any{"waiters": waiters, "shell_snapshots": snapshots}, err
}

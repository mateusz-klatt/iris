package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

// Request contains the version-one wire fields. Unknown client metadata is ignored.
type Request struct {
	CommonDir string            `json:"common_dir"`
	Status    map[string]string `json:"status"`
	ReplyTo   string            `json:"reply_to"`
	Q         string            `json:"q"`
	Agent     string            `json:"agent"`
	DryRun    bool              `json:"dry_run"`
	Before    string            `json:"before"`
	CheckedAt string            `json:"checked_at"`
	Clients   []map[string]any  `json:"clients"`
	Client    string            `json:"client"`
	SessionID string            `json:"session_id"`
	Host      string            `json:"host"`
	Cwd       string            `json:"cwd"`
	Worktree  string            `json:"worktree"`
	// WorktreeID names the worktree root by device and inode; another path to the same
	// directory is made the first path this host named for it (D6).
	WorktreeID string `json:"worktree_id"`
	// Receipt asks a fetch to keep its mail needing attention until Receipts confirm the response
	// arrived (ADR 010).
	Receipt          bool      `json:"receipt"`
	Receipts         []Receipt `json:"receipts"`
	RemoteURL        string    `json:"remote_url"`
	RootCommit       string    `json:"root_commit"`
	Branch           string    `json:"branch"`
	ParentSessionID  string    `json:"parent_session_id"`
	ClientPID        int64     `json:"client_pid"`
	ClientPIDStartMS int64     `json:"client_pid_start_ms"`
	SingleSession    bool      `json:"single_session"`
	Source           string    `json:"source"`
	Execution        string    `json:"execution"`
	// Exclude names an Execution a bind must skip: an MCP that had to register its own asks
	// past it for the session's hook's.
	Exclude string `json:"exclude"`
	// HandOver names the Execution that ending this one hands its unacknowledged mail to: the
	// session's hook's, when an MCP ends the Execution it had to register itself.
	HandOver     string   `json:"hand_over"`
	ExecutionID  string   `json:"execution_id"`
	Generation   int64    `json:"generation"`
	Key          string   `json:"key"`
	ToolCallID   string   `json:"tool_call_id"`
	Paths        []string `json:"paths"`
	RenameFrom   string   `json:"rename_from"`
	RenameTo     string   `json:"rename_to"`
	Kind         string   `json:"kind"`
	Path         string   `json:"path"`
	RepoWide     bool     `json:"repo_wide"`
	TTLS         int64    `json:"ttl_s"`
	Fence        int64    `json:"fence"`
	All          bool     `json:"all"`
	Token        string   `json:"token"`
	To           []string `json:"to"`
	Subject      string   `json:"subject"`
	Body         string   `json:"body"`
	Attempt      int64    `json:"attempt"`
	Handled      bool     `json:"handled"`
	RepositoryID string   `json:"repository_id"`
	After        int64    `json:"after"`
	Limit        int      `json:"limit"`
	ConsumerID   string   `json:"consumer_id"`
	Stream       string   `json:"stream"`
}

// Receipt names one delivery a fetch returned, at the attempt that fetch gave it.
type Receipt struct {
	ID      string `json:"id"`
	Attempt int64  `json:"attempt"`
}

type execution struct {
	ID, AgentID, Repo, Host, Client, Session, Worktree, Branch, State, Name string
	Generation, Epoch, Deadline, PID, PIDStart                              int64
	Recovery                                                                sql.NullInt64
	Parent                                                                  sql.NullString
	Single, Heartbeat                                                       bool
}

const executionSelect = `SELECT e.id,e.agent_id,e.repository_id,e.host_id,e.client,e.session_id,e.worktree,e.branch,e.state,a.name,e.generation,e.boot_epoch,e.deadline_tick_ms,e.client_pid,e.client_pid_start_ms,e.recovery_deadline,e.parent_id,e.single_session,e.heartbeat_mode FROM Execution e JOIN Agent a ON a.id=e.agent_id `

func (e *execution) fields() []any {
	return []any{&e.ID, &e.AgentID, &e.Repo, &e.Host, &e.Client, &e.Session, &e.Worktree, &e.Branch, &e.State, &e.Name, &e.Generation, &e.Epoch, &e.Deadline, &e.PID, &e.PIDStart, &e.Recovery, &e.Parent, &e.Single, &e.Heartbeat}
}

func scanExecution(row interface{ Scan(...any) error }) (e execution, err error) {
	err = row.Scan(e.fields()...)
	return
}
func (t *transaction) live(ex string, generation int64) (execution, error) {
	e, err := scanExecution(t.row(executionSelect+"WHERE e.id=?", ex))
	if errors.Is(err, sql.ErrNoRows) {
		return e, fail("lost", "Execution is unknown; register the session again")
	}
	if err != nil {
		return e, err
	}
	if e.State != "active" || e.Epoch != t.s.epoch || e.Recovery.Valid || e.Deadline <= t.s.clock.Tick() || (generation != 0 && e.Generation != generation) {
		return e, fail("lost", "Execution lease or generation is lost; reattach the session")
	}
	return e, nil
}
func bound(e execution) map[string]any {
	return map[string]any{"execution_id": e.ID, "generation": e.Generation, "repository_id": e.Repo, "agent": map[string]any{"id": e.AgentID, "name": e.Name}}
}

var safePart = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

func namePart(s string) string {
	s = strings.Trim(safePart.ReplaceAllString(s, "-"), "-")
	if len(s) > 64 {
		s = s[:64]
	}
	if s == "" {
		return "unknown"
	}
	return s
}

// scpRemote is git's scp-like form, as in git@host:owner/repo.git. It is not a URL.
var scpRemote = regexp.MustCompile(`^(?:[^@/]+@)?([^:/]+):([^/].*)$`)

var defaultPorts = map[string]string{"https": "443", "http": "80", "ssh": "22", "git": "9418"}

// remoteAlias names a repository by host and path, so that one repository cloned over
// https on one machine and over ssh on another has one alias. It returns nothing for a
// host that may mean something different on another machine.
func remoteAlias(raw string) string {
	raw = strings.TrimSpace(raw)
	var host, repoPath, port string
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		if u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "ssh" && u.Scheme != "git" {
			return ""
		}
		host, repoPath = u.Hostname(), u.Path
		// A non-default port can be a different service on the same host, so it stays
		// part of the identity. A default port is the same address written longer.
		if p := u.Port(); p != "" && p != defaultPorts[u.Scheme] {
			port = ":" + p
		}
	} else if m := scpRemote.FindStringSubmatch(raw); m != nil {
		host, repoPath = m[1], m[2]
	} else {
		return ""
	}
	host = strings.ToLower(host)
	if !strings.Contains(host, ".") || net.ParseIP(host) != nil || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".") {
		return ""
	}
	repoPath = strings.Trim(strings.TrimSuffix(strings.TrimRight(repoPath, "/"), ".git"), "/")
	if repoPath == "" {
		return ""
	}
	return "remote:" + host + port + "/" + repoPath
}

func (t *transaction) repository(r Request) (string, error) {
	// Machine-relative remotes are deliberately never global aliases.
	local := "local:" + r.Host + ":" + strings.ReplaceAll(r.Worktree, "\\", "/")
	aliases := []string{local}
	if r.CommonDir != "" {
		aliases = append(aliases, "gitdir:"+r.Host+":"+strings.ReplaceAll(r.CommonDir, "\\", "/"))
	}
	var repo, root string
	var err error
	for _, alias := range aliases {
		err = t.row("SELECT r.id,r.root_commit FROM RepositoryAlias a JOIN Repository r ON r.id=a.repository_id WHERE a.alias=?", alias).Scan(&repo, &root)
		if err == nil && (root == "" || r.RootCommit == "" || root == r.RootCommit) {
			// A known worktree can gain its global identity later, for example after a
			// remote is added, so that other machines can still join this repository.
			known := aliases
			if remote := remoteAlias(r.RemoteURL); remote != "" {
				known = append(known, remote)
			}
			if r.RootCommit != "" {
				known = append(known, "root:"+r.RootCommit)
				// The root learned now is this repository's root from here on, so a later
				// session that presents a different one cannot join it by remote.
				if root == "" {
					if _, err = t.exec("UPDATE Repository SET root_commit=? WHERE id=? AND root_commit=''", r.RootCommit, repo); err != nil {
						return "", err
					}
				}
			}
			for _, key := range known {
				if _, err = t.exec("INSERT OR IGNORE INTO RepositoryAlias VALUES(?,?)", key, repo); err != nil {
					return "", err
				}
			}
			return repo, nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		repo = ""
	}
	remote := remoteAlias(r.RemoteURL)
	if remote != "" {
		rows, err := t.rows("SELECT r.id,r.root_commit FROM RepositoryAlias a JOIN Repository r ON r.id=a.repository_id WHERE a.alias=?", remote)
		if err != nil {
			return "", err
		}
		count := 0
		contradiction := false
		for rows.Next() {
			var candidate, commit string
			rows.Scan(&candidate, &commit)
			count++
			if commit != "" && r.RootCommit != "" && commit != r.RootCommit {
				contradiction = true
			}
			repo = candidate
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return "", err
		}
		if count != 1 || contradiction {
			repo = ""
		}
	}
	if repo == "" {
		repo = id("rp_")
		if _, err = t.exec("INSERT INTO Repository(id,case_policy,root_commit,name) VALUES(?,'folded',?,?)", repo, r.RootCommit, repositoryName(r.RemoteURL, r.Worktree)); err != nil {
			return "", err
		}
	}
	for _, alias := range aliases {
		if _, err = t.exec("INSERT OR IGNORE INTO RepositoryAlias VALUES(?,?)", alias, repo); err != nil {
			return "", err
		}
	}
	if remote != "" {
		if _, err = t.exec("INSERT OR IGNORE INTO RepositoryAlias VALUES(?,?)", remote, repo); err != nil {
			return "", err
		}
	}
	if r.RootCommit != "" {
		_, err = t.exec("INSERT OR IGNORE INTO RepositoryAlias VALUES(?,?)", "root:"+r.RootCommit, repo)
	}
	return repo, err
}

// resume renews a session and, after a hub restart, moves it into the current epoch.
func (t *transaction) resume(e *execution, strong bool) (int64, error) {
	lease := int64(900000)
	if e.Heartbeat || strong {
		lease = 60000
		e.Heartbeat = true
	}
	if _, err := t.exec("UPDATE Execution SET recovery_deadline=NULL,boot_epoch=?,deadline_tick_ms=?,heartbeat_mode=? WHERE id=?", t.s.epoch, t.s.clock.Tick()+lease, e.Heartbeat, e.ID); err != nil {
		return 0, err
	}
	e.Epoch = t.s.epoch
	e.Recovery = sql.NullInt64{}
	e.Deadline = t.s.clock.Tick() + lease
	// Rotate the numeric fence as well as the epoch: release accepts a fence
	// without a complete token, so retaining the number would admit zombies.
	if _, err := t.exec(`INSERT INTO Claim(execution_id,repository_id,kind,path,path_key,origin,boot_epoch,expires_ms)
            SELECT execution_id,repository_id,kind,path,path_key,origin,?,expires_ms FROM Claim
            WHERE execution_id=? AND revoked_ms IS NULL AND boot_epoch<>?`, t.s.epoch, e.ID, t.s.epoch); err != nil {
		return 0, err
	}
	_, err := t.exec("UPDATE Claim SET revoked_ms=? WHERE execution_id=? AND revoked_ms IS NULL AND boot_epoch<>?", t.s.clock.Now().UnixMilli(), e.ID, t.s.epoch)
	return lease, err
}
func (t *transaction) session(r Request) (map[string]any, error) {
	if r.Client == "" || r.SessionID == "" || r.Host == "" || r.Cwd == "" {
		return nil, fail("bad_request", "client, session_id, host and cwd are required")
	}
	if r.Worktree == "" {
		r.Worktree = r.Cwd
	}
	if err := t.canonicalWorktree(&r); err != nil {
		return nil, err
	}
	e, err := scanExecution(t.row(executionSelect+"WHERE e.host_id=? AND e.client=? AND e.session_id=? AND e.state='active'", r.Host, r.Client, r.SessionID))
	if err == nil {
		if r.Generation != 0 && e.Generation != r.Generation {
			return nil, fail("lost", "session generation no longer exists")
		}
		if e.Recovery.Valid && r.Generation != e.Generation {
			return nil, fail("lost", "recovery requires the prior session generation")
		}
		lease, err := t.resume(&e, r.Source == "shim" || r.Source == "watch" || r.Source == "mcp")
		if err != nil {
			return nil, err
		}
		return t.sessionResponse(e, lease)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if r.Generation != 0 {
		return nil, fail("lost", "expired generations cannot be revived")
	}
	repo, err := t.repository(r)
	if err != nil {
		return nil, err
	}
	var generation int64
	if err = t.row("SELECT COALESCE(MAX(generation),0)+1 FROM Execution WHERE host_id=? AND client=? AND session_id=?", r.Host, r.Client, r.SessionID).Scan(&generation); err != nil {
		return nil, err
	}
	var agent, agentName string
	// A session that ran here before gets its own mailbox back, unless another active
	// session holds it now. Without this, the first free slot was taken, so the
	// order in which sessions resumed decided whose mail each one saw.
	prior, priorName, err := t.priorAgent(r, repo)
	if err != nil {
		return nil, err
	}
	slot := 1
	for prior == "" {
		var busy int
		err = t.row("SELECT id,name FROM Agent WHERE repository_id=? AND host=? AND client=? AND worktree=? AND slot=?", repo, r.Host, r.Client, r.Worktree, slot).Scan(&agent, &agentName)
		if errors.Is(err, sql.ErrNoRows) {
			agent = id("ag_")
			agentName = namePart(r.Client) + "-" + namePart(r.Host) + "-" + namePart(path.Base(strings.ReplaceAll(r.Worktree, "\\", "/")))
			if slot > 1 {
				agentName += fmt.Sprintf("-%d", slot)
			}
			var duplicate int
			if err = t.row("SELECT COUNT(*) FROM Agent WHERE name=?", agentName).Scan(&duplicate); err != nil {
				return nil, err
			}
			if duplicate > 0 {
				agentName += "-" + strings.TrimPrefix(agent, "ag_")[:8]
			}
			if _, err = t.exec("INSERT INTO Agent VALUES(?,?,?,?,?,?,?)", agent, agentName, repo, r.Host, r.Client, r.Worktree, slot); err != nil {
				return nil, err
			}
			break
		}
		if err != nil {
			return nil, err
		}
		if err = t.row("SELECT COUNT(*) FROM Execution WHERE agent_id=? AND state='active'", agent).Scan(&busy); err != nil {
			return nil, err
		}
		if busy == 0 {
			break
		}
		slot++
	}
	if prior != "" {
		agent, agentName = prior, priorName
	}
	var parent any
	if r.ParentSessionID != "" {
		var parentID string
		if err = t.row("SELECT id FROM Execution WHERE host_id=? AND client=? AND session_id=? AND repository_id=? AND state='active'", r.Host, r.Client, r.ParentSessionID, repo).Scan(&parentID); err != nil {
			return nil, absent(err, "bad_request", "parent session is not active in this repository")
		}
		parent = parentID
	}
	lease := int64(900000)
	heartbeat := r.Source == "shim" || r.Source == "watch" || r.Source == "mcp"
	if heartbeat {
		lease = 60000
	}
	ex := id("ex_")
	_, err = t.exec(`INSERT INTO Execution(id,agent_id,repository_id,host_id,client,session_id,generation,worktree,branch,boot_epoch,deadline_tick_ms,state,parent_id,client_pid,client_pid_start_ms,single_session,heartbeat_mode) VALUES(?,?,?,?,?,?,?,?,?,?,?,'active',?,?,?,?,?)`, ex, agent, repo, r.Host, r.Client, r.SessionID, generation, r.Worktree, r.Branch, t.s.epoch, t.s.clock.Tick()+lease, parent, r.ClientPID, r.ClientPIDStartMS, r.SingleSession, heartbeat)
	if err != nil {
		return nil, err
	}
	e, err = scanExecution(t.row(executionSelect+"WHERE e.id=?", ex))
	if err != nil {
		return nil, err
	}
	return t.sessionResponse(e, lease)
}
func (t *transaction) sessionResponse(e execution, lease int64) (map[string]any, error) {
	out := bound(e)
	out["handle"] = e.ID
	out["repository_id"] = e.Repo
	out["lease_ms"] = lease
	peers, err := t.agents(e.Repo)
	if err != nil {
		return nil, err
	}
	out["peers"] = peers
	claims, err := t.claims(e.Repo)
	if err != nil {
		return nil, err
	}
	out["claims"] = claims
	var unread int
	if err = t.row("SELECT COUNT(*) FROM Delivery WHERE recipient_agent_id=? AND id NOT IN (SELECT delivery_id FROM Ack)", e.AgentID).Scan(&unread); err != nil {
		return nil, err
	}
	out["unread"] = unread
	if out["attention"], err = t.attention(e); err != nil {
		return nil, err
	}
	return out, nil
}
func (t *transaction) bind(r Request) (map[string]any, error) {
	if r.Execution != "" {
		e, err := t.live(r.Execution, 0)
		if err != nil {
			return nil, err
		}
		if e.Client != r.Client || e.Host != r.Host {
			return nil, fail("unbound", "handle does not match this client and host")
		}
		return bound(e), nil
	}
	query := executionSelect + "WHERE e.client=? AND e.host_id=? AND e.state='active' AND e.recovery_deadline IS NULL AND e.boot_epoch=? AND e.deadline_tick_ms>?"
	args := []any{r.Client, r.Host, t.s.epoch, t.s.clock.Tick()}
	if r.SessionID != "" {
		query += " AND e.session_id=?"
		args = append(args, r.SessionID)
	} else {
		if r.ClientPID <= 0 || r.ClientPIDStartMS <= 0 {
			return nil, fail("unbound", "provide a session id or explicit execution handle")
		}
		// A process positively known to host one session, or one top-level session, as a
		// Claude process does: its subagents register with a parent and do not compete. A
		// process that multiplexes sessions is never matched this way (D6).
		query += " AND e.client_pid=? AND e.client_pid_start_ms=? AND (e.single_session=1 OR (e.parent_id IS NULL AND e.client IN ('claude')))"
		args = append(args, r.ClientPID, r.ClientPIDStartMS)
	}
	if r.Exclude != "" {
		query += " AND e.id<>?"
		args = append(args, r.Exclude)
	}
	rows, err := t.rows(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var matches []execution
	for rows.Next() {
		var e execution
		rows.Scan(e.fields()...)
		matches = append(matches, e)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fail("unbound", "no positive binding; provide the SessionStart execution handle")
	}
	if len(matches) > 1 {
		return nil, fail("ambiguous", "multiple sessions match; provide an explicit execution handle")
	}
	return bound(matches[0]), nil
}
func (t *transaction) heartbeat(r Request) (map[string]any, error) {
	if r.Source != "hook" && r.Source != "shim" && r.Source != "watch" {
		return nil, fail("bad_request", "unknown heartbeat source")
	}
	strong := r.Source != "hook"
	// A shim knows its Execution and generation but not always a session id, and an idle
	// session fires no hook. Its heartbeat is therefore what reattaches it after a restart.
	e, err := scanExecution(t.row(executionSelect+"WHERE e.id=?", r.ExecutionID))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && e.State == "active" && e.Recovery.Valid && e.Generation == r.Generation {
		lease, err := t.resume(&e, strong)
		if err != nil {
			return nil, err
		}
		return t.sessionResponse(e, lease)
	}
	if e, err = t.live(r.ExecutionID, r.Generation); err != nil {
		return nil, err
	}
	lease := int64(900000)
	if e.Heartbeat || strong {
		strong, lease = true, 60000
	}
	if _, err = t.exec("UPDATE Execution SET deadline_tick_ms=?,heartbeat_mode=? WHERE id=?", t.s.clock.Tick()+lease, strong, e.ID); err != nil {
		return nil, err
	}
	if r.Source == "watch" {
		// The session's monitor is alive, so its hooks need not repeat what it says (A4).
		err = t.watchSeen(e.ID)
	}
	return map[string]any{"lease_ms": lease}, err
}
func (t *transaction) end(r Request) (map[string]any, error) {
	var generation int64
	var state string
	if err := t.row("SELECT generation,state FROM Execution WHERE id=?", r.ExecutionID).Scan(&generation, &state); err != nil {
		return nil, absent(err, "lost", "unknown Execution")
	}
	if generation != r.Generation {
		return nil, fail("lost", "wrong generation")
	}
	if state == "ended" {
		return map[string]any{}, nil
	}
	e, err := t.live(r.ExecutionID, r.Generation)
	if err != nil {
		return nil, err
	}
	if r.HandOver != "" {
		if err = t.handOver(e, r.HandOver); err != nil {
			return nil, err
		}
	}
	if _, err := t.exec("UPDATE Execution SET state='ended' WHERE id=?", r.ExecutionID); err != nil {
		return nil, err
	}
	return map[string]any{}, t.expire()
}

// handOver moves from's agent's unacknowledged mail to the agent of the live Execution to, and
// wakes it. Only an Execution an MCP registered may hand over, and only to the one the client
// process rung finds for it: top-level, of the same host, client, process id and start time.
// An MCP that had to register its own does this when it moves to the session's hook's, so mail
// sent to the stray mailbox in the meantime is not stranded (D6).
func (t *transaction) handOver(from execution, to string) error {
	dst, err := t.live(to, 0)
	if err != nil {
		var domain *Error
		if !errors.As(err, &domain) {
			return err
		}
	}
	if err != nil || !from.Heartbeat || from.PID <= 0 || dst.Host != from.Host || dst.Client != from.Client ||
		dst.PID != from.PID || dst.PIDStart != from.PIDStart || dst.Parent.Valid {
		return fail("bad_request", "mail can be handed only from an MCP's own Execution to its client process's session")
	}
	// A message sent to both mailboxes is already the destination's: the stray copy is settled
	// rather than moved onto it, which the one-delivery-per-recipient rule forbids.
	rows, err := t.rows("SELECT id,attempt,EXISTS(SELECT 1 FROM Delivery o WHERE o.message_id=d.message_id AND o.recipient_agent_id=?) FROM Delivery d WHERE recipient_agent_id=? AND id NOT IN (SELECT delivery_id FROM Ack)", dst.AgentID, from.AgentID)
	if err != nil {
		return err
	}
	type stray struct {
		id      string
		attempt int64
		dup     bool
	}
	var all []stray
	for rows.Next() {
		var d stray
		rows.Scan(&d.id, &d.attempt, &d.dup)
		all = append(all, d)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	now := t.s.clock.Now().UnixMilli()
	for _, d := range all {
		if d.dup {
			// An acknowledgement names an attempt above zero; settling counts as one.
			attempt := max(d.attempt, 1)
			if _, err = t.exec("UPDATE Delivery SET execution_id=?,attempt=?,attention=0 WHERE id=?", from.ID, attempt, d.id); err != nil {
				return err
			}
			if _, err = t.exec("INSERT INTO Ack(delivery_id,execution_id,attempt,acked_ms) VALUES(?,?,?,?)", d.id, from.ID, attempt, now); err != nil {
				return err
			}
			continue
		}
		if _, err = t.exec("UPDATE Delivery SET recipient_agent_id=?,execution_id=NULL,attention=1 WHERE id=?", dst.AgentID, d.id); err != nil {
			return err
		}
		if err = t.event(dst.AgentID, "", "inbox_changed", d.id); err != nil {
			return err
		}
	}
	return nil
}
func (t *transaction) agents(repo string) ([]map[string]any, error) {
	rows, err := t.rows(`SELECT a.id,a.name,a.client,a.worktree,COALESCE(e.branch,''),COALESCE(e.state,'offline') FROM Agent a LEFT JOIN Execution e ON e.id=(SELECT id FROM Execution WHERE agent_id=a.id ORDER BY rowid DESC LIMIT 1) WHERE a.repository_id=? ORDER BY a.name`, repo)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []map[string]any{}
	for rows.Next() {
		var a, n, c, w, b, s string
		rows.Scan(&a, &n, &c, &w, &b, &s)
		out = append(out, map[string]any{"id": a, "name": n, "client": c, "worktree": w, "branch": b, "state": s})
	}
	return out, rows.Err()
}
func timestamp(ms sql.NullInt64) any {
	if !ms.Valid {
		return nil
	}
	return time.UnixMilli(ms.Int64).UTC().Format(time.RFC3339Nano)
}

// priorAgent is the mailbox this session used last in repo, from the same host,
// client and worktree, when no active Execution holds it now. It is "" otherwise.
func (t *transaction) priorAgent(r Request, repo string) (id, name string, err error) {
	err = t.row(`SELECT a.id,a.name FROM Execution e JOIN Agent a ON a.id=e.agent_id
 WHERE e.host_id=? AND e.client=? AND e.session_id=? AND e.repository_id=? AND a.worktree=?
 AND NOT EXISTS (SELECT 1 FROM Execution x WHERE x.agent_id=e.agent_id AND x.state='active')
 ORDER BY e.generation DESC LIMIT 1`, r.Host, r.Client, r.SessionID, repo, r.Worktree).Scan(&id, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return id, name, err
}

// hooksSeen is, for each client on host, when its hooks last reached the hub, or nothing for a
// client whose hooks never did. iris doctor shows it as the client's last hook.
func (t *transaction) hooksSeen(host string) (map[string]any, error) {
	if host == "" {
		return nil, fail("bad_request", "host is required")
	}
	rows, err := t.rows("SELECT client,MAX(hook_seen_ms) FROM Execution WHERE host_id=? AND hook_seen_ms>0 GROUP BY client", host)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	clients := map[string]any{}
	for rows.Next() {
		var client string
		var ms int64
		rows.Scan(&client, &ms)
		clients[client] = timestamp(sql.NullInt64{Int64: ms, Valid: true})
	}
	return map[string]any{"clients": clients}, rows.Err()
}

// canonicalWorktree makes r's worktree the path this host first named for that directory, and
// its cwd the same place under it, when r names the directory by device and inode. A bind
// mount or a symlink reaches one directory by two paths, and keyed on the path alone it was two
// worktrees and two agents for one session (D6). A remount or a reboot can give a device and
// inode to another directory, so only a path of the same repository, by remote and root
// commit, folds into the first one; another repository under that id replaces it. Without an
// id, as from an older client or on Windows, the path stands, as before.
func (t *transaction) canonicalWorktree(r *Request) error {
	if r.WorktreeID == "" || r.Host == "" || r.Worktree == "" {
		return nil
	}
	var first, remote, root string
	err := t.row("SELECT worktree,remote_url,root_commit FROM WorktreeAlias WHERE host_id=? AND worktree_id=?", r.Host, r.WorktreeID).Scan(&first, &remote, &root)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (remote != r.RemoteURL || root != r.RootCommit)) {
		_, err = t.exec("INSERT OR REPLACE INTO WorktreeAlias(host_id,worktree_id,worktree,remote_url,root_commit) VALUES(?,?,?,?,?)", r.Host, r.WorktreeID, r.Worktree, r.RemoteURL, r.RootCommit)
		return err
	}
	if err != nil || first == r.Worktree {
		return err
	}
	if rest, ok := strings.CutPrefix(r.Cwd, r.Worktree); ok && (rest == "" || rest[0] == '/' || rest[0] == '\\') {
		r.Cwd = first + rest
	}
	r.Worktree = first
	return nil
}

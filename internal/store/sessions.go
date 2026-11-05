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
	Client           string   `json:"client"`
	SessionID        string   `json:"session_id"`
	Host             string   `json:"host"`
	Cwd              string   `json:"cwd"`
	Worktree         string   `json:"worktree"`
	RemoteURL        string   `json:"remote_url"`
	RootCommit       string   `json:"root_commit"`
	Branch           string   `json:"branch"`
	ParentSessionID  string   `json:"parent_session_id"`
	ClientPID        int64    `json:"client_pid"`
	ClientPIDStartMS int64    `json:"client_pid_start_ms"`
	SingleSession    bool     `json:"single_session"`
	Source           string   `json:"source"`
	Execution        string   `json:"execution"`
	ExecutionID      string   `json:"execution_id"`
	Generation       int64    `json:"generation"`
	Key              string   `json:"key"`
	ToolCallID       string   `json:"tool_call_id"`
	Paths            []string `json:"paths"`
	RenameFrom       string   `json:"rename_from"`
	RenameTo         string   `json:"rename_to"`
	Kind             string   `json:"kind"`
	Path             string   `json:"path"`
	RepoWide         bool     `json:"repo_wide"`
	TTLS             int64    `json:"ttl_s"`
	Fence            int64    `json:"fence"`
	All              bool     `json:"all"`
	Token            string   `json:"token"`
	To               []string `json:"to"`
	Subject          string   `json:"subject"`
	Body             string   `json:"body"`
	Attempt          int64    `json:"attempt"`
	RepositoryID     string   `json:"repository_id"`
	After            int64    `json:"after"`
	Limit            int      `json:"limit"`
	ConsumerID       string   `json:"consumer_id"`
	Stream           string   `json:"stream"`
}
type execution struct {
	ID, AgentID, Repo, Host, Client, Session, Worktree, Branch, State, Name string
	Generation, Epoch, Deadline, PID, PIDStart                              int64
	Recovery                                                                sql.NullInt64
	Parent                                                                  sql.NullString
	Single, Heartbeat                                                       bool
}

const executionSelect = `SELECT e.id,e.agent_id,e.repository_id,e.host_id,e.client,e.session_id,e.worktree,e.branch,e.state,a.name,e.generation,e.boot_epoch,e.deadline_tick_ms,e.client_pid,e.client_pid_start_ms,e.recovery_deadline,e.parent_id,e.single_session,e.heartbeat_mode FROM Execution e JOIN Agent a ON a.id=e.agent_id `

func scanExecution(row interface{ Scan(...any) error }) (e execution, err error) {
	err = row.Scan(&e.ID, &e.AgentID, &e.Repo, &e.Host, &e.Client, &e.Session, &e.Worktree, &e.Branch, &e.State, &e.Name, &e.Generation, &e.Epoch, &e.Deadline, &e.PID, &e.PIDStart, &e.Recovery, &e.Parent, &e.Single, &e.Heartbeat)
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
	return map[string]any{"execution_id": e.ID, "generation": e.Generation, "agent": map[string]any{"id": e.AgentID, "name": e.Name}}
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
func remoteAlias(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "ssh" && u.Scheme != "git") {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if !strings.Contains(host, ".") || net.ParseIP(host) != nil || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".") {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), ".git")
	return "remote:" + u.String()
}
func (t *transaction) repository(r Request) (string, error) {
	// Machine-relative remotes are deliberately never global aliases.
	local := "local:" + r.Host + ":" + strings.ReplaceAll(r.Worktree, "\\", "/")
	var repo, root string
	err := t.row("SELECT r.id,r.root_commit FROM RepositoryAlias a JOIN Repository r ON r.id=a.repository_id WHERE a.alias=?", local).Scan(&repo, &root)
	if err == nil {
		if root == "" || r.RootCommit == "" || root == r.RootCommit {
			return repo, nil
		}
		// A reused path with a contradictory root must not inherit the old namespace.
		repo = ""
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
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
			if err = rows.Scan(&candidate, &commit); err != nil {
				rows.Close()
				return "", err
			}
			count++
			if commit != "" && r.RootCommit != "" && commit != r.RootCommit {
				contradiction = true
			}
			repo = candidate
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return "", err
		}
		if count != 1 || contradiction {
			repo = ""
		}
	}
	if repo == "" {
		repo = id("rp_")
		if _, err = t.exec("INSERT INTO Repository(id,root_commit) VALUES(?,?)", repo, r.RootCommit); err != nil {
			return "", err
		}
	}
	if _, err = t.exec("INSERT OR IGNORE INTO RepositoryAlias VALUES(?,?)", local, repo); err != nil {
		return "", err
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
func (t *transaction) session(r Request) (map[string]any, error) {
	if r.Client == "" || r.SessionID == "" || r.Host == "" || r.Cwd == "" {
		return nil, fail("bad_request", "client, session_id, host and cwd are required")
	}
	if r.Worktree == "" {
		r.Worktree = r.Cwd
	}
	e, err := scanExecution(t.row(executionSelect+"WHERE e.host_id=? AND e.client=? AND e.session_id=? AND e.state='active'", r.Host, r.Client, r.SessionID))
	if err == nil {
		if r.Generation != 0 && e.Generation != r.Generation {
			return nil, fail("lost", "session generation no longer exists")
		}
		if e.Recovery.Valid && r.Generation != e.Generation {
			return nil, fail("lost", "recovery requires the prior session generation")
		}
		lease := int64(900000)
		if e.Heartbeat || r.Source == "shim" || r.Source == "watch" || r.Source == "mcp" {
			lease = 60000
			e.Heartbeat = true
		}
		_, err = t.exec("UPDATE Execution SET recovery_deadline=NULL,boot_epoch=?,deadline_tick_ms=?,heartbeat_mode=? WHERE id=?", t.s.epoch, t.s.clock.Tick()+lease, e.Heartbeat, e.ID)
		if err != nil {
			return nil, err
		}
		e.Epoch = t.s.epoch
		e.Recovery = sql.NullInt64{}
		e.Deadline = t.s.clock.Tick() + lease
		// Recovered claims acquire the new epoch; old complete fence tokens remain invalid.
		if _, err = t.exec("UPDATE Claim SET boot_epoch=? WHERE execution_id=? AND revoked_ms IS NULL", t.s.epoch, e.ID); err != nil {
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
	slot := 1
	for {
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
	var parent any
	if r.ParentSessionID != "" {
		var parentID string
		if err = t.row("SELECT id FROM Execution WHERE host_id=? AND client=? AND session_id=? AND repository_id=? AND state='active'", r.Host, r.Client, r.ParentSessionID, repo).Scan(&parentID); err != nil {
			return nil, fail("bad_request", "parent session is not active in this repository")
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
		query += " AND e.single_session=1 AND e.client_pid=? AND e.client_pid_start_ms=?"
		args = append(args, r.ClientPID, r.ClientPIDStartMS)
	}
	rows, err := t.rows(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var matches []execution
	for rows.Next() {
		e, err := scanExecution(rows)
		if err != nil {
			return nil, err
		}
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
	if r.Generation <= 0 {
		return nil, fail("bad_request", "generation is required")
	}
	e, err := t.live(r.ExecutionID, r.Generation)
	if err != nil {
		return nil, err
	}
	if r.Source != "hook" && r.Source != "shim" && r.Source != "watch" {
		return nil, fail("bad_request", "unknown heartbeat source")
	}
	lease := int64(900000)
	strong := e.Heartbeat || r.Source != "hook"
	if strong {
		lease = 60000
	}
	_, err = t.exec("UPDATE Execution SET deadline_tick_ms=?,heartbeat_mode=? WHERE id=?", t.s.clock.Tick()+lease, strong, e.ID)
	return map[string]any{"lease_ms": lease}, err
}
func (t *transaction) end(r Request) (map[string]any, error) {
	var generation int64
	var state string
	if err := t.row("SELECT generation,state FROM Execution WHERE id=?", r.ExecutionID).Scan(&generation, &state); err != nil {
		return nil, fail("lost", "unknown Execution")
	}
	if generation != r.Generation {
		return nil, fail("lost", "wrong generation")
	}
	if state == "ended" {
		return map[string]any{}, nil
	}
	if _, err := t.live(r.ExecutionID, r.Generation); err != nil {
		return nil, err
	}
	if _, err := t.exec("UPDATE Execution SET state='ended' WHERE id=?", r.ExecutionID); err != nil {
		return nil, err
	}
	return map[string]any{}, t.expire()
}
func (t *transaction) agents(repo string) ([]map[string]any, error) {
	rows, err := t.rows(`SELECT a.id,a.name,a.client,a.worktree,COALESCE(e.branch,''),COALESCE(e.state,'offline') FROM Agent a LEFT JOIN Execution e ON e.id=(SELECT id FROM Execution WHERE agent_id=a.id ORDER BY rowid DESC LIMIT 1) WHERE a.repository_id=? ORDER BY a.name`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var a, n, c, w, b, s string
		if err = rows.Scan(&a, &n, &c, &w, &b, &s); err != nil {
			return nil, err
		}
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

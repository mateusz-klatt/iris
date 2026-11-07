package store

import (
	"database/sql"
	"errors"
	"strings"
)

// recipient is a resolved address: the mailbox, its name as the sender sees it,
// and the repository it belongs to.
type recipient struct{ id, name, repo string }

// resolve turns one address into a mailbox (ADR 004):
//   - an agent's name or id, in any repository on the hub;
//   - "human", the sender's own repository's human inbox;
//   - "human:<repository>", another repository's human inbox;
//   - "repo:<repository>", that repository's contact, or its human inbox when it has none.
//
// A repository is named by id, or by name when exactly one repository has it.
func (t *transaction) resolve(e execution, address string) (recipient, error) {
	switch {
	case address == "human":
		id, err := t.human(e.Repo)
		return recipient{id: id, name: "human", repo: e.Repo}, err
	case strings.HasPrefix(address, "human:"):
		repo, err := t.repoAddress(strings.TrimPrefix(address, "human:"))
		if err != nil {
			return recipient{}, err
		}
		id, err := t.human(repo)
		return recipient{id: id, name: "human", repo: repo}, err
	case strings.HasPrefix(address, "repo:"):
		repo, err := t.repoAddress(strings.TrimPrefix(address, "repo:"))
		if err != nil {
			return recipient{}, err
		}
		var a recipient
		err = t.row("SELECT a.id,a.name,a.repository_id FROM Repository r JOIN Agent a ON a.id=r.contact_agent_id WHERE r.id=?", repo).Scan(&a.id, &a.name, &a.repo)
		if errors.Is(err, sql.ErrNoRows) {
			// No contact, or the contact's agent is gone: a person reads it in the console.
			id, herr := t.human(repo)
			return recipient{id: id, name: "human", repo: repo}, herr
		}
		return a, err
	}
	var a recipient
	if err := t.row("SELECT id,name,repository_id FROM Agent WHERE (name=? OR id=?) AND client<>'human'", address, address).Scan(&a.id, &a.name, &a.repo); err != nil {
		return recipient{}, fail("not_found", "no agent on this hub is called "+address)
	}
	return a, nil
}

// repoAddress is a repository named by id, or by a name only one repository has.
func (t *transaction) repoAddress(name string) (string, error) {
	var repo string
	err := t.row("SELECT id FROM Repository WHERE id=?", name).Scan(&repo)
	if err == nil {
		return repo, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	rows, err := t.rows("SELECT id FROM Repository WHERE name=? AND name<>'' LIMIT 2", name)
	if err != nil {
		return "", err
	}
	var found []string
	for rows.Next() {
		rows.Scan(&repo)
		found = append(found, repo)
	}
	err = rows.Err()
	_ = rows.Close()
	switch {
	case err != nil:
		return "", err
	case len(found) == 0:
		return "", fail("not_found", "no repository is called "+name)
	case len(found) > 1:
		return "", fail("ambiguous", "several repositories are called "+name+"; use repo:<repository id>")
	}
	return found[0], nil
}

// reachable is whether a session of the agent can take the mail now: one within
// its lease, or within its recovery window after a hub restart. state='active'
// alone is not enough, because a lost lease is only marked on the next request.
func (t *transaction) reachable(agent string) (bool, error) {
	var n int
	err := t.row(`SELECT COUNT(*) FROM Execution WHERE agent_id=? AND state='active'
 AND (deadline_tick_ms>? OR (recovery_deadline IS NOT NULL AND recovery_deadline>?))`, agent, t.s.clock.Tick(), t.s.clock.Now().UnixMilli()).Scan(&n)
	return n > 0, err
}

// contact sets, or with an empty agent clears, the agent that repo: mail for the
// repository reaches. Only the owner may call it; the hub checks that. The agent
// must be a non-human agent of that repository. A change applies to later sends.
func (t *transaction) contact(r Request) (map[string]any, error) {
	var repo string
	if err := t.row("SELECT id FROM Repository WHERE id=?", r.RepositoryID).Scan(&repo); err != nil {
		return nil, fail("not_found", "unknown repository")
	}
	var agent any
	if r.Agent != "" {
		var id string
		if err := t.row("SELECT id FROM Agent WHERE (name=? OR id=?) AND repository_id=? AND client<>'human'", r.Agent, r.Agent, repo).Scan(&id); err != nil {
			return nil, fail("bad_request", "the contact must be an agent of this repository")
		}
		agent = id
	}
	if _, err := t.exec("UPDATE Repository SET contact_agent_id=? WHERE id=?", agent, repo); err != nil {
		return nil, err
	}
	return map[string]any{"repository_id": repo, "contact": r.Agent}, t.event("", "", "changed", "")
}

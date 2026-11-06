package store

import (
	"net/url"
	"path"
	"strings"
)

func repositoryName(remote, worktree string) string {
	remote = strings.TrimSpace(remote)
	if u, err := url.Parse(remote); err == nil && u.Path != "" {
		remote = u.Path
	}
	// Also covers scp-style remotes, whose colon separates the host and path.
	if _, tail, ok := strings.Cut(remote, ":"); ok {
		remote = tail
	}
	remote = strings.TrimSuffix(strings.TrimRight(remote, "/"), ".git")
	if remote != "" && path.Base(remote) != "." {
		return path.Base(remote)
	}
	return path.Base(strings.ReplaceAll(worktree, "\\", "/"))
}

func (t *transaction) backfillRepositoryNames() error {
	rows, err := t.rows("SELECT id FROM Repository")
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		rows, err = t.rows("SELECT alias FROM RepositoryAlias WHERE repository_id=? ORDER BY rowid", id)
		if err != nil {
			return err
		}
		var remote, first string
		for rows.Next() {
			var alias string
			rows.Scan(&alias)
			if strings.HasPrefix(alias, "remote:") && remote == "" {
				remote = strings.TrimPrefix(alias, "remote:")
			}
			if strings.HasPrefix(alias, "local:") && first == "" {
				_, first, _ = strings.Cut(strings.TrimPrefix(alias, "local:"), ":")
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if _, err = t.exec("UPDATE Repository SET name=? WHERE id=?", repositoryName(remote, first), id); err != nil {
			return err
		}
	}
	return nil
}

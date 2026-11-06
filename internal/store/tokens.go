package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"regexp"
)

var tokenName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

// AddToken returns a fresh secret once. Only its SHA-256 digest is persisted.
// Replacing a revoked name creates an independent credential.
func (s *Store) AddToken(ctx context.Context, name string) (string, error) {
	if !tokenName.MatchString(name) {
		return "", fail("bad_request", "token name must be 1-64 ASCII letters, digits, dots, underscores or hyphens, starting with a letter or digit")
	}
	raw := make([]byte, 32)
	_, _ = rand.Read(raw) // crypto/rand.Read never returns an error
	token := base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(token))
	err := s.transact(ctx, func(t *transaction) error {
		result, err := t.exec(`INSERT INTO HostToken(name,digest,created_ms) VALUES(?,?,?) ON CONFLICT(name) DO UPDATE SET digest=excluded.digest,created_ms=excluded.created_ms,revoked_ms=NULL,last_used_ms=NULL,requests=0 WHERE HostToken.revoked_ms IS NOT NULL`, name, digest[:], s.clock.Now().UnixMilli())
		if err != nil {
			return err
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			return fail("conflict", "token name already exists; revoke it before replacing")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

func (s *Store) ListTokens(ctx context.Context) ([]map[string]any, error) {
	out := []map[string]any{}
	err := s.transact(ctx, func(t *transaction) error {
		rows, err := t.rows("SELECT name,created_ms,revoked_ms,last_used_ms,requests FROM HostToken ORDER BY name")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			var created, count int64
			var revoked, used sql.NullInt64
			rows.Scan(&name, &created, &revoked, &used, &count)
			out = append(out, map[string]any{"name": name, "created_ms": created, "revoked_ms": nullableMillis(revoked), "last_used_ms": nullableMillis(used), "requests": count})
		}
		return rows.Err()
	})
	return out, err
}
func nullableMillis(n sql.NullInt64) any {
	if n.Valid {
		return n.Int64
	}
	return nil
}

// AuthenticateToken checks current revocation state on every request, without a cache.
func (s *Store) AuthenticateToken(ctx context.Context, token string) (string, error) {
	digest := sha256.Sum256([]byte(token))
	var name string
	err := s.transact(ctx, func(t *transaction) error {
		err := t.row("SELECT name FROM HostToken WHERE digest=? AND revoked_ms IS NULL", digest[:]).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return fail("unauthorized", "invalid or revoked bearer token")
		}
		if err != nil {
			return err
		}
		_, err = t.exec("UPDATE HostToken SET last_used_ms=?,requests=requests+1 WHERE name=?", s.clock.Now().UnixMilli(), name)
		return err
	})
	return name, err
}
func (s *Store) RevokeToken(ctx context.Context, name string) (string, error) {
	var digest []byte
	err := s.transact(ctx, func(t *transaction) error {
		err := t.row("SELECT digest FROM HostToken WHERE name=?", name).Scan(&digest)
		if errors.Is(err, sql.ErrNoRows) {
			return fail("not_found", "unknown token name")
		}
		if err != nil {
			return err
		}
		_, err = t.exec("UPDATE HostToken SET revoked_ms=COALESCE(revoked_ms,?) WHERE name=?", s.clock.Now().UnixMilli(), name)
		return err
	})
	return base64.RawURLEncoding.EncodeToString(digest), err
}

// TokenActive checks a digest for streams already authenticated by the hub.
func (s *Store) TokenActive(ctx context.Context, encoded string) bool {
	digest, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return false
	}
	var n int
	err = s.transact(ctx, func(t *transaction) error {
		return t.row("SELECT COUNT(*) FROM HostToken WHERE digest=? AND revoked_ms IS NULL", digest).Scan(&n)
	})
	return err == nil && n == 1
}

package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"path/filepath"
	"testing"
)

func TestTokenLifecycle(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tokens.db")
	s, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddToken(ctx, "../bad"); err == nil {
		t.Fatal("unsafe name accepted")
	}
	secret, err := s.AddToken(ctx, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddToken(ctx, "laptop"); err == nil {
		t.Fatal("active token replaced")
	}
	var digest []byte
	if err = s.db.QueryRow("SELECT digest FROM HostToken").Scan(&digest); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte(secret))
	if string(digest) != string(want[:]) || string(digest) == secret {
		t.Fatal("token not hashed")
	}
	if name, err := s.AuthenticateToken(ctx, secret); err != nil || name != "laptop" {
		t.Fatal(name, err)
	}
	list, err := s.ListTokens(ctx)
	if err != nil || len(list) != 1 || list[0]["requests"] != int64(1) || list[0]["last_used_ms"] == nil {
		t.Fatal(list, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err = s.AuthenticateToken(ctx, secret); err != nil {
		t.Fatal("token did not survive restart", err)
	}
	encoded, err := s.RevokeToken(ctx, "laptop")
	if err != nil || encoded != base64.RawURLEncoding.EncodeToString(want[:]) {
		t.Fatal(encoded, err)
	}
	if _, err = s.RevokeToken(ctx, "laptop"); err != nil {
		t.Fatal("revoke not idempotent", err)
	}
	if _, err = s.AuthenticateToken(ctx, secret); err == nil {
		t.Fatal("revoked secret accepted")
	}
	if s.TokenActive(ctx, encoded) || s.TokenActive(ctx, "!") {
		t.Fatal("revoked or malformed digest accepted")
	}
	if _, err = s.RevokeToken(ctx, "missing"); err == nil {
		t.Fatal("missing token revoked")
	}
	replacement, err := s.AddToken(ctx, "laptop")
	if err != nil || replacement == secret {
		t.Fatal("replacement failed", err)
	}
	if _, err = s.AuthenticateToken(ctx, secret); err == nil {
		t.Fatal("old secret revived")
	}
	list, err = s.ListTokens(ctx)
	if err != nil || list[0]["revoked_ms"] != nil || list[0]["requests"] != int64(0) {
		t.Fatal(list, err)
	}
}

func TestCorruptTokenMetadataFailsClosed(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	if _, err := s.AddToken(ctx, "host"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE HostToken SET created_ms='invalid'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListTokens(ctx); err == nil {
		t.Fatal("corrupt timestamp listed as valid")
	}
}

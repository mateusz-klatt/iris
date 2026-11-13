package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/store"
)

// A hub started on a restored backup says that it runs on a new stream, once: the start consumes
// the backup's restore marker, so the next start on the same database says nothing (ADR 006).
func TestServeSaysWhenItStartsOnARestoredBackup(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("AppData", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "cfg"))
	db := filepath.Join(dir, "hub.db")
	s, err := store.Open(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	raw, _ := sql.Open("sqlite", db)
	if err := store.MarkRestore(context.Background(), raw, time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	original := serveContext
	t.Cleanup(func() { serveContext = original })
	serve := func() string {
		ctx, stop := context.WithCancel(context.Background())
		defer stop()
		serveContext = func() (context.Context, context.CancelFunc) { return ctx, stop }
		errCh := make(chan error, 1)
		_, stderr := capture(t, func() {
			go func() {
				errCh <- run([]string{"serve", "--listen", "127.0.0.1:0", "--db", db, "--token-file", filepath.Join(dir, "hub.token")})
			}()
			stop()
			select {
			case err := <-errCh:
				if err != nil {
					t.Errorf("the hub did not exit cleanly: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("the hub did not stop")
			}
		})
		return stderr
	}
	const said = "restored backup, so the hub runs on a new stream"
	if first := serve(); !strings.Contains(first, said) {
		t.Fatalf("the first start on the backup said %q", first)
	}
	if second := serve(); strings.Contains(second, said) {
		t.Fatalf("a later start said it again: %q", second)
	}
}

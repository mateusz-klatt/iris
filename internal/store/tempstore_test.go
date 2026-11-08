package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// The hub's image is FROM scratch, with no /tmp. A sweep large enough to need SQLite's
// temporary storage then failed with "disk I/O error" (IOERR_GETTEMPPATH), and the hub
// crash-looped for five minutes after the first deploy of ADR 006. The store keeps temporary
// storage in memory, so it never needs a temporary directory, whatever the container has.
func TestASweepNeedsNoTemporaryDirectory(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("SQLITE_TMPDIR", filepath.Join(t.TempDir(), "missing"))
	s, c, _ := testStore(t)
	// As many old events as the first sweep in production met (17,153). Deleting that many in
	// one statement writes a statement journal larger than SQLite keeps in memory.
	if _, err := s.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<20000)
INSERT INTO Event(kind,agent_id,execution_id,object_id,created_ms) SELECT 'changed',NULL,NULL,'object-'||i,0 FROM n`); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Hour)
	removed, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatalf("the sweep needed a temporary directory: %v", err)
	}
	if removed["events"] == 0 {
		t.Fatal("the sweep removed nothing, so it proved nothing")
	}
	var mode int
	if err := s.db.QueryRow("PRAGMA temp_store").Scan(&mode); err != nil || mode != 2 {
		t.Fatalf("temp_store is %d, not MEMORY: %v", mode, err)
	}
}

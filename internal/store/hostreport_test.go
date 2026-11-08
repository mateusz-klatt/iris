package store

import (
	"context"
	"testing"
)

// ADR 007: each host keeps its own report. A report replaces only its host's, and one from a
// doctor that names no host goes to "unknown", where it can never replace a real host's.
func TestDoctorReportsAreKeptPerHost(t *testing.T) {
	s, _, _ := testStore(t)
	put := func(host, status string) {
		call(t, s, "ui_doctor_put", Request{Host: host, CheckedAt: "2026-11-08T00:00:00Z", Clients: []map[string]any{{"client": "claude", "status": status}}})
	}
	put("mac", "ok")
	put("pc", "silent")
	put("", "needs_human") // an older doctor
	put("mac", "slow")     // the Mac again: only its own report changes
	hosts := call(t, s, "ui_doctor_get", Request{})["hosts"].([]map[string]any)
	want := map[string]string{"mac": "slow", "pc": "silent", "unknown": "needs_human"}
	if len(hosts) != len(want) {
		t.Fatal(hosts)
	}
	for _, h := range hosts {
		status := h["clients"].([]any)[0].(map[string]any)["status"]
		if status != want[h["host"].(string)] {
			t.Fatalf("%v: %v", h["host"], status)
		}
	}
	if len(call(t, testStoreOnly(t), "ui_doctor_get", Request{})["hosts"].([]map[string]any)) != 0 {
		t.Fatal("a hub with no report shows one")
	}
}

func testStoreOnly(t *testing.T) *Store {
	t.Helper()
	s, _, _ := testStore(t)
	return s
}

// A stored report that is not JSON is an error, not an empty host.
func TestACorruptHostReportIsAnError(t *testing.T) {
	s, _, _ := testStore(t)
	if _, err := s.db.Exec("INSERT INTO HostReport VALUES('mac','{')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call(context.Background(), "ui_doctor_get", Request{}); err == nil {
		t.Fatal("a corrupt report was read as a host")
	}
}

// The single report kept before ADR 007 came from a doctor that named no host, so the
// migration keeps it under "unknown".
func TestTheOldReportMovesToUnknown(t *testing.T) {
	s, _, file := testStore(t)
	for _, q := range []string{removeConsoleSchema, "DROP TABLE HostReport", "DROP TABLE WorktreeAlias", "DROP INDEX DeliveryReceiptDue", "ALTER TABLE Delivery DROP COLUMN receipt_due_ms", "CREATE TABLE DoctorReport(id INTEGER PRIMARY KEY CHECK(id=1),report TEXT NOT NULL)", `INSERT INTO DoctorReport VALUES(1,'{"checked_at":"x","clients":[]}')`, "PRAGMA user_version=13"} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close()
	s.lock = nil
	n, err := Open(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Close() }()
	hosts := call(t, n, "ui_doctor_get", Request{})["hosts"].([]map[string]any)
	if len(hosts) != 1 || hosts[0]["host"] != "unknown" || hosts[0]["checked_at"] != "x" {
		t.Fatal(hosts)
	}
}

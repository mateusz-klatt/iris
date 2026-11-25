package sessionproof

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

const (
	testExec1 = "ex_0123456789abcdef0123456789abcdef"
	testExec2 = "ex_fedcba9876543210fedcba9876543210"
)

func TestEncodeAndDecodeRecordRoundTrips(t *testing.T) {
	hookRec := Record{
		Version:     FormatVersion,
		Origin:      OriginHook,
		Generation:  1,
		ExecutionID: testExec1,
	}
	raw, err := EncodeRecord(hookRec)
	if err != nil {
		t.Fatalf("EncodeRecord(hook): %v", err)
	}
	got, err := DecodeRecord(raw)
	if err != nil {
		t.Fatalf("DecodeRecord(hook): %v", err)
	}
	if got != hookRec {
		t.Fatalf("hook round trip = %+v, want %+v", got, hookRec)
	}
	if p := got.Pair(); p != (Pair{Generation: 1, ExecutionID: testExec1}) {
		t.Fatalf("hook Pair() = %+v", p)
	}
	if _, ok := got.PriorPair(); ok {
		t.Fatal("hook record unexpectedly returned PriorPair")
	}

	shimRec := Record{
		Version:          FormatVersion,
		Origin:           OriginShim,
		Generation:       2,
		ExecutionID:      testExec2,
		PriorGeneration:  1,
		PriorExecutionID: testExec1,
	}
	raw, err = EncodeRecord(shimRec)
	if err != nil {
		t.Fatalf("EncodeRecord(shim): %v", err)
	}
	// Trailing newline / JSON whitespace is accepted up to 4095 bytes.
	got, err = DecodeRecord(append(raw, '\n'))
	if err != nil {
		t.Fatalf("DecodeRecord(shim): %v", err)
	}
	if got != shimRec {
		t.Fatalf("shim round trip = %+v, want %+v", got, shimRec)
	}
	if p, ok := got.PriorPair(); !ok || p != (Pair{Generation: 1, ExecutionID: testExec1}) {
		t.Fatalf("shim PriorPair() = (%+v, %v)", p, ok)
	}
}

func TestDecodeRecordSizeBounds(t *testing.T) {
	base := `{"version":1,"origin":"hook","generation":1,"execution_id":"` + testExec1 + `"`
	padTo := func(total int) []byte {
		pad := total - len(base) - 1
		if pad < 0 {
			t.Fatalf("target %d too small", total)
		}
		return []byte(base + strings.Repeat(" ", pad) + "}")
	}
	exact4095 := padTo(RecordReadLimit - 1)
	if len(exact4095) != 4095 {
		t.Fatalf("len = %d, want 4095", len(exact4095))
	}
	if _, err := DecodeRecord(exact4095); err != nil {
		t.Fatalf("4095-byte record rejected: %v", err)
	}
	exact4096 := padTo(RecordReadLimit)
	if len(exact4096) != 4096 {
		t.Fatalf("len = %d, want 4096", len(exact4096))
	}
	if _, err := DecodeRecord(exact4096); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("4096-byte record = %v, want ErrInvalidRecord", err)
	}
}

func TestValidateExecutionID(t *testing.T) {
	if err := ValidateExecutionID(testExec1); err != nil {
		t.Fatalf("valid id rejected: %v", err)
	}
	for _, bad := range []string{
		"",
		"ex_",
		"id_0123456789abcdef0123456789abcdef",
		"ex_0123456789abcdef0123456789abcde",
		"ex_0123456789abcdef0123456789abcdef0",
		"ex_0123456789ABCDEF0123456789abcdef",
		"ex_0123456789abcdef0123456789abcdeg",
		"ex_0123456789abcdef0123456789abcde/",
	} {
		if err := ValidateExecutionID(bad); !errors.Is(err, ErrInvalidExecutionID) {
			t.Errorf("ValidateExecutionID(%q) = %v, want ErrInvalidExecutionID", bad, err)
		}
	}
}

func TestDecodeRecordRejectsStrictRuleViolations(t *testing.T) {
	cases := map[string]string{
		"empty":                  "",
		"null":                   "null",
		"array":                  "[]",
		"number":                 "123",
		"bad syntax":             "{",
		"truncated mid-object":   `{"version":1`,
		"dangling key":           `{"version":1,`,
		"non-string key":         `{1:2}`,
		"trailing object":        `{"version":1,"origin":"hook","generation":1,"execution_id":"` + testExec1 + `"}{}`,
		"trailing token":         `{"version":1,"origin":"hook","generation":1,"execution_id":"` + testExec1 + `"} 1`,
		"trailing garbage":       `{"version":1,"origin":"hook","generation":1,"execution_id":"` + testExec1 + `"} x`,
		"duplicate version":      `{"version":1,"version":1,"origin":"hook","generation":1,"execution_id":"` + testExec1 + `"}`,
		"duplicate origin":       `{"version":1,"origin":"hook","origin":"hook","generation":1,"execution_id":"` + testExec1 + `"}`,
		"duplicate generation":   `{"version":1,"origin":"hook","generation":1,"generation":1,"execution_id":"` + testExec1 + `"}`,
		"duplicate execution_id": `{"version":1,"origin":"hook","generation":1,"execution_id":"` + testExec1 + `","execution_id":"` + testExec1 + `"}`,
		"duplicate prior_gen":    `{"version":1,"origin":"shim","generation":2,"execution_id":"` + testExec2 + `","prior_generation":1,"prior_generation":1,"prior_execution_id":"` + testExec1 + `"}`,
		"duplicate prior_exec":   `{"version":1,"origin":"shim","generation":2,"execution_id":"` + testExec2 + `","prior_generation":1,"prior_execution_id":"` + testExec1 + `","prior_execution_id":"` + testExec1 + `"}`,
		"unknown key":            `{"version":1,"origin":"hook","generation":1,"execution_id":"` + testExec1 + `","extra":1}`,
		"case variant key":       `{"Version":1,"origin":"hook","generation":1,"execution_id":"` + testExec1 + `"}`,
		"missing version":        `{"origin":"hook","generation":1,"execution_id":"` + testExec1 + `"}`,
		"missing origin":         `{"version":1,"generation":1,"execution_id":"` + testExec1 + `"}`,
		"missing generation":     `{"version":1,"origin":"hook","execution_id":"` + testExec1 + `"}`,
		"missing execution_id":   `{"version":1,"origin":"hook","generation":1}`,
		"wrong version type":     `{"version":"1","origin":"hook","generation":1,"execution_id":"` + testExec1 + `"}`,
		"unsupported version":    `{"version":2,"origin":"hook","generation":1,"execution_id":"` + testExec1 + `"}`,
		"unknown origin":         `{"version":1,"origin":"watch","generation":1,"execution_id":"` + testExec1 + `"}`,
		"zero generation":        `{"version":1,"origin":"hook","generation":0,"execution_id":"` + testExec1 + `"}`,
		"negative generation":    `{"version":1,"origin":"hook","generation":-1,"execution_id":"` + testExec1 + `"}`,
		"bad execution_id":       `{"version":1,"origin":"hook","generation":1,"execution_id":"ex_short"}`,
		"hook with prior_gen":    `{"version":1,"origin":"hook","generation":2,"execution_id":"` + testExec1 + `","prior_generation":0}`,
		"hook with prior_exec":   `{"version":1,"origin":"hook","generation":2,"execution_id":"` + testExec1 + `","prior_execution_id":""}`,
		"hook with prior null":   `{"version":1,"origin":"hook","generation":2,"execution_id":"` + testExec1 + `","prior_generation":null}`,
		"shim missing prior_gen": `{"version":1,"origin":"shim","generation":2,"execution_id":"` + testExec2 + `","prior_execution_id":"` + testExec1 + `"}`,
		"shim missing prior_id":  `{"version":1,"origin":"shim","generation":2,"execution_id":"` + testExec2 + `","prior_generation":1}`,
		"shim gen 1":             `{"version":1,"origin":"shim","generation":1,"execution_id":"` + testExec2 + `","prior_generation":0,"prior_execution_id":"` + testExec1 + `"}`,
		"shim prior_gen G-2":     `{"version":1,"origin":"shim","generation":3,"execution_id":"` + testExec2 + `","prior_generation":1,"prior_execution_id":"` + testExec1 + `"}`,
		"shim prior_gen G":       `{"version":1,"origin":"shim","generation":2,"execution_id":"` + testExec2 + `","prior_generation":2,"prior_execution_id":"` + testExec1 + `"}`,
		"shim bad prior_id":      `{"version":1,"origin":"shim","generation":2,"execution_id":"` + testExec2 + `","prior_generation":1,"prior_execution_id":"ex_bad"}`,
		"shim same exec id":      `{"version":1,"origin":"shim","generation":2,"execution_id":"` + testExec1 + `","prior_generation":1,"prior_execution_id":"` + testExec1 + `"}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeRecord([]byte(raw)); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("DecodeRecord(%s) = %v, want ErrInvalidRecord", raw, err)
			}
		})
	}
}

func TestEncodeRecordRejectsInvalidRecords(t *testing.T) {
	bad := []Record{
		{Version: 0, Origin: OriginHook, Generation: 1, ExecutionID: testExec1},
		{Version: 1, Origin: OriginHook, Generation: 0, ExecutionID: testExec1},
		{Version: 1, Origin: OriginHook, Generation: 1, ExecutionID: "bad"},
		{Version: 1, Origin: OriginHook, Generation: 2, ExecutionID: testExec1, PriorGeneration: 1},
		{Version: 1, Origin: OriginHook, Generation: 2, ExecutionID: testExec1, PriorExecutionID: testExec2},
		{Version: 1, Origin: OriginShim, Generation: 1, ExecutionID: testExec2, PriorGeneration: 0, PriorExecutionID: testExec1},
		{Version: 1, Origin: OriginShim, Generation: 3, ExecutionID: testExec2, PriorGeneration: 1, PriorExecutionID: testExec1},
		{Version: 1, Origin: OriginShim, Generation: 2, ExecutionID: testExec2, PriorGeneration: 1, PriorExecutionID: "bad"},
		{Version: 1, Origin: OriginShim, Generation: 2, ExecutionID: testExec1, PriorGeneration: 1, PriorExecutionID: testExec1},
		{Version: 1, Origin: "mcp", Generation: 1, ExecutionID: testExec1},
	}
	for i, r := range bad {
		if raw, err := EncodeRecord(r); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("case %d: EncodeRecord(%+v) = (%q, %v), want ErrInvalidRecord", i, r, raw, err)
		}
	}
	// Hook encoding must omit predecessor keys completely.
	raw, err := EncodeRecord(Record{Version: 1, Origin: OriginHook, Generation: 3, ExecutionID: testExec1})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("prior_")) {
		t.Fatalf("hook JSON unexpectedly contains predecessor field: %s", raw)
	}
}

func TestSnapshotHelpers(t *testing.T) {
	empty := Snapshot{
		BasePath:        "/tmp/sess",
		GenerationState: FileAbsent,
		RecordState:     FileSkipped,
		EndedState:      FileAbsent,
	}
	if _, ok := empty.ExactPair(); ok {
		t.Fatal("empty snapshot returned exact pair")
	}
	if got := empty.GenerationFloor(); got != 0 {
		t.Fatalf("empty GenerationFloor = %d", got)
	}
	if empty.AnyDamaged() || len(empty.DamagedPaths()) != 0 {
		t.Fatalf("empty snapshot reported damage: %+v", empty)
	}

	valid := Snapshot{
		BasePath:        "/tmp/sess",
		Generation:      2,
		GenerationState: FileValid,
		Record: Record{
			Version:          1,
			Origin:           OriginShim,
			Generation:       3,
			ExecutionID:      testExec2,
			PriorGeneration:  2,
			PriorExecutionID: testExec1,
		},
		RecordState: FileValid,
		EndedState:  FileAbsent,
	}
	if p, ok := valid.ExactPair(); !ok || p != (Pair{Generation: 3, ExecutionID: testExec2}) {
		t.Fatalf("ExactPair = (%+v, %v)", p, ok)
	}
	if got := valid.GenerationFloor(); got != 3 {
		t.Fatalf("GenerationFloor = %d, want 3", got)
	}

	damaged := Snapshot{
		BasePath:        "/tmp/sess",
		GenerationState: FileDamaged,
		RecordState:     FileDamaged,
		EndedState:      FileDamaged,
	}
	if !damaged.AnyDamaged() {
		t.Fatal("damaged snapshot did not report AnyDamaged")
	}
	gotPaths := damaged.DamagedPaths()
	wantPaths := []string{"/tmp/sess", "/tmp/sess.execution", "/tmp/sess.ended"}
	if len(gotPaths) != len(wantPaths) {
		t.Fatalf("DamagedPaths = %v, want %v", gotPaths, wantPaths)
	}
	for i := range wantPaths {
		if gotPaths[i] != wantPaths[i] {
			t.Fatalf("DamagedPaths[%d] = %q, want %q", i, gotPaths[i], wantPaths[i])
		}
	}
}

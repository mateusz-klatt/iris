package hubclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSuccessionProofCapableAndJSONDecoding(t *testing.T) {
	if !SuccessionProofCapable([]string{"shell_scope", "file_claims", "succession_proof"}) {
		t.Fatal("expected succession_proof capability to be detected")
	}
	if SuccessionProofCapable([]string{"shell_scope", "file_claims"}) {
		t.Fatal("unexpected succession_proof capability")
	}

	var cand Candidate
	raw := `{
		"execution_id":"ex_child","generation":2,"handle":"ex_child","repository_id":"rp_1",
		"agent":{"id":"ag_1","name":"codex-h-w"},"session_id":"S1","worktree":"/w",
		"capabilities":["shell_scope","file_claims","succession_proof"],
		"prior_execution_id":"ex_parent","creation_source":"shim","creation_proof":"shim_succession",
		"hook_seen":false,"lease_ms":60000
	}`
	if err := json.Unmarshal([]byte(raw), &cand); err != nil {
		t.Fatal(err)
	}
	cls, err := cand.Classify()
	if err != nil {
		t.Fatal(err)
	}
	want := Classification{
		Kind:             CandidateShimSuccession,
		PriorExecutionID: "ex_parent",
		PriorGeneration:  1,
		CreationSource:   "shim",
		CreationProof:    "shim_succession",
		HookSeen:         false,
	}
	if cls != want {
		t.Fatalf("got %+v, want %+v", cls, want)
	}
	if !cls.IsHooklessSuccession() {
		t.Fatal("expected IsHooklessSuccession() to be true")
	}

	// Null provenance decodes with Known=true, Valid=false -> CandidateLegacy.
	var legacy Candidate
	rawLegacy := `{
		"execution_id":"ex_old","generation":1,"capabilities":["succession_proof"],
		"prior_execution_id":null,"creation_source":null,"creation_proof":null,"hook_seen":true
	}`
	if err := json.Unmarshal([]byte(rawLegacy), &legacy); err != nil {
		t.Fatal(err)
	}
	legacyCls, err := legacy.Classify()
	if err != nil {
		t.Fatal(err)
	}
	if legacyCls != (Classification{Kind: CandidateLegacy, HookSeen: true}) {
		t.Fatalf("legacy classification=%+v", legacyCls)
	}

	// NullableString and OptionalBool reject invalid types and JSON null booleans.
	var ns NullableString
	if err := json.Unmarshal([]byte(`123`), &ns); err == nil {
		t.Fatal("expected NullableString error on number")
	}
	for _, bad := range []string{`null`, `"true"`} {
		var ob OptionalBool
		if err := json.Unmarshal([]byte(bad), &ob); err == nil {
			t.Fatalf("expected OptionalBool error on %s", bad)
		}
	}
}

func TestCandidateClassifyRulesAndHelpers(t *testing.T) {
	t.Run("unclassified and lease-only legacy replies", func(t *testing.T) {
		leaseOnly := Candidate{LeaseMS: 60000}
		if !leaseOnly.IsLeaseOnly() {
			t.Fatal("expected lease-only candidate")
		}
		cls, err := leaseOnly.Classify()
		if err != nil {
			t.Fatal(err)
		}
		if cls != (Classification{Kind: CandidateUnclassified}) {
			t.Fatalf("got %+v", cls)
		}

		v15Bind := Candidate{
			ExecutionID: "ex_1", Generation: 1, Capabilities: []string{"shell_scope", "file_claims"},
			Provenance: Provenance{HookSeen: OptionalBool{Known: true, Value: true}},
		}
		if v15Bind.IsLeaseOnly() {
			t.Fatal("v0.1.15 bind reply with execution/hook_seen must not look lease-only")
		}
		cls, err = v15Bind.Classify()
		if err != nil {
			t.Fatal(err)
		}
		if cls != (Classification{Kind: CandidateUnclassified, HookSeen: true}) {
			t.Fatalf("got %+v", cls)
		}
	})

	t.Run("ordinary initial and hook-created winners", func(t *testing.T) {
		initCand := Candidate{
			ExecutionID: "ex_1", Generation: 1, Capabilities: []string{SuccessionProofCapability},
			Provenance: Provenance{
				PriorExecutionID: NullableString{Known: true, Valid: false},
				CreationSource:   NullableString{Known: true, Valid: true, Value: "hook"},
				CreationProof:    NullableString{Known: true, Valid: true, Value: "initial"},
				HookSeen:         OptionalBool{Known: true, Value: true},
			},
		}
		initCls, err := initCand.Classify()
		if err != nil {
			t.Fatal(err)
		}
		if initCls.Kind != CandidateOrdinary || initCls.IsHooklessSuccession() || initCls.HookCreatedFrom("ex_0", 1) {
			t.Fatalf("unexpected initial classification: %+v", initCls)
		}

		hookWinner := Candidate{
			ExecutionID: "ex_2", Generation: 2, Capabilities: []string{SuccessionProofCapability},
			Provenance: Provenance{
				PriorExecutionID: NullableString{Known: true, Valid: true, Value: "ex_1"},
				CreationSource:   NullableString{Known: true, Valid: true, Value: "hook"},
				CreationProof:    NullableString{Known: true, Valid: true, Value: "absent"},
				HookSeen:         OptionalBool{Known: true, Value: true},
			},
		}
		winCls, err := hookWinner.Classify()
		if err != nil {
			t.Fatal(err)
		}
		if !winCls.HookCreatedFrom("ex_1", 1) || winCls.HookCreatedFrom("other", 1) || winCls.HookCreatedFrom("ex_1", 2) {
			t.Fatalf("unexpected HookCreatedFrom behavior: %+v", winCls)
		}

		shimUnlocked := Candidate{
			ExecutionID: "ex_2", Generation: 2, Capabilities: []string{SuccessionProofCapability},
			Provenance: Provenance{
				PriorExecutionID: NullableString{Known: true, Valid: true, Value: "ex_1"},
				CreationSource:   NullableString{Known: true, Valid: true, Value: "shim"},
				CreationProof:    NullableString{Known: true, Valid: true, Value: "shim_succession"},
				HookSeen:         OptionalBool{Known: true, Value: true},
			},
		}
		unlockedCls, err := shimUnlocked.Classify()
		if err != nil {
			t.Fatal(err)
		}
		if unlockedCls.IsHooklessSuccession() {
			t.Fatal("shim succession child with hook_seen=true is no longer hookless")
		}
	})

	t.Run("capable hub validation failures", func(t *testing.T) {
		validInit := func() Candidate {
			return Candidate{
				ExecutionID: "ex_2", Generation: 2, Capabilities: []string{SuccessionProofCapability},
				Provenance: Provenance{
					PriorExecutionID: NullableString{Known: true, Valid: true, Value: "ex_1"},
					CreationSource:   NullableString{Known: true, Valid: true, Value: "shim"},
					CreationProof:    NullableString{Known: true, Valid: true, Value: "shim_succession"},
					HookSeen:         OptionalBool{Known: true, Value: false},
				},
			}
		}

		cases := []struct {
			name   string
			mutate func(*Candidate)
		}{
			{"missing execution_id", func(c *Candidate) { c.ExecutionID = "" }},
			{"non-positive generation", func(c *Candidate) { c.Generation = 0 }},
			{"omitted hook_seen", func(c *Candidate) { c.HookSeen.Known = false }},
			{"omitted prior_execution_id", func(c *Candidate) { c.PriorExecutionID.Known = false }},
			{"omitted creation_source", func(c *Candidate) { c.CreationSource.Known = false }},
			{"omitted creation_proof", func(c *Candidate) { c.CreationProof.Known = false }},
			{"null source with non-null proof", func(c *Candidate) { c.CreationSource.Valid = false }},
			{"non-null source with null proof", func(c *Candidate) { c.CreationProof.Valid = false }},
			{"null source/proof with non-null prior", func(c *Candidate) {
				c.CreationSource.Valid = false
				c.CreationProof.Valid = false
			}},
			{"unknown creation_source", func(c *Candidate) { c.CreationSource.Value = "evil" }},
			{"unknown creation_proof", func(c *Candidate) { c.CreationProof.Value = "bogus" }},
			{"absent prior with non-initial proof", func(c *Candidate) { c.PriorExecutionID.Valid = false }},
			{"initial proof with null prior at generation > 1", func(c *Candidate) {
				c.PriorExecutionID.Valid = false
				c.CreationSource.Value = "hook"
				c.CreationProof.Value = "initial"
			}},
			{"empty prior string", func(c *Candidate) { c.PriorExecutionID.Value = "" }},
			{"self prior_execution_id", func(c *Candidate) { c.PriorExecutionID.Value = "ex_2" }},
			{"prior with generation 1", func(c *Candidate) { c.Generation = 1 }},
			{"prior with initial proof", func(c *Candidate) {
				c.CreationSource.Value = "hook"
				c.CreationProof.Value = "initial"
			}},
			{"shim_succession with hook source", func(c *Candidate) { c.CreationSource.Value = "hook" }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				c := validInit()
				tc.mutate(&c)
				if cls, err := c.Classify(); err == nil {
					t.Fatalf("expected classification error for %s, got %+v", tc.name, cls)
				}
			})
		}
	})
}

func TestStage1bClientMethods(t *testing.T) {
	var seenBind BindRequest
	var seenHB HeartbeatRequest
	var seenSucc SucceedRequest
	var seenPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		switch r.URL.Path {
		case "/v1/bind":
			if err := json.NewDecoder(r.Body).Decode(&seenBind); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(`{
				"execution_id":"ex_child","generation":2,"agent":{"id":"ag_1","name":"codex-h-w"},
				"session_id":"S1","worktree":"/w","capabilities":["shell_scope","file_claims","succession_proof"],
				"prior_execution_id":"ex_parent","creation_source":"shim","creation_proof":"shim_succession","hook_seen":false
			}`))
		case "/v1/executions/ex_child/heartbeat":
			if err := json.NewDecoder(r.Body).Decode(&seenHB); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(`{
				"execution_id":"ex_child","generation":2,"agent":{"id":"ag_1","name":"codex-h-w"},
				"session_id":"S1","worktree":"/w","capabilities":["shell_scope","file_claims","succession_proof"],
				"prior_execution_id":"ex_parent","creation_source":"shim","creation_proof":"shim_succession",
				"hook_seen":false,"lease_ms":60000
			}`))
		case "/v1/sessions/succeed/exact":
			if r.Header.Get("Authorization") != "Bearer tok" {
				t.Fatalf("missing bearer token")
			}
			if err := json.NewDecoder(r.Body).Decode(&seenSucc); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(`{
				"execution_id":"ex_child","generation":2,"handle":"ex_child","repository_id":"rp_1",
				"agent":{"id":"ag_1","name":"codex-h-w"},"session_id":"S1","lease_ms":60000,"peers":[],"claims":[],"unread":0,
				"attention":{},"capabilities":["shell_scope","file_claims","succession_proof"],"worktree":"/w",
				"prior_execution_id":"ex_parent","creation_source":"shim","creation_proof":"shim_succession","hook_seen":false
			}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"unknown API route","details":{}}}`))
		}
	}))
	defer srv.Close()

	c := &Client{Config: staticConfig{url: srv.URL, token: "tok"}}

	cand, err := c.Bind(t.Context(), BindRequest{
		Client: "codex", Host: "h", SessionID: "S1", SuccessionProof: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !seenBind.SuccessionProof || cand.ExecutionID != "ex_child" {
		t.Fatalf("unexpected bind request/reply: req=%+v cand=%+v", seenBind, cand)
	}
	if cls, err := cand.Classify(); err != nil || !cls.IsHooklessSuccession() {
		t.Fatalf("bind classify failed: cls=%+v err=%v", cls, err)
	}

	hbCand, err := c.Heartbeat(t.Context(), "ex_child", HeartbeatRequest{
		Generation: 2, Source: "shim", SuccessionProof: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !seenHB.SuccessionProof || hbCand.LeaseMS != 60000 {
		t.Fatalf("unexpected hb request/reply: req=%+v cand=%+v", seenHB, hbCand)
	}

	resp, err := c.Succeed(t.Context(), SucceedRequest{
		Client: "codex", Host: "h", SessionID: "S1", Source: "shim",
		PriorGeneration: 1, PriorExecutionID: "ex_parent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenPath != "/v1/sessions/succeed/exact" || seenSucc.PriorExecutionID != "ex_parent" {
		t.Fatalf("unexpected succeed call: path=%q req=%+v", seenPath, seenSucc)
	}
	succCand := resp.Candidate()
	if resp.SessionID != "S1" || succCand.SessionID != "S1" {
		t.Fatalf("succeed dropped session_id: resp=%q cand=%q", resp.SessionID, succCand.SessionID)
	}
	if cls, err := succCand.Classify(); err != nil || !cls.IsHooklessSuccession() {
		t.Fatalf("succeed classify failed: cls=%+v err=%v", cls, err)
	}

	// Error branches for Bind, Heartbeat (empty id + HTTP error), and OlderHub on 404 route.
	if _, err := c.Heartbeat(t.Context(), "", HeartbeatRequest{Generation: 2, Source: "shim"}); err == nil {
		t.Fatal("expected error for empty heartbeat execution id")
	}

	olderSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"unknown API route","details":{}}}`))
	}))
	defer olderSrv.Close()
	older := &Client{Config: staticConfig{url: olderSrv.URL, token: "tok"}}
	if _, err := older.Bind(t.Context(), BindRequest{Client: "codex", Host: "h"}); !OlderHub(err) {
		t.Fatalf("expected OlderHub on bind 404, got %v", err)
	}
	if _, err := older.Heartbeat(t.Context(), "ex_1", HeartbeatRequest{Generation: 1, Source: "shim"}); !OlderHub(err) {
		t.Fatalf("expected OlderHub on heartbeat 404, got %v", err)
	}
	if _, err := older.Succeed(t.Context(), SucceedRequest{
		Client: "codex", Host: "h", SessionID: "S1", Source: "shim", PriorGeneration: 1, PriorExecutionID: "ex_1",
	}); !OlderHub(err) {
		t.Fatalf("expected OlderHub on succeed 404, got %v", err)
	}
	if !strings.Contains(cand.Worktree, "/w") {
		t.Fatalf("unexpected worktree: %+v", cand)
	}
}

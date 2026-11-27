package wake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/binding"
	"github.com/mateusz-klatt/iris/internal/hook"
	"github.com/mateusz-klatt/iris/internal/hub"
	"github.com/mateusz-klatt/iris/internal/hubclient"
	"github.com/mateusz-klatt/iris/internal/kimiwake"
	"github.com/mateusz-klatt/iris/internal/proc"
	"github.com/mateusz-klatt/iris/internal/sessionproof"
	"github.com/mateusz-klatt/iris/internal/store"
)

const (
	kimiD3Execution = "ex_11111111111111111111111111111111"
	kimiD3Other     = "ex_22222222222222222222222222222222"
)

type kimiD3Request struct{ method, uri, body string }

// kimiD3Trace captures the wire while letting the current hub handle the original
// request. Replacing the handler lets a bound watcher talk to the next hub epoch.
type kimiD3Trace struct {
	handler atomic.Pointer[hub.Hub]
	mu      sync.Mutex
	seen    []kimiD3Request
}

func (trace *kimiD3Trace) record(t *testing.T, r *http.Request) {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Error(err)
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.seen = append(trace.seen, kimiD3Request{r.Method, r.URL.RequestURI(), string(raw)})
}

func (trace *kimiD3Trace) take() []kimiD3Request {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	requests := trace.seen
	trace.seen = nil
	return requests
}

func kimiD3Watcher(t *testing.T, url string, opted bool) (*onceRun, string, string) {
	t.Helper()
	load, dir := pinnedKimiConfig(t, url)
	token := filepath.Join(dir, "hub.token")
	writeWakeProof(t, filepath.Join(dir, "client.json"), fmt.Sprintf(`{"hub_url":%q,"session_succession":%t}`, url, opted))
	pid := os.Getpid()
	self, found := proc.Lookup(pid)
	if !found {
		t.Fatal("the test process has no identity")
	}
	w, ok := newKimiOnce(io.Discard, load, proc.NearestWithoutMarkers, proc.Lookup, os.Getpid)
	if !ok || w.successionEnabled(t.Context()) != opted {
		t.Fatal("watcher did not load pinned succession setting")
	}
	if err := w.store.MapSession(pid, self.StartMS, onceSession, time.Second); err != nil {
		t.Fatal(err)
	}
	writeWakeProof(t, filepath.Join(w.store.Root, "preference"), "on")
	key := kimiwake.Key{PID: pid, StartMS: self.StartMS, Session: onceSession}
	o := &onceRun{w: w, key: key, parent: t.Context()}
	return o, token, wakeGenerationPath(token, "kimi", onceSession)
}

func kimiD3Exact(t *testing.T, base string, record sessionproof.Record) {
	t.Helper()
	record.Version = sessionproof.FormatVersion
	encoded, err := sessionproof.EncodeRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	writeWakeProof(t, base+".execution", string(encoded))
}

func kimiD3Bind(t *testing.T, o *onceRun) (Outcome, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), onceCall)
	defer cancel()
	tick := time.NewTicker(onceBeat)
	defer tick.Stop()
	return o.bind(ctx, tick)
}

func TestKimiWakeD3RestartWireAndTuple(t *testing.T) {
	for _, mode := range []string{"off", "opted heartbeat", "opted initial unbound", "changed tuple"} {
		t.Run(mode, func(t *testing.T) {
			trace := &kimiD3Trace{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				trace.record(t, r)
				trace.handler.Load().ServeHTTP(w, r)
			}))
			defer server.Close()
			filename := filepath.Join(t.TempDir(), "hub.db")
			open := func() *store.Store {
				db, err := store.Open(filename, nil)
				if err != nil {
					t.Fatal(err)
				}
				current := hub.New(db, "synthetic-token", []string{server.Listener.Addr().String()})
				current.Log = log.New(io.Discard, "", 0)
				trace.handler.Store(current)
				return db
			}
			db := open()
			defer func() { _ = db.Close() }()
			opted := mode != "off"
			o, token, base := kimiD3Watcher(t, server.URL, opted)
			req := store.Request{Client: "kimi", SessionID: onceSession, Host: o.w.host, Cwd: o.w.cwd,
				Source: "hook", ClientPID: int64(o.key.PID), ClientPIDStartMS: o.key.StartMS, SingleSession: true}
			out, err := db.Call(t.Context(), "sessions", req)
			if err != nil {
				t.Fatal(err)
			}
			id, _ := out["execution_id"].(string)
			writeWakeProof(t, base, "1")
			if opted {
				kimiD3Exact(t, base, sessionproof.Record{Origin: sessionproof.OriginHook, Generation: 1, ExecutionID: id})
			} else {
				writeWakeProof(t, base+".execution", "invalid JSON")
			}
			wantBind := kimiD3Request{"POST", "/v1/bind", fmt.Sprintf(`{"client":"kimi","client_pid":%d,"client_pid_start_ms":%d,"cwd":%q,"host":%q,"session_id":%q}`, o.key.PID, o.key.StartMS, o.w.cwd, o.w.host, onceSession)}
			if opted {
				wantBind.body = strings.TrimSuffix(wantBind.body, "}") + `,"succession_proof":true}`
			}
			if mode != "opted initial unbound" {
				if out, stop := kimiD3Bind(t, o); stop || !o.bound {
					t.Fatalf("could not bind to the original epoch: %v", out)
				}
				if got := trace.take(); !slices.Equal(got, []kimiD3Request{wantBind}) {
					t.Fatalf("initial lookup differed from the v0.1.15 request: %+v", got)
				}
			}
			if mode == "changed tuple" {
				req.Generation = 1
				req.ClientPIDStartMS++
				if _, err := db.Call(t.Context(), "sessions", req); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db = open()
			var first kimiD3Request
			if mode == "opted initial unbound" {
				if out, stop := kimiD3Bind(t, o); stop || !o.bound {
					t.Fatalf("could not reattach after unbound: %v", out)
				}
				first = wantBind
			} else {
				out, stop := o.beat(t.Context())
				if (mode == "changed tuple" && (!stop || out != Lost)) || (mode != "changed tuple" && stop) {
					t.Fatalf("unexpected outcome after restart: %v %t", out, stop)
				}
				first = kimiD3Request{"POST", "/v1/executions/" + id + "/heartbeat", `{"generation":1,"source":"shim"}`}
				if opted {
					first.uri += "?process_bound=true"
					first.body = `{"generation":1,"source":"shim","succession_proof":true}`
				}
			}
			got := trace.take()
			if len(got) == 0 || got[0] != first {
				t.Fatalf("unexpected request trace after restart: %+v", got)
			}
			if !opted {
				if len(got) != 1 {
					t.Fatalf("OFF heartbeat sent additional requests: %+v", got)
				}
				entries, err := hook.ReadRecentObservations(token, 10)
				if err != nil || len(entries) != 0 {
					t.Fatalf("OFF observed a damaged exact record: %+v %v", entries, err)
				}
			} else {
				want := hubclient.SessionRequest{Client: "kimi", Host: o.w.host, CWD: o.w.cwd, SessionID: onceSession, Generation: 1,
					Source: "shim", ClientPID: o.key.PID, ClientPIDStartMS: o.key.StartMS, SingleSession: true}
				body, err := json.Marshal(want)
				if err != nil {
					t.Fatal(err)
				}
				if len(got) != 2 || got[1] != (kimiD3Request{"POST", "/v1/sessions?process_bound=true", string(body)}) {
					t.Fatalf("opted restart did not attempt only the positive-generation tuple: %+v", got)
				}
			}
			if mode != "changed tuple" && (o.target.Bound.ExecutionID != id || o.target.Bound.Generation != 1 || !o.target.Bound.HookSeen) {
				t.Fatalf("reattach changed the pinned binding: %+v", o.target.Bound)
			}
		})
	}
}

func TestKimiWakeD3ProofBeforeAndAfterRenewal(t *testing.T) {
	for _, phase := range []string{"bind", "before heartbeat", "during heartbeat", "new generation during heartbeat", "exact and legacy damage"} {
		t.Run(phase, func(t *testing.T) {
			trace := &kimiD3Trace{}
			var proofPath atomic.Pointer[string]
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				trace.record(t, r)
				switch phase {
				case "during heartbeat":
					// Even a malformed heartbeat reply must not hide this observation.
					writeWakeProof(t, *proofPath.Load()+".execution", "invalid JSON")
					_, _ = io.WriteString(w, `{"execution_id":"bad-return-pair","generation":3}`)
					return
				case "new generation during heartbeat":
					writeWakeProof(t, *proofPath.Load(), "4")
				}
				_, _ = fmt.Fprintf(w, `{"execution_id":%q,"generation":3,"hook_seen":true,"match":"process_session"}`, kimiD3Execution)
			}))
			defer server.Close()
			o, token, proof := kimiD3Watcher(t, server.URL, true)
			proofPath.Store(&proof)
			if phase == "bind" || phase == "before heartbeat" || phase == "exact and legacy damage" {
				writeWakeProof(t, proof+".execution", "invalid JSON")
			}
			if phase == "exact and legacy damage" {
				writeWakeProof(t, proof, "invalid generation")
			}
			var out Outcome
			var stop bool
			want := 1
			if phase == "bind" {
				out, stop = kimiD3Bind(t, o)
			} else {
				o.bound = true
				o.target.Bound = binding.Bound{ExecutionID: kimiD3Execution, Generation: 3, HookSeen: true}
				out, stop = o.beat(t.Context())
				if phase == "before heartbeat" || phase == "exact and legacy damage" {
					want = 0
				}
			}
			if got := trace.take(); out != Lost || !stop || len(got) != want {
				t.Fatalf("damaged proof: outcome %v %t, requests %+v", out, stop, got)
			}
			wantObservation := hook.DamagedGenerationProof + " at " + proof + ".execution"
			if phase == "new generation during heartbeat" {
				wantObservation = hook.BindingGenerationRefused + " at " + proof
			}
			entries, err := hook.ReadRecentObservations(token, 10)
			if err != nil || len(entries) != 1 || entries[0].Error != wantObservation {
				t.Fatalf("wrong exact-proof observation: %+v %v", entries, err)
			}
		})
	}
}

func TestKimiWakeD3DiscardsInitialRecoveryWhenExactProofChanges(t *testing.T) {
	for _, change := range []string{"changed lineage", "damaged record"} {
		t.Run(change, func(t *testing.T) {
			trace := &kimiD3Trace{}
			var proofPath atomic.Pointer[string]
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				trace.record(t, r)
				if r.URL.Path == "/v1/bind" {
					w.WriteHeader(http.StatusConflict)
					_, _ = io.WriteString(w, `{"error":{"code":"unbound"}}`)
					return
				}
				base := *proofPath.Load()
				if change == "changed lineage" {
					kimiD3Exact(t, base, sessionproof.Record{Origin: sessionproof.OriginShim, Generation: 3, ExecutionID: kimiD3Execution, PriorGeneration: 2, PriorExecutionID: kimiD3Other})
				} else {
					writeWakeProof(t, base+".execution", "damaged")
				}
				_, _ = fmt.Fprintf(w, `{"execution_id":%q,"generation":3}`, kimiD3Execution)
			}))
			defer server.Close()
			o, token, base := kimiD3Watcher(t, server.URL, true)
			proofPath.Store(&base)
			kimiD3Exact(t, base, sessionproof.Record{Origin: sessionproof.OriginHook, Generation: 3, ExecutionID: kimiD3Execution})
			out, stop := kimiD3Bind(t, o)
			got := trace.take()
			if !stop || out != Lost || o.bound || len(got) != 2 || got[1].uri != "/v1/sessions?process_bound=true" {
				t.Fatalf("watcher admitted the changed local proof: %v %t, requests %+v", out, stop, got)
			}
			entries, err := hook.ReadRecentObservations(token, 10)
			if err != nil || len(entries) != 1 || entries[0].Error != hook.DamagedGenerationProof+" at "+base+".execution" {
				t.Fatalf("changed exact proof was not reported: %+v %v", entries, err)
			}
		})
	}
}

func TestKimiWakeD3AdmitsOnlyItsExactProof(t *testing.T) {
	for _, tc := range []struct {
		name, suffix string
		record       sessionproof.Record
		class        hubclient.Classification
		live, ended  int
		lost         bool
	}{
		{name: "ended generation", suffix: ".ended", ended: 3, lost: true},
		{name: "newer legacy generation", live: 4, lost: true},
		{name: "newer exact generation", suffix: ".execution", record: sessionproof.Record{Origin: sessionproof.OriginHook, Generation: 4, ExecutionID: kimiD3Other}, lost: true},
		{name: "forked exact pair", suffix: ".execution", record: sessionproof.Record{Origin: sessionproof.OriginHook, Generation: 3, ExecutionID: kimiD3Other}, lost: true},
		{name: "ordinary row with a shim record", suffix: ".execution", record: sessionproof.Record{Origin: sessionproof.OriginShim, Generation: 3, ExecutionID: kimiD3Execution, PriorGeneration: 2, PriorExecutionID: kimiD3Other}, class: hubclient.Classification{Kind: hubclient.CandidateOrdinary}, lost: true},
		{name: "shim child with changed predecessor", suffix: ".execution", record: sessionproof.Record{Origin: sessionproof.OriginShim, Generation: 3, ExecutionID: kimiD3Execution, PriorGeneration: 2, PriorExecutionID: kimiD3Other}, class: hubclient.Classification{Kind: hubclient.CandidateShimSuccession, PriorGeneration: 2, PriorExecutionID: "ex_33333333333333333333333333333333"}, lost: true},
		{name: "ended predecessor", suffix: ".ended", record: sessionproof.Record{Origin: sessionproof.OriginShim, Generation: 3, ExecutionID: kimiD3Execution, PriorGeneration: 2, PriorExecutionID: kimiD3Other}, ended: 2, lost: true},
		{name: "matching hooked shim child", record: sessionproof.Record{Origin: sessionproof.OriginShim, Generation: 3, ExecutionID: kimiD3Execution, PriorGeneration: 2, PriorExecutionID: kimiD3Other}, class: hubclient.Classification{Kind: hubclient.CandidateShimSuccession, PriorGeneration: 2, PriorExecutionID: kimiD3Other}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, token, base := kimiD3Watcher(t, "http://127.0.0.1:9", true)
			if tc.live > 0 {
				writeWakeProof(t, base, strconv.Itoa(tc.live))
			}
			if tc.ended > 0 {
				writeWakeProof(t, base+".ended", strconv.Itoa(tc.ended))
			}
			if tc.record.Generation > 0 {
				kimiD3Exact(t, base, tc.record)
			}
			b := binding.Bound{ExecutionID: kimiD3Execution, Generation: 3, Class: tc.class, Classified: tc.class.Kind != ""}
			_, err := o.checkWakeProof(t.Context(), b)
			if hubclient.IsCode(err, "lost") != tc.lost || !tc.lost && err != nil {
				t.Fatalf("unexpected proof result: %v", err)
			}
			if tc.lost {
				requireWakeObservation(t, token, "kimi", base+tc.suffix)
				if tc.suffix == ".execution" {
					entries, err := hook.ReadRecentObservations(token, 10)
					if err != nil || !strings.HasPrefix(entries[0].Error, hook.DamagedGenerationProof+" at ") {
						t.Fatalf("lineage mismatch was not classified as damage: %+v %v", entries, err)
					}
				}
			}
		})
	}
}

func TestKimiWakeD3RetriesLegacyDamageAndLockContention(t *testing.T) {
	trace := &kimiD3Trace{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trace.record(t, r)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	o, token, base := kimiD3Watcher(t, server.URL, true)
	o.bound = true
	o.target.Bound = binding.Bound{ExecutionID: kimiD3Execution, Generation: 3, HookSeen: true}
	writeWakeProof(t, base, "3")
	unlock, err := o.w.sessions.Lock(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	out, stop := o.beat(t.Context())
	unlock()
	if stop || len(trace.take()) != 0 {
		t.Fatalf("contended proof renewed or ended the watcher: %v %t", out, stop)
	}
	for _, file := range []struct{ suffix, clean string }{{"", "3"}, {".ended", "2"}} {
		writeWakeProof(t, base+file.suffix, "damaged")
		if out, stop = o.beat(t.Context()); stop || len(trace.take()) != 0 {
			t.Fatalf("unreadable %s renewed or ended the watcher: %v %t", base+file.suffix, out, stop)
		}
		requireWakeObservation(t, token, "kimi", base+file.suffix)
		writeWakeProof(t, base+file.suffix, file.clean)
		if out, stop = o.beat(t.Context()); stop || len(trace.take()) != 1 {
			t.Fatalf("watcher could not renew when %s was readable: %v %t", base+file.suffix, out, stop)
		}
	}
}

func TestKimiWakeD3DiscardsInadmissibleRecovery(t *testing.T) {
	for _, tc := range []struct {
		name            string
		heartbeatCode   string
		deadOn          string
		mapErrorOn      string
		wrongExecution  bool
		exact           bool
		wantReattach    bool
		wantLost        bool
		wantObservation bool
	}{
		{name: "wrong heartbeat pair", wrongExecution: true, wantLost: true},
		{name: "temporary heartbeat failure", heartbeatCode: "unavailable"},
		{name: "parent changed before reattach", heartbeatCode: "lost", deadOn: "heartbeat", wantLost: true},
		{name: "parent changed during reattach", heartbeatCode: "lost", deadOn: "reattach", wantReattach: true, wantLost: true},
		{name: "parent mapping unreadable before reattach", heartbeatCode: "lost", mapErrorOn: "heartbeat"},
		{name: "parent mapping unreadable during reattach", heartbeatCode: "lost", mapErrorOn: "reattach", wantReattach: true},
		{name: "reattach crossed an exact pair", heartbeatCode: "lost", exact: true, wrongExecution: true, wantReattach: true, wantLost: true, wantObservation: true},
		{name: "reattach crossed a cached pair", heartbeatCode: "lost", wrongExecution: true, wantReattach: true, wantLost: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := &kimiD3Trace{}
			var dead, unreadable atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				trace.record(t, r)
				if strings.HasSuffix(r.URL.Path, "/heartbeat") {
					if tc.deadOn == "heartbeat" {
						dead.Store(true)
					}
					if tc.mapErrorOn == "heartbeat" {
						unreadable.Store(true)
					}
					if tc.heartbeatCode != "" {
						w.WriteHeader(http.StatusConflict)
						_, _ = fmt.Fprintf(w, `{"error":{"code":%q}}`, tc.heartbeatCode)
						return
					}
				} else {
					if tc.deadOn == "reattach" {
						dead.Store(true)
					}
					if tc.mapErrorOn == "reattach" {
						unreadable.Store(true)
					}
				}
				id := kimiD3Execution
				if tc.wrongExecution {
					id = kimiD3Other
				}
				_, _ = fmt.Fprintf(w, `{"execution_id":%q,"generation":3,"hook_seen":true}`, id)
			}))
			defer server.Close()
			o, token, base := kimiD3Watcher(t, server.URL, true)
			o.bound = true
			o.target.Bound = binding.Bound{ExecutionID: kimiD3Execution, Generation: 3, HookSeen: true}
			alive := o.w.alive
			o.w.alive = func(pid int, start int64) bool { return !dead.Load() && alive(pid, start) }
			mapped := o.w.mapped
			o.w.mapped = func(pid int, start int64) (string, bool, error) {
				if unreadable.Load() {
					return "", false, errKimiMappingUnclear
				}
				return mapped(pid, start)
			}
			writeWakeProof(t, base, "3")
			if tc.exact {
				kimiD3Exact(t, base, sessionproof.Record{Origin: sessionproof.OriginHook, Generation: 3, ExecutionID: kimiD3Execution})
			}
			out, stop := o.beat(t.Context())
			got := trace.take()
			want := 1
			if tc.wantReattach {
				want++
			}
			if stop != tc.wantLost || (tc.wantLost && out != Lost) || len(got) != want || o.target.Bound.ExecutionID != kimiD3Execution {
				t.Fatalf("outcome %v %t, requests %+v, target %+v", out, stop, got, o.target)
			}
			if tc.wantObservation {
				requireWakeObservation(t, token, "kimi", base+".execution")
			}
		})
	}
}

func TestKimiWakeD3UnboundRecoveryRequiresUsableLocalGeneration(t *testing.T) {
	for _, name := range []string{"end marker alone", "damaged exact record", "generation ended"} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("unusable local proof caused a recovery request")
			}))
			defer server.Close()
			o, token, base := kimiD3Watcher(t, server.URL, true)
			writeWakeProof(t, base+".ended", "3")
			unbound := &hubclient.Error{Code: "unbound"}
			want := error(unbound)
			switch name {
			case "damaged exact record":
				writeWakeProof(t, base+".execution", "damaged")
				want = errKimiProofLost
			case "generation ended":
				writeWakeProof(t, base, "3")
				want = errKimiProofLost
			}
			if _, err := o.recoverUnbound(t.Context(), unbound); !errors.Is(err, want) {
				t.Fatalf("recovery returned %v, want %v", err, want)
			}
			if name == "damaged exact record" {
				requireWakeObservation(t, token, "kimi", base+".execution")
			}
		})
	}
}

func TestKimiWakeD3RechecksProofBeforeAnnouncingAttention(t *testing.T) {
	for _, mode := range []string{"off with exact damage", "opted with exact damage", "opted with legacy damage"} {
		t.Run(mode, func(t *testing.T) {
			trace := &kimiD3Trace{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				trace.record(t, r)
				if r.URL.Path == "/v1/bind" {
					_, _ = fmt.Fprintf(w, `{"execution_id":%q,"generation":3,"hook_seen":true,"match":"process_session"}`, kimiD3Execution)
					return
				}
				_, _ = io.WriteString(w, `{"stream":"s","high_water":1,"attention":{"unread":1,"identity":{"stream":"s","cursor":1}}}`)
			}))
			defer server.Close()
			o, token, base := kimiD3Watcher(t, server.URL, mode != "off with exact damage")
			if own, err := o.w.store.Own(o.key, o.w.self, time.Second); err != nil || !own {
				t.Fatalf("could not own watcher record: %t %v", own, err)
			}
			var output bytes.Buffer
			o.w.out = &output
			if out, stop := kimiD3Bind(t, o); stop || !o.bound {
				t.Fatalf("watcher did not bind to a healthy generation: %v %t", out, stop)
			}
			trace.take()
			o.stream, o.cursor, o.held = "s", 1, map[string]int64{"inbox_changed": 1}
			path := base + ".execution"
			if mode == "opted with legacy damage" {
				path = base
			}
			writeWakeProof(t, path, "damaged")
			out, stop := o.decide(t.Context())
			switch mode {
			case "opted with exact damage":
				if !stop || out != Lost || output.Len() != 0 || len(trace.take()) != 1 {
					t.Fatalf("exact-proof damage announced attention: %v %t %q", out, stop, output.String())
				}
				requireWakeObservation(t, token, "kimi", path)
				return
			case "opted with legacy damage":
				if stop || output.Len() != 0 || len(trace.take()) != 1 {
					t.Fatalf("legacy-proof damage announced attention: %v %t %q", out, stop, output.String())
				}
				requireWakeObservation(t, token, "kimi", path)
				writeWakeProof(t, path, "3")
				out, stop = o.decide(t.Context())
			}
			got := trace.take()
			if !stop || out != Attention || output.String() != attentionLine+"\n" || len(got) != 2 || !strings.Contains(got[1].uri, "consumer_id=iris-watch") {
				t.Fatalf("watcher could not announce and checkpoint: %v %t %q, %+v", out, stop, output.String(), got)
			}
		})
	}
}

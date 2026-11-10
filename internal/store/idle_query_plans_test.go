package store

import (
	"fmt"
	"strings"
	"testing"
)

// queryPlan is SQLite's plan for query, one detail per step.
func queryPlan(t *testing.T, s *Store, query string, args ...any) []string {
	t.Helper()
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return details
}

// Migration 023 reads Event once, however many events and pending deliveries there are. One pass
// gathers each delivery's newest inbox_changed event, and each delivery then finds its own by
// key; no delivery reads the events again.
func TestTheAttentionMigrationReadsTheEventsOnce(t *testing.T) {
	s, _, _ := testStore(t)
	for _, query := range []string{attentionHighWaterSQL, attentionNewestEventSQL} {
		details := queryPlan(t, s, query)
		plan := strings.Join(details, "; ")
		t.Log(plan)
		scans := 0
		for _, detail := range details {
			if strings.Contains(detail, "CORRELATED") {
				t.Errorf("a step runs once per row: %s", plan)
			}
			if strings.HasPrefix(detail, "SCAN Event") {
				scans++
			}
		}
		if scans > 1 {
			t.Errorf("Event is read %d times: %s", scans, plan)
		}
	}
}

func TestIdleCoordinationPlansAvoidScanningHistoricalRows(t *testing.T) {
	for _, history := range []int{200, 2000} {
		t.Run(fmt.Sprint(history), func(t *testing.T) {
			s, clock, _ := testStore(t)
			r, registered := session(t, s, 1)
			agent := registered["agent"].(map[string]any)["id"].(string)
			repo := registered["repository_id"].(string)
			call(t, s, "claim", claimRequest(r, "live-claim", "live.go", "exact"))
			call(t, s, "messages", Request{ExecutionID: r.ExecutionID, Generation: r.Generation, Key: "live-mail", To: []string{agent}, Body: "live attention"})
			tx, err := s.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			for i := range history {
				ex, message, delivery := fmt.Sprint("plan-ex-", i), fmt.Sprint("plan-mail-", i), fmt.Sprint("plan-delivery-", i)
				for _, statement := range []struct {
					query string
					args  []any
				}{
					{`INSERT INTO Execution(id,agent_id,repository_id,host_id,client,session_id,generation,worktree,branch,boot_epoch,deadline_tick_ms,state,client_pid,client_pid_start_ms,single_session) VALUES(?,?,?,'host','codex',?,1,'/repo/w1','old',1,0,'ended',0,0,1)`, []any{ex, agent, repo, ex}},
					{`INSERT INTO Claim(execution_id,repository_id,kind,path,path_key,origin,boot_epoch,revoked_ms) VALUES(?,?,'exact',?,?,'explicit',1,1)`, []any{ex, repo, ex, ex}},
					{`INSERT INTO Message(id,sender_execution_id,repository_id,thread_id,subject,body,created_ms) VALUES(?,?,?,?,'old','historical content',1)`, []any{message, ex, repo, message}},
					{`INSERT INTO Delivery(id,message_id,recipient_agent_id,execution_id,attempt,attention) VALUES(?,?,?,?,1,0)`, []any{delivery, message, agent, ex}},
					{`INSERT INTO Ack(delivery_id,execution_id,attempt,acked_ms) VALUES(?,?,1,1)`, []any{delivery, ex}},
					{`INSERT INTO Event(kind,agent_id,object_id,created_ms) VALUES('inbox_changed',?,?,1)`, []any{agent, delivery}},
				} {
					if _, err := tx.Exec(statement.query, statement.args...); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			for _, query := range []struct {
				name, sql string
				args      []any
				indexes   []string
			}{
				{"execution expiry", expireExecutionsSQL, []any{clock.Now().UnixMilli(), s.epoch, clock.Tick()}, []string{"execution_active_deadline"}},
				{"claim expiry", expiredClaimsSQL, []any{clock.Now().UnixMilli()}, []string{"claim_unrevoked_execution"}},
				{"transition requeue", requeueExecutionSQL, []any{r.ExecutionID}, []string{"delivery_execution", "sqlite_autoindex_Ack_1"}},
				{"missed receipt", missedReceiptsSQL, []any{clock.Now().UnixMilli()}, []string{"DeliveryReceiptDue"}},
				{"console expiry", expireConsoleSessionsSQL, []any{clock.Now().UnixMilli()}, []string{"console_session_expiry"}},
				{"attention count", pendingDeliveriesSQL, []any{agent}, []string{"delivery_attention_recipient"}},
				{"pending mail and its identity", pendingMailSQL, []any{agent}, []string{"delivery_attention_recipient"}},
				{"freed paths and their identity", freedPathsSQL, []any{agent, r.ExecutionID}, []string{"event_recipient_cursor"}},
				{"event page", agentEventsSQL, []any{int64(0), agent, r.ExecutionID, 100}, []string{"event_recipient_cursor", "delivery_attention_recipient"}},
			} {
				details := queryPlan(t, s, query.sql, query.args...)
				plan := strings.Join(details, "; ")
				t.Logf("%s with %d historical rows: %s", query.name, history, plan)
				for _, index := range query.indexes {
					if !strings.Contains(plan, index) {
						t.Errorf("%s does not use %s: %s", query.name, index, plan)
					}
				}
				for _, detail := range details {
					if detail == "SCAN Execution" || detail == "SCAN Delivery" || detail == "SCAN c" {
						t.Errorf("%s scans history: %s", query.name, plan)
					}
				}
			}
		})
	}
}

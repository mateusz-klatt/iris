package store

import (
	"strings"
	"testing"
)

// The overview gives each agent its own unread count and reads deliveries once for all of
// them. Counted per agent, every overview read the whole Delivery table once for each agent.
func TestOverviewCountsUnreadMailWithOneReadOfDeliveries(t *testing.T) {
	s, _, _ := testStore(t)
	a, joinedA := session(t, s, 1)
	b, joinedB := session(t, s, 2)
	_, joinedC := session(t, s, 3)
	name := func(joined map[string]any) string { return joined["agent"].(map[string]any)["name"].(string) }
	for _, m := range []Request{mailTo(a, "one", name(joinedB)), mailTo(a, "two", name(joinedB)), mailTo(a, "three", name(joinedB)), mailTo(a, "four", name(joinedC))} {
		call(t, s, "messages", m)
	}
	first := call(t, s, "inbox", b)["deliveries"].([]map[string]any)[0]
	call(t, s, "ack", Request{ExecutionID: b.ExecutionID, Generation: b.Generation, Attempt: number(first["attempt"]), Key: first["id"].(string)})
	want := map[string]int{name(joinedA): 0, name(joinedB): 2, name(joinedC): 1}
	agents := call(t, s, "ui_overview", Request{})["agents"].([]map[string]any)
	if len(agents) != len(want) {
		t.Fatalf("agents %v", agents)
	}
	for _, agent := range agents {
		if got := agent["unread"]; got != want[agent["name"].(string)] {
			t.Errorf("%s has %v unread, want %d", agent["name"], got, want[agent["name"].(string)])
		}
	}

	rows, err := s.db.Query("EXPLAIN QUERY PLAN " + overviewAgentsSQL + "1=1 ORDER BY a.name")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	parent, detail := map[int]int{}, map[int]string{}
	for rows.Next() {
		var id, up, unused int
		var text string
		if err := rows.Scan(&id, &up, &unused, &text); err != nil {
			t.Fatal(err)
		}
		parent[id], detail[id] = up, text
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	reads := 0
	for id, text := range detail {
		if !strings.HasPrefix(text, "SCAN d") {
			continue
		}
		reads++
		for up := parent[id]; up != 0; up = parent[up] {
			if strings.HasPrefix(detail[up], "CORRELATED") {
				t.Errorf("deliveries are read once for each agent: %v", detail)
			}
		}
	}
	if reads != 1 {
		t.Errorf("deliveries are read %d times in the plan: %v", reads, detail)
	}
}

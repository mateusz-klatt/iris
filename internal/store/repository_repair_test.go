package store

import (
	"fmt"
	"testing"
)

// This reproduces the shape of the reported split without reading production:
// historical remote roots and four hosts with separate ids for the current root.
func TestRepositoryRepairFourHostsAfterRewrite(t *testing.T) {
	s, _, _ := testStore(t)
	const currentRoot = "77888185d6ac7c6a857fcd670bf0335918f80b7e"
	for i, root := range []string{"0f101bb", "e681a54"} {
		call(t, s, "sessions", rewriteRegistration(fmt.Sprintf("archive-%d", i), "historical", root))
	}
	hosts := []string{"linux", "wsl", "mac", "windows"}
	registrations := make([]Request, len(hosts))
	identities := make([]map[string]any, len(hosts))
	executions := make([]Request, len(hosts))
	for i, host := range hosts {
		r := rewriteRegistration(host, "current", currentRoot)
		// The old implementation created each host-local identity separately.
		r.RemoteURL = ""
		identities[i] = call(t, s, "sessions", r)
		if _, err := s.db.Exec("INSERT INTO RepositoryAlias VALUES(?,?)", remoteAlias(rewriteRemote), identities[i]["repository_id"]); err != nil {
			t.Fatal(err)
		}
		r.RemoteURL = rewriteRemote
		registrations[i] = r
		executions[i] = Request{ExecutionID: identities[i]["execution_id"].(string), Generation: identities[i]["generation"].(int64)}
		call(t, s, "claim", claimRequest(executions[i], "work", "src/"+host+".go", "exact"))
	}
	for i := range hosts {
		next := identities[(i+1)%len(hosts)]["agent"].(map[string]any)["name"].(string)
		historySend(t, s, executions[i], next, "split-mail", "", "work before repair", "mail from "+hosts[i])
	}
	into := identities[0]["repository_id"].(string)
	for _, identity := range identities[1:] {
		from := identity["repository_id"].(string)
		before := databaseState(t, s)
		plan := call(t, s, "merge", mergeRequest(from, into, true))
		if databaseState(t, s) != before {
			t.Fatal("rehearsal changed the split database")
		}
		counts := plan["counts"].(map[string]int)
		if counts["Agent"] != 1 || counts["Execution"] != 1 || counts["Claim"] != 1 || counts["Message"] != 1 {
			t.Fatal("plan did not describe the host's durable state", plan)
		}
		call(t, s, "merge", mergeRequest(from, into, false))
	}
	for i, host := range hosts {
		got := call(t, s, "validate", executions[i])
		if got["execution_id"] != identities[i]["execution_id"] || got["generation"] != identities[i]["generation"] || got["repository_id"] != into || got["agent"].(map[string]any)["id"] != identities[i]["agent"].(map[string]any)["id"] {
			t.Fatalf("host %s lost its Execution, generation or mailbox: %v", host, got)
		}
		again := call(t, s, "sessions", registrations[i])
		if again["execution_id"] != identities[i]["execution_id"] || again["repository_id"] != into || len(again["peers"].([]map[string]any)) != len(hosts) {
			t.Fatalf("host %s split again or missed peers: %v", host, again)
		}
		next := (i + 1) % len(hosts)
		target := "src/" + hosts[next] + ".go"
		expectError(t, s, "claim", claimRequest(executions[i], "after-repair", target, "exact"), "conflict")
		guard := registrations[i]
		guard.Paths = []string{target}
		if conflicts := call(t, s, "check", guard)["conflicts"].([]map[string]any); len(conflicts) != 1 {
			t.Fatalf("host %s's guard missed the repaired cross-host claim: %v", host, conflicts)
		}
		mail := call(t, s, "inbox", executions[i])["deliveries"].([]map[string]any)
		previous := hosts[(i+len(hosts)-1)%len(hosts)]
		if len(mail) != 1 || mail[0]["body"] != "mail from "+previous {
			t.Fatalf("host %s lost its mailbox's pre-repair delivery: %v", host, mail)
		}
		if history := call(t, s, "history", executions[i])["messages"].([]map[string]any); len(history) != 2 {
			t.Fatalf("host %s lost sent or received history: %v", host, history)
		}
	}
	// Only the selected current history coalesces. Historical roots remain distinct.
	if got := count(t, s, "SELECT COUNT(*) FROM Repository"); got != 3 {
		t.Fatalf("registration after repair left %d repositories, want three histories", got)
	}
	fifth := call(t, s, "sessions", rewriteRegistration("fifth", "current", currentRoot))
	if fifth["repository_id"] != into || len(fifth["peers"].([]map[string]any)) != 5 {
		t.Fatal("a new host split despite a unique current-root remote", fifth)
	}
	if got := count(t, s, "SELECT COUNT(*) FROM Repository"); got != 3 {
		t.Fatal("a new host created another identity", got)
	}
}

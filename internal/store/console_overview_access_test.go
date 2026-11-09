package store

import "testing"

func TestConsoleOverviewDescribesCurrentRepositoryAccess(t *testing.T) {
	s, _, ao, _, bo := crossRepo(t)
	origin, target := ao["repository_id"].(string), bo["repository_id"].(string)
	reader := createTestConsoleAccount(t, s, "Repository reader", "member")
	principal := readConsolePrincipal(t, s, reader)
	if repositories := accountRead(t, s, principal, "ui_overview", Request{})["repositories"].([]map[string]any); len(repositories) != 0 {
		t.Fatal("no-grant account received repository controls", repositories)
	}
	giveReadGrant(t, s, reader, origin, "ro")
	giveReadGrant(t, s, reader, target, "rw")
	access := func(out map[string]any) map[string]any {
		result := map[string]any{}
		for _, repository := range out["repositories"].([]map[string]any) {
			result[repository["id"].(string)] = repository["console_access"]
		}
		return result
	}
	got := access(accountRead(t, s, principal, "ui_overview", Request{}))
	if len(got) != 2 || got[origin] != "ro" || got[target] != "rw" {
		t.Fatal("member controls do not match current grants", got)
	}
	if _, err := s.db.Exec("UPDATE ConsoleRepositoryAccess SET mode='ro' WHERE account_id=? AND repository_id=?", reader.ID, target); err != nil {
		t.Fatal(err)
	}
	if got := access(accountRead(t, s, principal, "ui_overview", Request{})); got[target] != "ro" {
		t.Fatal("overview retained earlier write controls", got)
	}
	operator := createTestConsoleAccount(t, s, "Repository operator", "operator")
	got = access(accountRead(t, s, readConsolePrincipal(t, s, operator), "ui_overview", Request{}))
	if len(got) != 2 || got[origin] != "rw" || got[target] != "rw" {
		t.Fatal("operator controls incorrectly require grants", got)
	}
	got = access(call(t, s, "ui_overview", Request{}))
	if len(got) != 2 || got[origin] != nil || got[target] != nil {
		t.Fatal("legacy session gained account metadata", got)
	}
}

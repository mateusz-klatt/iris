package hub

import "testing"

func TestConsoleAccessRequiresAnExplicitGrantList(t *testing.T) {
	h, server, _ := consoleSetup(t)
	op := consoleAccount(t, h, "Operator", "operator")
	member := consoleAccount(t, h, "Member", "member")
	browser := consoleSignIn(t, h, server.URL, op.Username)
	repository := register(t, server, 1)["repository_id"].(string)
	path := "/v1/ui/accounts/" + member.ID + "/access"
	body := func(role string, grants any) map[string]any {
		return map[string]any{"edit_revision": 2, "role": role, "disabled": false, "grants": grants}
	}
	first := body("member", []map[string]string{{"repository_id": repository, "mode": "rw"}})
	first["edit_revision"] = member.EditRevision
	consoleDo(t, h, server.URL, "PUT", path, first, browser, 200)

	listed := func() map[string]any {
		t.Helper()
		accounts := consoleJSON(t, consoleDo(t, h, server.URL, "GET", "/v1/ui/accounts", nil, browser, 200))["accounts"].([]any)
		for _, entry := range accounts {
			if account := entry.(map[string]any); account["id"] == member.ID {
				return account
			}
		}
		t.Fatal("the directory omitted the member")
		return nil
	}
	for _, role := range []string{"member", "operator", "none"} {
		missing := body(role, nil)
		delete(missing, "grants")
		consoleDo(t, h, server.URL, "PUT", path, missing, browser, 400)
		consoleDo(t, h, server.URL, "PUT", path, body(role, nil), browser, 400)
	}
	account := listed()
	if account["edit_revision"] != float64(2) || len(account["grants"].([]any)) != 1 {
		t.Fatal("a request without a grant list changed the account")
	}

	consoleDo(t, h, server.URL, "PUT", path, body("member", []map[string]string{}), browser, 200)
	if grants := listed()["grants"].([]any); len(grants) != 0 {
		t.Fatal("an explicit empty list did not remove the grant")
	}
}

// A repository grant reads and sends mail. It must never read as console administration.
func TestAGrantIsNeverConsoleAdministration(t *testing.T) {
	h, server, _ := consoleSetup(t)
	op := consoleAccount(t, h, "Operator", "operator")
	member := consoleAccount(t, h, "Member", "member")
	operator := consoleSignIn(t, h, server.URL, op.Username)
	repository := register(t, server, 1)["repository_id"].(string)
	path := "/v1/ui/accounts/" + member.ID + "/access"
	edit := map[string]any{"edit_revision": member.EditRevision, "role": "member", "disabled": false, "grants": []map[string]string{{"repository_id": repository, "mode": "rw"}}}

	refusals := func() (string, string) {
		t.Helper()
		browser := consoleSignIn(t, h, server.URL, member.Username)
		list := consoleDo(t, h, server.URL, "GET", "/v1/ui/accounts", nil, browser, 403)
		write := consoleDo(t, h, server.URL, "PUT", path, edit, browser, 403)
		return list.Body.String(), write.Body.String()
	}
	listWithout, writeWithout := refusals()
	consoleDo(t, h, server.URL, "PUT", path, edit, operator, 200)
	listWith, writeWith := refusals()
	if listWith != listWithout || writeWith != writeWithout {
		t.Fatalf("an rw grant changed the refusal: %q %q, without it %q %q", listWith, writeWith, listWithout, writeWithout)
	}
}

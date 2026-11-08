package hub

import (
	"net/url"
	"testing"
)

func TestAgentHistoryHTTPRoutes(t *testing.T) {
	_, s := setup(t)
	a, b, c := register(t, s, 1), register(t, s, 2), register(t, s, 3)
	first := send(t, s, a, b, "first")
	second := send(t, s, b, a, "second")
	id, thread := first["message_id"].(string), first["thread_id"].(string)
	query := "?execution_id=" + a["execution_id"].(string)
	page := request(t, s, "GET", "/v1/messages/history"+query+"&q=PRIVATE&limit=1", nil, 200)
	if len(page["messages"].([]any)) != 1 || page["next"] == "" {
		t.Fatal(page)
	}
	page = request(t, s, "GET", "/v1/messages/history"+query+"&q=PRIVATE&limit=1&before="+url.QueryEscape(page["next"].(string)), nil, 200)
	if len(page["messages"].([]any)) != 1 || page["next"] != "" {
		t.Fatal(page)
	}
	page = request(t, s, "GET", "/v1/messages/history"+query+"&q=absent", nil, 200)
	if len(page["messages"].([]any)) != 0 {
		t.Fatal(page)
	}
	page = request(t, s, "GET", "/v1/threads/"+thread+query, nil, 200)
	if page["messages"].([]any)[0].(map[string]any)["message_id"] != id {
		t.Fatal(page)
	}
	request(t, s, "GET", "/v1/threads/"+thread+"?execution_id="+c["execution_id"].(string), nil, 404)
	request(t, s, "GET", "/v1/threads/missing"+query, nil, 404)
	request(t, s, "GET", "/v1/messages/history"+query+"&before=invalid", nil, 400)
	request(t, s, "GET", "/v1/messages/history"+query+"&q=%22", nil, 400)
	request(t, s, "GET", "/v1/messages/history?execution_id=missing", nil, 409)
	for _, message := range []string{id, second["message_id"].(string)} {
		part := request(t, s, "GET", "/v1/messages/"+message+"/body"+query+"&after=8&limit=4", nil, 200)
		if part["message_id"] != message || part["body"] != "BODY" || part["offset"] != float64(8) {
			t.Fatal(part)
		}
	}
	request(t, s, "GET", "/v1/messages/"+id+"/body?execution_id="+c["execution_id"].(string), nil, 404)
	request(t, s, "GET", "/v1/messages/"+id+"/body"+query+"&after=-1", nil, 400)
	// History and read do not fetch mail or clear the recipient's pending receipt.
	status := request(t, s, "GET", "/v1/messages/"+id+"/status"+query, nil, 200)
	if status["recipients"].([]any)[0].(map[string]any)["state"] != "pending" {
		t.Fatal(status)
	}
}

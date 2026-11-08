package store

import (
	"slices"
	"testing"
)

func TestOperatorThreadFilterIncludesSentAndReceivedAcrossRepositories(t *testing.T) {
	s, irisAgent, irisOut, sampleAgent, sampleOut := crossRepo(t)
	irisRepo := irisOut["repository_id"].(string)
	sampleRepo := sampleOut["repository_id"].(string)
	inIris := call(t, s, "messages", sendTo(irisAgent, "in-iris", "review pending", "human"))["thread_id"].(string)
	inSample := call(t, s, "messages", sendTo(sampleAgent, "in-sample", "review pending", "human"))["thread_id"].(string)
	outIris := call(t, s, "ui_messages", Request{RepositoryID: irisRepo, Key: "out-iris", To: []string{agentName(sampleOut)}, Body: "decision approved"})["thread_id"].(string)
	outSample := call(t, s, "ui_messages", Request{RepositoryID: sampleRepo, Key: "out-sample", To: []string{agentName(irisOut)}, Body: "decision approved"})["thread_id"].(string)
	betweenAgents := call(t, s, "messages", sendTo(irisAgent, "agents-only", "human review pending", agentName(sampleOut)))["thread_id"].(string)

	for _, tc := range []struct {
		name string
		req  Request
		want []string
	}{
		{"all operator mail", Request{Agent: "human"}, []string{inIris, inSample, outIris, outSample}},
		{"iris operator mail", Request{Agent: "human", RepositoryID: irisRepo}, []string{inIris, outIris, outSample}},
		{"sample operator mail", Request{Agent: "human", RepositoryID: sampleRepo}, []string{inSample, outIris, outSample}},
		{"search operator mail", Request{Agent: "human", Q: "review"}, []string{inIris, inSample}},
		{"repository and search", Request{Agent: "human", RepositoryID: irisRepo, Q: "review"}, []string{inIris}},
		{"no search matches", Request{Agent: "human", Q: "absent"}, []string{}},
		{"unknown repository", Request{Agent: "human", RepositoryID: "missing"}, []string{}},
		{"agent name still exact", Request{Agent: agentName(sampleOut)}, []string{inSample, outIris, betweenAgents}},
		{"agent id still exact", Request{Agent: sampleOut["agent"].(map[string]any)["id"].(string)}, []string{inSample, outIris, betweenAgents}},
		{"one operator mailbox by id", Request{Agent: "human_" + irisRepo}, []string{inIris, outIris}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			threads := call(t, s, "ui_threads", tc.req)["threads"].([]map[string]any)
			got := []string{}
			for _, thread := range threads {
				got = append(got, thread["id"].(string))
			}
			slices.Sort(got)
			slices.Sort(tc.want)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("threads %v, want %v", got, tc.want)
			}
		})
	}

	// Every timestamp is identical. A one-thread page must still visit each matching
	// conversation exactly once, preserving its unread count and leaving mail unread.
	for _, repo := range []string{"", irisRepo} {
		all := call(t, s, "ui_threads", Request{Agent: "human", RepositoryID: repo})["threads"].([]map[string]any)
		want := []string{}
		for _, thread := range all {
			want = append(want, thread["id"].(string))
		}
		got := []string{}
		before := ""
		for range len(want) + 1 {
			page := call(t, s, "ui_threads", Request{Agent: "human", RepositoryID: repo, Before: before, Limit: 1})
			threads := page["threads"].([]map[string]any)
			if len(threads) != 1 {
				t.Fatalf("expected one matching thread: %v", page)
			}
			thread := threads[0]
			id := thread["id"].(string)
			got = append(got, id)
			unread := int64(0)
			if id == inIris || id == inSample {
				unread = 1
			}
			if thread["unread"] != unread || thread["count"] != int64(1) {
				t.Fatalf("unexpected thread counts: %v", thread)
			}
			next, more := page["next"].(string)
			if !more {
				break
			}
			before = next
		}
		if !slices.Equal(got, want) {
			t.Fatalf("repository %q: paged %v, want %v", repo, got, want)
		}
	}
	if unread := call(t, s, "ui_overview", Request{})["human_unread"]; unread != 2 {
		t.Fatalf("filtering operator mail changed unread count: %v", unread)
	}
}

func TestOperatorThreadFilterKeepsEveryHumanMailboxAfterRepair(t *testing.T) {
	s, _, _ := testStore(t)
	a, ar, b, br := mergeSplit(t, s)
	from, into := br["repository_id"].(string), ar["repository_id"].(string)
	want := []string{
		call(t, s, "messages", sendTo(a, "in-a", "pending a", "human"))["thread_id"].(string),
		call(t, s, "messages", sendTo(b, "in-b", "pending b", "human"))["thread_id"].(string),
		call(t, s, "ui_messages", Request{RepositoryID: from, Key: "out-b", To: []string{agentName(ar)}, Body: "decision b"})["thread_id"].(string),
		call(t, s, "ui_messages", Request{RepositoryID: into, Key: "out-a", To: []string{agentName(br)}, Body: "decision a"})["thread_id"].(string),
	}
	call(t, s, "messages", sendTo(a, "agents-only", "pending agent review", agentName(br)))
	call(t, s, "merge", mergeRequest(from, into, false))
	if got := count(t, s, "SELECT COUNT(*) FROM Agent WHERE client='human' AND repository_id=?", into); got != 2 {
		t.Fatalf("repair did not retain both operator mailboxes: %d", got)
	}
	slices.Sort(want)
	for _, repo := range []string{"", into} {
		threads := call(t, s, "ui_threads", Request{Agent: "human", RepositoryID: repo})["threads"].([]map[string]any)
		got := []string{}
		for _, thread := range threads {
			got = append(got, thread["id"].(string))
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Fatalf("operator threads after repair: %v, want %v", got, want)
		}
	}
	if unread := call(t, s, "ui_overview", Request{})["human_unread"]; unread != 2 {
		t.Fatalf("listing repaired operator mailboxes changed unread count: %v", unread)
	}
}

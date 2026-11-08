package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// Sends to other repositories are limited per credential and minute, as a whole send:
// a refused send delivers to nobody. Sends within a repository are not counted.
func TestCrossRepositorySendsHaveTheirOwnQuota(t *testing.T) {
	s, irisAgent, irisOut, sampleAgent, sampleOut := crossRepo(t)
	_ = irisAgent
	ctx := WithPrincipal(context.Background(), "sample-token")
	send := func(key string, to ...string) error {
		_, err := s.Call(ctx, "messages", sendTo(sampleAgent, key, "x", to...))
		return err
	}
	for i := 0; i < crossLimit; i++ {
		if err := send(fmt.Sprint("ok", i), agentName(irisOut)); err != nil {
			t.Fatal(i, err)
		}
	}
	var e *Error
	if err := send("over", agentName(irisOut)); !errors.As(err, &e) || e.Code != "rate_limited" {
		t.Fatal(err)
	}
	// Mail inside the repository is not cross-repository mail.
	if err := send("local", agentName(sampleOut)); err != nil {
		t.Fatal(err)
	}
	// Another credential has its own allowance.
	if _, err := s.Call(WithPrincipal(context.Background(), "other"), "messages", sendTo(sampleAgent, "other", "x", agentName(irisOut))); err != nil {
		t.Fatal(err)
	}
}

func TestTheQuotaWindowStartsAgain(t *testing.T) {
	s := &Store{}
	start := time.Unix(1800000000, 0)
	if !s.allowCross("p", crossLimit, start) || s.allowCross("p", 1, start.Add(time.Second)) {
		t.Fatal("the limit was not applied")
	}
	if !s.allowCross("p", 1, start.Add(time.Minute)) {
		t.Fatal("the window did not start again")
	}
}

func TestEveryAgentListsAllRepositories(t *testing.T) {
	s, _, irisOut, _, sampleOut := crossRepo(t)
	all := call(t, s, "agents", Request{All: true})["agents"].([]map[string]any)
	repos := map[any]bool{}
	for _, a := range all {
		repos[a["repository_id"]] = true
	}
	if !repos[irisOut["repository_id"]] || !repos[sampleOut["repository_id"]] {
		t.Fatal(all)
	}
}

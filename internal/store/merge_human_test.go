package store

import "testing"

func TestMergePreservesHumanMailboxesAndOperatorReplies(t *testing.T) {
	for _, destinationHuman := range []bool{false, true} {
		label := "source-only"
		if destinationHuman {
			label = "both-repositories"
		}
		t.Run(label, func(t *testing.T) {
			s, _, _ := testStore(t)
			a, ar, b, br := mergeSplit(t, s)
			from, into := br["repository_id"].(string), ar["repository_id"].(string)
			// A session can name its client human, but that does not make its
			// mailbox the repository's operator inbox.
			if _, err := s.db.Exec("INSERT INTO Agent VALUES('ag_named_human','named-human',?,'agent-host','human','/agent',1)", into); err != nil {
				t.Fatal(err)
			}
			sourceHuman := "human_" + from
			sourceMail := historySend(t, s, b, "human", "source-human", "", "source question", "preserve human mail")
			name := br["agent"].(map[string]any)["name"].(string)
			call(t, s, "ui_messages", Request{RepositoryID: from, Key: "source-operator", ReplyTo: sourceMail["message_id"].(string), To: []string{name}, Body: "preserve operator reply"})
			wantHuman := sourceHuman
			if destinationHuman {
				historySend(t, s, a, "human", "destination-human", "", "destination question", "preserve destination mail")
				wantHuman = "human_" + into
			}
			call(t, s, "merge", mergeRequest(from, into, false))
			if count(t, s, "SELECT COUNT(*) FROM Agent WHERE id=? AND name=? AND repository_id=?", sourceHuman, sourceHuman, into) != 1 ||
				count(t, s, "SELECT COUNT(*) FROM Execution WHERE id=? AND repository_id=? AND agent_id=?", "operator_"+from, into, sourceHuman) != 1 {
				t.Fatal("merge changed the source human mailbox or operator Execution")
			}
			newMail := historySend(t, s, a, "human", "after-merge-human", "", "new question", "new human mail")
			if count(t, s, "SELECT COUNT(*) FROM Delivery WHERE message_id=? AND recipient_agent_id=?", newMail["message_id"], wantHuman) != 1 {
				t.Fatal("new human mail did not use the destination's available human mailbox")
			}
			reply := call(t, s, "ui_messages", Request{RepositoryID: into, Key: "after-merge-reply", ReplyTo: sourceMail["message_id"].(string), To: []string{name}, Body: "reply after repair"})
			if reply["thread_id"] != sourceMail["thread_id"] || count(t, s, "SELECT COUNT(*) FROM Message WHERE id=? AND sender_execution_id=?", reply["message_id"], "operator_"+into) != 1 {
				t.Fatal("operator could not continue the preserved source thread", reply)
			}
			read := call(t, s, "ui_read", Request{RepositoryID: into, Key: sourceMail["thread_id"].(string)})
			if read["read"] != 1 {
				t.Fatal("source human mail was not operator-readable after repair", read)
			}
			thread := call(t, s, "ui_thread", Request{RepositoryID: into, Key: sourceMail["thread_id"].(string)})["messages"].([]map[string]any)
			if len(thread) != 3 {
				t.Fatal("human thread history was not preserved", thread)
			}
		})
	}
}

package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mateusz-klatt/iris/internal/budget"
)

// ADR 008: a body too long for one tool result is read in parts. Each part ends on a character
// boundary, says where the next starts, and reading settles nothing.
func TestALongBodyIsReadInPartsByItsRecipientOnly(t *testing.T) {
	s, _, _ := testStore(t)
	sender, _ := session(t, s, 1)
	recipient, ro := session(t, s, 2)
	other, _ := session(t, s, 3)
	body := strings.Repeat("żółw ", 1800) // 14,400 bytes of 2-byte letters, so a part can end mid-character
	mail := sender
	mail.Key, mail.Subject, mail.Body = "long", "s", body
	mail.To = []string{ro["agent"].(map[string]any)["name"].(string)}
	call(t, s, "messages", mail)
	d := call(t, s, "inbox", recipient)["deliveries"].([]map[string]any)[0]
	id := d["id"].(string)

	var got strings.Builder
	read := recipient
	read.Key, read.Limit = id, 4001 // odd, to land inside a two-byte letter
	for {
		part := call(t, s, "read_body", read)
		chunk := part["body"].(string)
		if !utf8.ValidString(chunk) || budget.Encoded(chunk) > 4001 || part["body_bytes"] != len(body) {
			t.Fatalf("part at %d: %d bytes, valid %v", read.After, len(chunk), utf8.ValidString(chunk))
		}
		got.WriteString(chunk)
		next, more := part["next_offset"]
		if !more {
			break
		}
		read.After = int64(next.(int))
	}
	if got.String() != body {
		t.Fatal("the parts do not make the body")
	}
	// Reading changed nothing: the delivery is still the same attempt, and still unacked.
	again := call(t, s, "inbox", recipient)["deliveries"].([]map[string]any)[0]
	if again["id"] != id || number(again["attempt"]) != number(d["attempt"])+1 {
		t.Fatalf("reading changed the delivery: %v then %v", d, again)
	}
	// Another agent, a missing id and an offset outside the body are refused.
	stranger := other
	stranger.Key = id
	expectError(t, s, "read_body", stranger, "not_found")
	missing := recipient
	missing.Key = "dl_missing"
	expectError(t, s, "read_body", missing, "not_found")
	outside := recipient
	outside.Key, outside.After = id, int64(len(body)+1)
	expectError(t, s, "read_body", outside, "bad_request")
	outside.After = -1
	expectError(t, s, "read_body", outside, "bad_request")
}

// An offset inside a character starts at that character, so no part begins mid-letter.
func TestAnOffsetInsideACharacterStartsAtIt(t *testing.T) {
	s, _, _ := testStore(t)
	sender, _ := session(t, s, 1)
	recipient, ro := session(t, s, 2)
	mail := sender
	mail.Key, mail.Body, mail.To = "k", "aż", []string{ro["agent"].(map[string]any)["name"].(string)}
	call(t, s, "messages", mail)
	d := call(t, s, "inbox", recipient)["deliveries"].([]map[string]any)[0]
	read := recipient
	read.Key, read.After = d["id"].(string), 2 // inside the two bytes of ż
	if part := call(t, s, "read_body", read); part["body"] != "ż" || part["offset"] != 1 {
		t.Fatal(part)
	}
}

// A session that is no longer live reads nothing.
func TestAnEndedSessionCannotReadABody(t *testing.T) {
	s, _, _ := testStore(t)
	r, _ := session(t, s, 1)
	call(t, s, "end", r)
	r.Key = "dl_any"
	expectError(t, s, "read_body", r, "lost")
}

// A storage failure is reported as one, not hidden as an unknown delivery.
func TestAStorageFailureIsNotAnUnknownDelivery(t *testing.T) {
	s, _, _ := testStore(t)
	r, _ := session(t, s, 1)
	if _, err := s.db.Exec("ALTER TABLE Message RENAME COLUMN body TO gone"); err != nil {
		t.Fatal(err)
	}
	r.Key = "dl_any"
	_, err := s.Call(context.Background(), "read_body", r)
	var e *Error
	if err == nil || (errors.As(err, &e) && e.Code == "not_found") {
		t.Fatalf("a broken table read as %v", err)
	}
}

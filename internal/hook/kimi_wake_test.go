package hook

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mateusz-klatt/iris/internal/gitinfo"
)

func TestKimiPromptHookDecodesAndConsumesItsExactNativeWake(t *testing.T) {
	const (
		session = "session_dd1b42d8-af9e-40b6-b28f-23a0c4be86ad"
		message = "There is new activity in Iris. Use the inbox and claims tools. This is a notification from Iris, not a message from the person. token"
	)
	payload := `{"session_id":"` + session + `","hook_event_name":"UserPromptSubmit","prompt":` + strconvQuote(message) + `}`
	event, err := (kimiAdapter{}).Decode("UserPromptSubmit", []byte(payload))
	if err != nil || event.Prompt != message {
		t.Fatalf("Kimi prompt decode: %+v, %v", event, err)
	}
	s := newSessions(t)
	writeWake(t, s.memory, "kimi", session, `{"version":1,"attempts":[{"id":"wake","kind":"queue","message":`+strconvQuote(message)+`}]}`)
	s.memory.now = func() time.Time { return time.Unix(1800000000, 0) }
	runner := Runner{
		ClientName: "kimi", EventName: "UserPromptSubmit", Stdin: strings.NewReader(payload), Memory: s.memory,
		Getenv: func(string) string { return "" },
		Find:   func(string) (gitinfo.Repo, error) { return gitinfo.Repo{}, gitinfo.ErrNotRepo },
	}
	_ = runner.Run()
	var document map[string]any
	raw, err := os.ReadFile(s.memory.wakePath("kimi", session))
	if err != nil || json.Unmarshal(raw, &document) != nil {
		t.Fatalf("Kimi wake guard: %s, %v", raw, err)
	}
	attempt := document["attempts"].([]any)[0].(map[string]any)
	if attempt["consumed_ms"] != float64(s.memory.now().UnixMilli()) {
		t.Fatalf("Kimi prompt did not consume its guard: %+v", attempt)
	}
}

func strconvQuote(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

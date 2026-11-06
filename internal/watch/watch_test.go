package watch

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestCoalescing(t *testing.T) {
	var output bytes.Buffer
	c := Coalescer{Output: &output, Window: 30 * time.Second}
	start := time.Now()
	for _, kind := range []string{"inbox_changed", "claim_released", "reset", "other"} {
		if err := c.Add(start, kind, 1); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		_ = c.Add(start.Add(time.Second), "inbox_changed", 1)
	}
	_ = c.Flush(start.Add(29 * time.Second))
	if strings.Count(output.String(), "\n") != 2 {
		t.Fatal(output.String())
	}
	_ = c.Flush(start.Add(30 * time.Second))
	want := "Iris: 1 new messages. Use the inbox tool.\nIris: 1 claims released. Use the claims tool.\nIris: 4 new messages. Use the inbox tool.\n"
	if output.String() != want {
		t.Fatal(output.String())
	}
	_ = c.Flush(start.Add(time.Minute))
	if output.String() != want {
		t.Fatal("duplicate flush")
	}
}

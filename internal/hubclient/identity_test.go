package hubclient

import (
	"encoding/json"
	"testing"
)

// A hub that names attention always sends identity, null when nothing is pending; a hub from
// before sends none. The decoder tells the two apart; an identity that is neither null nor an
// object is an error.
func TestAnAttentionIdentityReadsAbsentNullAndNamed(t *testing.T) {
	for body, want := range map[string]Identity{
		`{"unread":1}`:                                    {},
		`{"identity":null,"high_water":9}`:                {Known: true},
		`{"identity":{"stream":"s","cursor":7}}`:          {Known: true, Pending: true, Stream: "s", Cursor: 7},
		`{"identity":  null  ,"monitored":true}`:          {Known: true},
		`{"identity":{"stream":"s","cursor":7,"x":true}}`: {Known: true, Pending: true, Stream: "s", Cursor: 7},
	} {
		var a Attention
		if err := json.Unmarshal([]byte(body), &a); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if a.Identity != want {
			t.Fatalf("%s read as %+v", body, a.Identity)
		}
	}
	var a Attention
	if err := json.Unmarshal([]byte(`{"identity":5}`), &a); err == nil {
		t.Fatal("a number read as an identity")
	}
	if err := json.Unmarshal([]byte(`{"high_water":12}`), &a); err != nil || a.HighWater != 12 {
		t.Fatalf("high_water %d: %v", a.HighWater, err)
	}
}

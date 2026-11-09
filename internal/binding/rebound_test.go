package binding

import "testing"

func TestReboundRequiresAPositiveIdentityChange(t *testing.T) {
	stream := Bound{ExecutionID: "ex_running", Generation: 2, RepositoryID: "rp_before"}
	for _, test := range []struct {
		name    string
		current Bound
		want    bool
	}{
		{"unbound", Bound{}, false},
		{"same identity", stream, false},
		{"merged repository", Bound{ExecutionID: stream.ExecutionID, Generation: stream.Generation, RepositoryID: "rp_after"}, false},
		{"another Execution", Bound{ExecutionID: "ex_adopted", Generation: stream.Generation}, true},
		{"another generation", Bound{ExecutionID: stream.ExecutionID, Generation: stream.Generation + 1}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := &Resolver{bound: test.current}
			if got := r.Rebound(stream); got != test.want {
				t.Fatalf("stream %+v, current %+v: rebound %v, want %v", stream, test.current, got, test.want)
			}
		})
	}
}

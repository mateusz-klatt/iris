package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// Gap review B1: concurrency and multi-path atomicity were tested apart. Here twenty sessions
// race over overlapping path sets at once: case variants of the same paths, a rename into the
// contested tree, and an explicit prefix claim over it. Whatever the interleaving, each session
// ends up holding all of its paths or none of them, and only one session holds the shared ones.
func TestRacingMultiPathClaimsAreAllOrNothing(t *testing.T) {
	for round := 0; round < 5; round++ {
		t.Run(fmt.Sprint(round), func(t *testing.T) {
			s, _, _ := testStore(t)
			type contender struct {
				op    string
				req   Request
				paths []string // what a grant must cover, as requested
			}
			var contenders []contender
			for i := 0; i < 20; i++ {
				r, _ := session(t, s, i)
				own := fmt.Sprintf("own/%d", i)
				switch i % 4 {
				case 0: // the shared paths, as spelled
					contenders = append(contenders, contender{"touch", touchRequest(r, "t", "shared/a", "shared/b", own), []string{"shared/a", "shared/b", own}})
				case 1: // the same paths in other cases, which fold to the same keys
					contenders = append(contenders, contender{"touch", touchRequest(r, "t", "SHARED/A", "Shared/B", own), []string{"SHARED/A", "Shared/B", own}})
				case 2: // a rename out of the contested tree, which needs both endpoints
					req := touchRequest(r, "t", own)
					req.RenameFrom, req.RenameTo = "shared/a", fmt.Sprintf("moved/%d", i)
					contenders = append(contenders, contender{"touch", req, []string{own, "shared/a", fmt.Sprintf("moved/%d", i)}})
				default: // an explicit claim over the whole tree
					contenders = append(contenders, contender{"claim", claimRequest(r, "c", "shared/", "prefix"), []string{"shared"}})
				}
			}
			var wg sync.WaitGroup
			start := make(chan struct{})
			errs := make(chan error, len(contenders))
			for _, c := range contenders {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, err := s.Call(context.Background(), c.op, c.req)
					var e *Error
					if err != nil && (!errors.As(err, &e) || e.Code != "conflict") {
						errs <- err
					}
				}()
			}
			close(start)
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatalf("unexpected error: %v", err)
			}

			held := map[string][]string{} // holder name to the paths it holds
			list := call(t, s, "claims", Request{RepositoryID: repoOf(t, s)})["claims"].([]map[string]any)
			for _, c := range list {
				held[c["holder"].(string)] = append(held[c["holder"].(string)], c["path"].(string))
			}
			sharedHolders := 0
			for i, c := range contenders {
				name := fmt.Sprintf("codex-host-w%d", i)
				got := held[name]
				if len(got) != 0 && !sameSet(got, c.paths) {
					t.Fatalf("%s holds %v of %v: a partial grant", name, got, c.paths)
				}
				for _, p := range got {
					if strings.HasPrefix(strings.ToLower(p), "shared") {
						sharedHolders++
						break
					}
				}
			}
			if sharedHolders != 1 {
				t.Fatalf("%d sessions hold the shared tree: %v", sharedHolders, held)
			}
		})
	}
}

func repoOf(t *testing.T, s *Store) string {
	t.Helper()
	_, out := session(t, s, 0)
	return out["repository_id"].(string)
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, x := range a {
		seen[x]++
	}
	for _, x := range b {
		if seen[x] == 0 {
			return false
		}
		seen[x]--
	}
	return true
}

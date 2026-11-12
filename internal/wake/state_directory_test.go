package wake

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStateDirectoryRefusalPrecedesAllDirectoryAndLockCreation(t *testing.T) {
	for _, operation := range []string{"snapshot", "reservation"} {
		t.Run(operation, func(t *testing.T) {
			s := testStore(t)
			var validated []string
			s.io.protectDir = func(path string) error {
				validated = append(validated, path)
				return errStateTest
			}
			var err error
			if operation == "snapshot" {
				_, err = s.snapshot()
			} else {
				var result publication
				result, err = s.publish(stateSnapshot{}, testDocument(testAttempt(1, "queue", "")))
				if result.Published {
					t.Fatal("refused directory published a reservation")
				}
			}
			if !errors.Is(err, errStateTest) || !reflect.DeepEqual(validated, []string{s.sessions.Dir()}) {
				t.Fatalf("directory refusal was bypassed: %v, validated %v", err, validated)
			}
			entries, err := os.ReadDir(filepath.Dir(s.sessions.Dir()))
			if err != nil || len(entries) != 0 {
				t.Fatalf("an unconditional protector created files before validation: %v %v", entries, err)
			}
		})
	}
}

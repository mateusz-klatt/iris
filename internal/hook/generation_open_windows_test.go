package hook

import "testing"

func TestOpenGenerationFileRejectsInvalidWindowsPath(t *testing.T) {
	if _, err := OpenGenerationFile("bad\x00path.ended"); err == nil {
		t.Fatal("embedded-NUL generation path was accepted")
	}
}

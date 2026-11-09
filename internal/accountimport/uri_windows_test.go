package accountimport

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestSourceURIWindowsNativeAndExtendedDriveOpenReadOnly(t *testing.T) {
	file := filepath.Join(t.TempDir(), "source % #+.sqlite")
	writer, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal("could not create synthetic Windows URI source")
	}
	if _, err = writer.Exec("CREATE TABLE marker(value INTEGER); INSERT INTO marker VALUES(7)"); err != nil {
		_ = writer.Close()
		t.Fatal("could not populate synthetic Windows URI source")
	}
	if err = writer.Close(); err != nil {
		t.Fatal("could not close synthetic Windows URI source")
	}
	for _, path := range []string{file, `\\?\` + file} {
		reader, err := sql.Open("sqlite", sourceURI(path))
		if err != nil {
			t.Fatal("could not open synthetic Windows source URI")
		}
		var count int
		if err = reader.QueryRow("SELECT COUNT(*) FROM marker WHERE value=7").Scan(&count); err != nil || count != 1 {
			_ = reader.Close()
			t.Fatal("Windows native or extended URI did not name the original source")
		}
		if _, err = reader.Exec("UPDATE marker SET value=8"); err == nil {
			_ = reader.Close()
			t.Fatal("Windows source URI permitted a write")
		}
		if err = reader.Close(); err != nil {
			t.Fatal("could not close synthetic Windows source URI")
		}
	}
}

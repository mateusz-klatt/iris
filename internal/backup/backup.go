// Package backup takes a consistent copy of a hub database while the hub runs, and checks the
// copy before it is put in place. It is the capture half of ADR 006's restore procedure; the
// other half is restorecheck, which tests a copy in a throwaway hub.
package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Run copies the database at source to dest. It reads the source in one read transaction, as
// SQLite's VACUUM INTO does, so the copy is a single moment of the live database with its WAL
// applied, and it never writes to the source. It refuses a dest that exists, the live database
// among them. The copy is written beside dest, checked, and then linked into place, which
// fails rather than replace a file that appeared meanwhile, so a dest that exists is always a
// checked copy. It writes what it copied and how to test a restore to out.
func Run(ctx context.Context, source, dest string, out io.Writer) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("the database is not a regular file")
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("%s exists; a backup never writes over a file", dest)
	}
	// The partial copy is created empty and owner-only before SQLite fills it, as the database
	// holds mail and token hashes; VACUUM INTO writes into an empty file.
	partial := dest + ".partial"
	f, err := os.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s exists, perhaps from a backup that was stopped; remove it first", partial)
	}
	if err != nil {
		return err
	}
	_ = f.Close()
	db, _ := sql.Open("sqlite", readOnly(source)) // the driver only records the name; Exec reports errors
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", partial); err != nil {
		_ = os.Remove(partial)
		return fmt.Errorf("copy: %w", err)
	}
	s, err := place(partial, dest)
	_ = os.Remove(partial) // on success dest is a second link to the same file
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "backup %s: integrity ok, schema %d, %d messages, %d events, %d bytes\n", dest, s.version, s.messages, s.events, s.bytes)
	_, _ = fmt.Fprintf(out, "test a restore on a copy, never the live volume:\n  iris admin restore-check %s <empty scratch directory>\n", dest)
	return nil
}

// place checks the copy and links it in as dest. verify is a variable so a test can make a
// file appear at dest while the copy is checked.
func place(partial, dest string) (summary, error) {
	s, err := verify(partial)
	if err != nil {
		return s, err
	}
	if err := os.Link(partial, dest); err != nil {
		return s, fmt.Errorf("put the backup in place: %w", err)
	}
	return s, nil
}

var verify = summarise

type summary struct {
	version, messages, events int
	bytes                     int64
}

// summarise runs SQLite's integrity check on a copy and reads what it holds. A copy that is not
// a hub database fails here too: it has no Message table.
func summarise(path string) (summary, error) {
	var s summary
	db, _ := sql.Open("sqlite", readOnly(path)) // the driver only records the name; queries report errors
	defer func() { _ = db.Close() }()
	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		return s, fmt.Errorf("check the copy: %w", err)
	}
	if result != "ok" {
		return s, fmt.Errorf("the copy failed SQLite's integrity check: %s", result)
	}
	err := db.QueryRow(`SELECT (SELECT user_version FROM pragma_user_version),(SELECT COUNT(*) FROM Message),
(SELECT COUNT(*) FROM Event),(SELECT page_count*page_size FROM pragma_page_count,pragma_page_size)`).Scan(&s.version, &s.messages, &s.events, &s.bytes)
	if err != nil {
		return s, fmt.Errorf("read the copy: %w", err)
	}
	return s, nil
}

// readOnly names a database for the driver as an SQLite URI, so that it is opened read-only,
// waits up to five seconds while the hub holds a lock, and keeps temporary storage in memory, as
// the hub does since the image has no temporary directory (#171). The path is escaped, so a ? or # in it
// stays part of the name: file:/home/a/hub.db, or file:C:/a/hub.db on Windows.
func readOnly(path string) string {
	abs, _ := filepath.Abs(path) // Abs fails only when the working directory is gone
	return "file:" + (&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath() + "?mode=ro&_pragma=busy_timeout(5000)&_pragma=temp_store(2)"
}

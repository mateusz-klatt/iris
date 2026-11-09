package accountimport

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"modernc.org/sqlite"
)

const sourceSchema = `CREATE TABLE accounts(id,username,password_hash,role,disabled,display_name,preferred_ui_locale,preferred_correspondence_locale,created_ts,last_login_ts)`

func sourceFile(t *testing.T, schema string, rows int) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "synthetic.sqlite")
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal("could not create synthetic SQLite source")
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err = db.Exec(schema); err != nil {
		t.Fatal("could not create synthetic schema")
	}
	r := testRow()
	for i := 0; i < rows; i++ {
		_, err = db.Exec(`INSERT INTO accounts VALUES(?,?,?,?,?,?,?,?,?,?)`, i+1, fmt.Sprintf("Person%d", i+1), r.PasswordHash, r.Role, 0, r.DisplayName, r.UILocale, r.CorrespondenceLocale, 1, nil)
		if err != nil {
			t.Fatal("could not create synthetic source row")
		}
	}
	return file
}

func modifySource(t *testing.T, file, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal("could not open synthetic source for mutation")
	}
	defer func() { _ = db.Close() }()
	if _, err = db.Exec(query, args...); err != nil {
		t.Fatal("could not mutate synthetic source")
	}
}

func TestReadOnlySourcePreservesFileAndNullableHistory(t *testing.T) {
	file := sourceFile(t, sourceSchema, 1)
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal("could not read synthetic source bytes")
	}
	b, err := Read(context.Background(), file, testManifest())
	if err != nil || len(b.Rows) != 1 || b.Rows[0].ID != "1" || b.Rows[0].CreatedMS != 1000 || b.Rows[0].LastLoginMS != nil || b.Rows[0].PasswordHash != testRow().PasswordHash {
		t.Fatal("synthetic source did not preserve account history and credentials")
	}
	after, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("read-only adapter changed source bytes")
	}
	modifySource(t, file, "UPDATE accounts SET disabled=1,last_login_ts=2")
	b, err = Read(context.Background(), file, testManifest())
	if err != nil || !b.Rows[0].Disabled || b.Rows[0].LastLoginMS == nil || *b.Rows[0].LastLoginMS != 2000 {
		t.Fatal("nullable timestamp or disabled integer decoding failed")
	}
	m := testManifest()
	m.TimestampEncoding = "unix_milliseconds"
	b, err = Read(context.Background(), file, m)
	if err != nil || b.Rows[0].CreatedMS != 1 || *b.Rows[0].LastLoginMS != 2 {
		t.Fatal("explicit milliseconds were guessed as seconds")
	}
	missing := filepath.Join(t.TempDir(), "does-not-exist.sqlite")
	if _, err = Read(context.Background(), missing, testManifest()); err == nil {
		t.Fatal("missing read-only source was created")
	}
	if _, err = os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("read-only adapter created source file")
	}
	entries, err := os.ReadDir(filepath.Dir(file))
	if err != nil || len(entries) != 1 {
		t.Fatal("read-only adapter created auxiliary files")
	}
}

func TestSQLiteInputsRefuseUnsupportedSchemaRowsAndTimes(t *testing.T) {
	for name, setup := range map[string]struct {
		schema string
		rows   int
		query  string
		args   []any
	}{
		"empty": {sourceSchema, 0, "", nil}, "too-many": {sourceSchema, MaxRows + 1, "", nil},
		"duplicate-ids":  {sourceSchema, 2, "UPDATE accounts SET id=1", nil},
		"unknown-role":   {sourceSchema, 1, "UPDATE accounts SET role='unsupported'", nil},
		"null-text":      {sourceSchema, 1, "UPDATE accounts SET display_name=NULL", nil},
		"oversized-text": {sourceSchema, 1, "UPDATE accounts SET username=?", []any{strings.Repeat("x", 257)}},
		"blob-text":      {sourceSchema, 1, "UPDATE accounts SET role=zeroblob(1000000)", nil},
		"blob-id":        {sourceSchema, 1, "UPDATE accounts SET id=zeroblob(1000000)", nil},
		"blob-disabled":  {sourceSchema, 1, "UPDATE accounts SET disabled=zeroblob(1000000)", nil},
		"blob-created":   {sourceSchema, 1, "UPDATE accounts SET created_ts=zeroblob(1000000)", nil},
		"blob-last":      {sourceSchema, 1, "UPDATE accounts SET last_login_ts=zeroblob(1000000)", nil},
		"real-id":        {sourceSchema, 1, "UPDATE accounts SET id=1.5", nil}, "negative-id": {sourceSchema, 1, "UPDATE accounts SET id=-1", nil},
		"boolean-text": {sourceSchema, 1, "UPDATE accounts SET disabled='true'", nil}, "boolean-two": {sourceSchema, 1, "UPDATE accounts SET disabled=2", nil},
		"created-text":       {sourceSchema, 1, "UPDATE accounts SET created_ts='2026-01-01'", nil},
		"created-negative":   {sourceSchema, 1, "UPDATE accounts SET created_ts=-1", nil},
		"created-overflow":   {sourceSchema, 1, "UPDATE accounts SET created_ts=9223372036854775807", nil},
		"last-real":          {sourceSchema, 1, "UPDATE accounts SET last_login_ts=1.5", nil},
		"last-before-create": {sourceSchema, 1, "UPDATE accounts SET last_login_ts=0", nil},
		"malformed-hash":     {sourceSchema, 1, "UPDATE accounts SET password_hash='private-sentinel'", nil},
		"unsupported-locale": {sourceSchema, 1, "UPDATE accounts SET preferred_ui_locale='unsupported'", nil},
		"missing-column":     {`CREATE TABLE accounts(id)`, 0, "", nil},
		"wrong-column":       {strings.Replace(sourceSchema, "last_login_ts", "unknown_column", 1), 0, "", nil},
		"extra-column":       {strings.Replace(sourceSchema, "last_login_ts)", "last_login_ts,extra)", 1), 0, "", nil},
		"generated-column":   {strings.Replace(sourceSchema, "last_login_ts)", "last_login_ts,extra AS (id+1))", 1), 0, "", nil},
		"view":               {`CREATE VIEW accounts AS SELECT 1 AS id`, 0, "", nil},
		"virtual":            {`CREATE VIRTUAL TABLE accounts USING fts5(content)`, 0, "", nil},
	} {
		t.Run(name, func(t *testing.T) {
			file := sourceFile(t, setup.schema, setup.rows)
			if setup.query != "" {
				modifySource(t, file, setup.query, setup.args...)
			}
			b, err := Read(context.Background(), file, testManifest())
			if name == "unknown-role" {
				if err != nil {
					t.Fatal("unknown source role must be quarantined rather than refused")
				}
				p, err := Validate(b)
				if err != nil || p[0].Account.Role != "none" || !p[0].Account.Disabled {
					t.Fatal("unknown source role granted access")
				}
			} else if err == nil || len(b.Rows) != 0 {
				t.Fatal("unsupported SQLite source was accepted")
			}
			if err != nil && (strings.Contains(err.Error(), "private-sentinel") || strings.Contains(err.Error(), testRow().PasswordHash)) {
				t.Fatal("source error disclosed credentials")
			}
		})
	}
}

type faultDriver struct{ state *sourceFault }
type sourceFault struct {
	count, fail int
	wrong       string
}

func (f *sourceFault) next() error {
	f.count++
	if f.count == f.fail {
		return errors.New("private-sentinel")
	}
	return nil
}
func (d faultDriver) Open(name string) (driver.Conn, error) {
	if err := d.state.next(); err != nil {
		return nil, err
	}
	c, err := (&sqlite.Driver{}).Open(name)
	if err != nil {
		return nil, err
	}
	return &faultConn{Conn: c, state: d.state}, nil
}

type faultConn struct {
	driver.Conn
	state *sourceFault
}

func (c *faultConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	if err := c.state.next(); err != nil {
		return nil, err
	}
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &faultTx{Tx: tx, state: c.state}, nil
}
func (c *faultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.state.next(); err != nil {
		return nil, err
	}
	r, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	return &faultRows{Rows: r, state: c.state, wrong: c.state.wrong != "" && strings.Contains(query, c.state.wrong)}, nil
}

type faultTx struct {
	driver.Tx
	state *sourceFault
}

func (tx *faultTx) Commit() error {
	if err := tx.state.next(); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Tx.Commit()
}

type faultRows struct {
	driver.Rows
	state *sourceFault
	wrong bool
}

func (r *faultRows) Columns() []string {
	if r.wrong {
		return append(r.Rows.Columns(), "wrong")
	}
	return r.Rows.Columns()
}
func (r *faultRows) Next(values []driver.Value) error {
	if err := r.state.next(); err != nil {
		return err
	}
	if r.wrong {
		values = values[:len(values)-1]
	}
	return r.Rows.Next(values)
}

var driverID atomic.Int64

func faultOpen(t *testing.T, state *sourceFault) func(string, string) (*sql.DB, error) {
	t.Helper()
	name := fmt.Sprintf("source-fault-%d", driverID.Add(1))
	sql.Register(name, faultDriver{state})
	return func(_ string, dsn string) (*sql.DB, error) { return sql.Open(name, dsn) }
}

func TestSourceSnapshotStorageFailuresRemainReadOnlyAndSanitized(t *testing.T) {
	file := sourceFile(t, sourceSchema, 1)
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal("could not capture synthetic source bytes")
	}
	state := &sourceFault{}
	open := faultOpen(t, state)
	b, err := read(context.Background(), file, testManifest(), open)
	if err != nil || len(b.Rows) != 1 {
		t.Fatal("fault wrapper changed valid source behavior")
	}
	boundaries := state.count
	for i := 1; i <= boundaries; i++ {
		state.count, state.fail = 0, i
		b, err = read(context.Background(), file, testManifest(), open)
		if err == nil || len(b.Rows) != 0 || strings.Contains(err.Error(), "private-sentinel") {
			t.Fatalf("source boundary %d exposed a partial snapshot or raw error", i)
		}
		after, readErr := os.ReadFile(file)
		if readErr != nil || !bytes.Equal(before, after) {
			t.Fatalf("source boundary %d changed the source", i)
		}
	}
	state.count, state.fail = 0, 0
	if _, err = read(context.Background(), file, testManifest(), open); err != nil {
		t.Fatal("snapshot could not retry after failures")
	}
	for _, wrong := range []string{"pragma_table_xinfo", "SELECT CASE"} {
		state.wrong = wrong
		if _, err = read(context.Background(), file, testManifest(), open); err == nil {
			t.Fatal("malformed driver column count was accepted")
		}
	}
}

func TestSourceEntryFailuresDoNotOpenOrCreateData(t *testing.T) {
	file := sourceFile(t, sourceSchema, 1)
	called := false
	open := func(string, string) (*sql.DB, error) { called = true; return nil, errors.New("private-sentinel") }
	m := testManifest()
	m.Version = 2
	if _, err := read(context.Background(), "unused", m, open); err == nil || called {
		t.Fatal("invalid manifest opened source")
	}
	if _, err := read(context.Background(), file, testManifest(), open); err == nil || strings.Contains(err.Error(), "private-sentinel") || !called {
		t.Fatal("source open error was not sanitized")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Read(ctx, "unused", testManifest()); err == nil {
		t.Fatal("canceled snapshot succeeded")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Remove(dir); err != nil {
		t.Fatal("could not remove synthetic working directory")
	}
	if _, err := Read(context.Background(), "relative", testManifest()); err == nil {
		t.Fatal("missing working directory resolved a relative source")
	}
}

func TestSourceValueTypesAndTimestampBounds(t *testing.T) {
	values := [10]any{int64(1), "Person", testRow().PasswordHash, "member", int64(0), "", "en", "pl", int64(0), nil}
	r, err := sourceRow(values, "unix_milliseconds")
	if err != nil || r.LastLoginMS != nil || r.CreatedMS != 0 {
		t.Fatal("valid zero timestamp or null history refused")
	}
	for _, value := range []any{"1", []byte("1"), float64(1), int64(-1), MaxTimestampMS + 1} {
		if _, ok := sourceTime(value, "unix_milliseconds"); ok {
			t.Fatal("unsupported timestamp type or bound accepted")
		}
	}
	if n, ok := sourceTime(MaxTimestampMS, "unix_milliseconds"); !ok || n != MaxTimestampMS {
		t.Fatal("last supported millisecond refused")
	}
	for _, index := range []int{0, 1, 4, 8, 9} {
		copy := values
		copy[index] = []byte("not-a-sql-integer-or-text")
		if _, err = sourceRow(copy, "unix_seconds"); err == nil {
			t.Fatal("source type confusion was accepted")
		}
	}
	var generic struct {
		Rows []string `json:"rows"`
	}
	if DecodeBatchJSON(strings.NewReader(`{"rows":["safe"]}`), &generic) != nil || !reflect.DeepEqual(generic.Rows, []string{"safe"}) {
		t.Fatal("batch decoder rejected strict JSON")
	}
	if DecodeBatchJSON(io.LimitReader(strings.NewReader(strings.Repeat(" ", (64<<10)+1)), (64<<10)+1), &generic) == nil {
		t.Fatal("oversized batch decoder input accepted")
	}
}

package accountimport

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

var columns = []string{"id", "username", "password_hash", "role", "disabled", "display_name", "preferred_ui_locale", "preferred_correspondence_locale", "created_ts", "last_login_ts"}

// Read opens only the named SQLite source, in mode=ro. One read transaction
// covers schema inspection and every selected row, giving a consistent snapshot.
// It does not invoke a source backend or open the hub's database.
func Read(ctx context.Context, path string, manifest Manifest) (Batch, error) {
	return read(ctx, path, manifest, sql.Open)
}

func read(ctx context.Context, path string, manifest Manifest, open func(string, string) (*sql.DB, error)) (Batch, error) {
	if err := ValidateManifest(manifest); err != nil {
		return Batch{}, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return Batch{}, invalid("", "could not locate source SQLite file")
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	q := u.Query()
	q.Set("mode", "ro")
	q.Add("_pragma", "query_only(1)")
	q.Add("_pragma", "trusted_schema(0)")
	q.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()
	db, err := open("sqlite", u.String())
	if err != nil {
		return Batch{}, invalid("", "could not open source SQLite file")
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Batch{}, invalid("", "could not start source snapshot")
	}
	defer func() { _ = tx.Rollback() }()
	batch, err := snapshot(ctx, tx, manifest)
	if err != nil {
		return Batch{}, err
	}
	if err = tx.Commit(); err != nil {
		return Batch{}, invalid("", "could not complete source snapshot")
	}
	return batch, nil
}

func snapshot(ctx context.Context, tx *sql.Tx, m Manifest) (Batch, error) {
	var declaration string
	err := tx.QueryRowContext(ctx, "SELECT sql FROM sqlite_schema WHERE type='table' AND name=?", m.Table).Scan(&declaration)
	if err != nil || strings.Contains(strings.ToUpper(declaration), "CREATE VIRTUAL TABLE") {
		return Batch{}, invalid("", "source table is unavailable or unsupported")
	}
	info, err := tx.QueryContext(ctx, "SELECT name,hidden FROM pragma_table_xinfo(?)", m.Table)
	if err != nil {
		return Batch{}, invalid("", "could not inspect source columns")
	}
	names := map[string]bool{}
	for info.Next() {
		var name string
		var hidden int
		if err = info.Scan(&name, &hidden); err != nil {
			_ = info.Close()
			return Batch{}, invalid("", "invalid source column declaration")
		}
		if hidden != 0 {
			_ = info.Close()
			return Batch{}, invalid("", "source generated or hidden columns are unsupported")
		}
		names[name] = true
	}
	err = info.Err()
	_ = info.Close()
	if err != nil {
		return Batch{}, invalid("", "could not inspect all source columns")
	}
	if len(names) != len(columns) {
		return Batch{}, invalid("", "source columns do not match the declared account contract")
	}
	for _, name := range columns {
		if !names[name] {
			return Batch{}, invalid("", "source columns do not match the declared account contract")
		}
	}
	// Bound text inside SQLite before allocating any field in Go. A rejected
	// field becomes NULL and is refused; it cannot be silently truncated.
	selected := []string{"CASE WHEN typeof(id)='integer' THEN id ELSE NULL END"}
	limits := map[string]int{"username": 256, "password_hash": 256, "role": 64, "display_name": 512, "preferred_ui_locale": 64, "preferred_correspondence_locale": 64}
	for _, name := range columns[1:] {
		if limit, ok := limits[name]; ok {
			selected = append(selected, "CASE WHEN typeof("+name+")='text' AND length(CAST("+name+" AS BLOB))<="+strconv.Itoa(limit)+" THEN "+name+" ELSE NULL END")
		} else if name == "last_login_ts" {
			selected = append(selected, "CASE WHEN typeof(last_login_ts) IN ('integer','null') THEN last_login_ts ELSE -1 END")
		} else {
			selected = append(selected, "CASE WHEN typeof("+name+")='integer' THEN "+name+" ELSE NULL END")
		}
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+strings.Join(selected, ",")+" FROM \""+m.Table+"\" LIMIT "+strconv.Itoa(MaxRows+1))
	if err != nil {
		return Batch{}, invalid("", "could not read source accounts")
	}
	defer func() { _ = rows.Close() }()
	batch := Batch{Manifest: m}
	for rows.Next() {
		var values [10]any
		args := make([]any, len(values))
		for i := range values {
			args[i] = &values[i]
		}
		if err = rows.Scan(args...); err != nil {
			return Batch{}, invalid("", "could not decode source account")
		}
		r, err := sourceRow(values, m.TimestampEncoding)
		if err != nil {
			return Batch{}, err
		}
		batch.Rows = append(batch.Rows, r)
		if len(batch.Rows) > MaxRows {
			return Batch{}, invalid("", "source account batch exceeds 32 rows")
		}
	}
	if err = rows.Err(); err != nil {
		return Batch{}, invalid("", "could not read all source accounts")
	}
	if _, err = Validate(batch); err != nil {
		return Batch{}, err
	}
	return batch, nil
}

func sourceRow(values [10]any, encoding string) (Row, error) {
	r := Row{}
	id, ok := values[0].(int64)
	if !ok || id <= 0 {
		return r, invalid("", "source row ID must be a positive SQLite integer")
	}
	r.ID = strconv.FormatInt(id, 10)
	fields := map[int]*string{1: &r.Username, 2: &r.PasswordHash, 3: &r.Role, 5: &r.DisplayName, 6: &r.UILocale, 7: &r.CorrespondenceLocale}
	for i, dest := range fields {
		value, ok := values[i].(string)
		if !ok {
			return Row{}, invalid(r.ID, "source text is null, oversized or not text")
		}
		*dest = value
	}
	disabled, ok := values[4].(int64)
	if !ok || (disabled != 0 && disabled != 1) {
		return Row{}, invalid(r.ID, "source disabled flag must be integer 0 or 1")
	}
	r.Disabled = disabled == 1
	created, ok := sourceTime(values[8], encoding)
	if !ok {
		return Row{}, invalid(r.ID, "unsupported source creation timestamp")
	}
	r.CreatedMS = created
	if values[9] != nil {
		last, ok := sourceTime(values[9], encoding)
		if !ok {
			return Row{}, invalid(r.ID, "unsupported source last-login timestamp")
		}
		r.LastLoginMS = &last
	}
	return r, nil
}

func sourceTime(value any, encoding string) (int64, bool) {
	n, ok := value.(int64)
	if !ok || n < 0 {
		return 0, false
	}
	if encoding == "unix_seconds" {
		if n > MaxTimestampMS/1000 {
			return 0, false
		}
		n *= 1000
	}
	return n, n <= MaxTimestampMS
}

// Package accountimport validates the bounded, explicit ADR 014 account transfer.
// Source credentials are data for the hub, never report fields or error details.
package accountimport

import (
	"crypto/sha256"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mateusz-klatt/iris/internal/consoleauth"
)

const (
	MaxRows                = 32
	MaxManifestBytes       = 32768
	MaxTimestampMS   int64 = 253402300799999
)

type Manifest struct {
	Version           int               `json:"version"`
	SourceID          string            `json:"source_id"`
	Table             string            `json:"table"`
	TimestampEncoding string            `json:"timestamp_encoding"`
	TimestampZone     string            `json:"timestamp_zone"`
	Renames           map[string]string `json:"renames"`
	Locales           map[string]string `json:"locales"`
}

type Row struct {
	ID                   string `json:"id"`
	Username             string `json:"username"`
	PasswordHash         string `json:"password_hash"`
	Role                 string `json:"role"`
	Disabled             bool   `json:"disabled"`
	DisplayName          string `json:"display_name"`
	UILocale             string `json:"ui_locale"`
	CorrespondenceLocale string `json:"correspondence_locale"`
	CreatedMS            int64  `json:"created_ms"`
	LastLoginMS          *int64 `json:"last_login_ms"`
}

type Batch struct {
	Manifest Manifest `json:"manifest"`
	Rows     []Row    `json:"rows"`
}

// PreparedRow stays inside trusted hub code. In particular, no JSON projection
// may expose its original credentials or internal ledger fingerprint.
type PreparedRow struct {
	Original    Row      `json:"-"`
	Account     Row      `json:"-"`
	LoginKey    string   `json:"-"`
	Fingerprint [32]byte `json:"-"`
}

// Error contains only a validated numeric source row ID and a fixed reason.
type Error struct{ RowID, Reason string }

func (e *Error) Error() string {
	if e.RowID == "" {
		return e.Reason
	}
	return "source row " + e.RowID + ": " + e.Reason
}

func invalid(id, reason string) error { return &Error{RowID: id, Reason: reason} }

func ValidSourceID(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for i, c := range []byte(value) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		if i == 0 || !strings.ContainsRune("._:-", rune(c)) {
			return false
		}
	}
	return true
}

func ValidRowID(value string) bool {
	n, err := strconv.ParseInt(value, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == value
}

func tableName(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for i, c := range []byte(value) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}

func ValidateManifest(m Manifest) error {
	if m.Version != 1 || !ValidSourceID(m.SourceID) || !tableName(m.Table) || (m.TimestampEncoding != "unix_seconds" && m.TimestampEncoding != "unix_milliseconds") || m.TimestampZone != "UTC" || len(m.Renames) > MaxRows || len(m.Locales) > 2*MaxRows {
		return invalid("", "invalid or unsupported account import manifest")
	}
	for id, name := range m.Renames {
		if !ValidRowID(id) {
			return invalid("", "invalid rename row ID")
		}
		if _, err := consoleauth.LoginKey(name); err != nil {
			return invalid(id, "invalid reviewed username")
		}
	}
	for from, to := range m.Locales {
		if !utf8.ValidString(from) || len(from) > 64 || !consoleauth.ValidLocale(to) {
			return invalid("", "invalid reviewed locale mapping")
		}
	}
	return nil
}

// DecodeManifest rejects unknown fields (including account-embedded assignments)
// and trailing JSON. It never includes decoder errors or source text in output.
func DecodeManifest(reader io.Reader) (Manifest, error) {
	var m Manifest
	if err := DecodeJSON(reader, &m); err != nil {
		return Manifest{}, err
	}
	return m, ValidateManifest(m)
}

func locale(value string, mappings map[string]string) (string, bool) {
	if mapped, ok := mappings[value]; ok {
		return mapped, true
	}
	return value, consoleauth.ValidLocale(value)
}

// Validate repeats all source and mapping checks independently of the adapter.
// The hub calls it again inside its apply transaction, before ledger comparisons.
func Validate(batch Batch) ([]PreparedRow, error) {
	m := batch.Manifest
	if err := ValidateManifest(m); err != nil {
		return nil, err
	}
	if len(batch.Rows) == 0 || len(batch.Rows) > MaxRows {
		return nil, invalid("", "account batch must contain 1 to 32 rows")
	}
	seen := map[string]bool{}
	usedLocales := map[string]bool{}
	keys := map[string]bool{}
	prepared := make([]PreparedRow, 0, len(batch.Rows))
	for _, r := range batch.Rows {
		if !ValidRowID(r.ID) {
			return nil, invalid("", "invalid source row ID")
		}
		if seen[r.ID] {
			return nil, invalid(r.ID, "duplicate source row ID")
		}
		seen[r.ID] = true
		if _, err := consoleauth.LoginKey(r.Username); err != nil {
			return nil, invalid(r.ID, "invalid source username")
		}
		if !strings.HasPrefix(r.PasswordHash, "scrypt$") || consoleauth.ValidateHash(r.PasswordHash) != nil {
			return nil, invalid(r.ID, "unsupported source password record")
		}
		if !utf8.ValidString(r.Role) || len(r.Role) > 64 || consoleauth.ValidateDisplayName(r.DisplayName) != nil || !utf8.ValidString(r.UILocale) || len(r.UILocale) > 64 || !utf8.ValidString(r.CorrespondenceLocale) || len(r.CorrespondenceLocale) > 64 {
			return nil, invalid(r.ID, "invalid source profile")
		}
		if r.CreatedMS < 0 || r.CreatedMS > MaxTimestampMS || r.LastLoginMS != nil && (*r.LastLoginMS < r.CreatedMS || *r.LastLoginMS > MaxTimestampMS) {
			return nil, invalid(r.ID, "unsupported source timestamp")
		}
		a := r
		renamed, hasRename := m.Renames[r.ID]
		if hasRename {
			a.Username = renamed
		}
		key, _ := consoleauth.LoginKey(a.Username) // both original and manifest names were validated
		if keys[key] {
			return nil, invalid(r.ID, "login key collision requires reviewed renames")
		}
		keys[key] = true
		var ok bool
		if a.UILocale, ok = locale(r.UILocale, m.Locales); !ok {
			return nil, invalid(r.ID, "unsupported UI locale requires reviewed mapping")
		}
		if a.CorrespondenceLocale, ok = locale(r.CorrespondenceLocale, m.Locales); !ok {
			return nil, invalid(r.ID, "unsupported correspondence locale requires reviewed mapping")
		}
		usedLocales[r.UILocale], usedLocales[r.CorrespondenceLocale] = true, true
		switch r.Role {
		case "admin":
			a.Role = "operator"
		case "member":
			a.Role = "member"
		default:
			a.Role = "none"
			a.Disabled = true
		}
		// Canonical JSON fixes field order and includes the original validated row
		// and only its relevant reviewed mappings. Unrelated batch rows cannot
		// alter a recorded row's identity on a later bounded rerun.
		canonical, _ := json.Marshal(struct {
			Version                                           int
			Table, Encoding, Zone                             string
			Original                                          Row
			Username, UILocale, CorrespondenceLocale          string
			HasRename, HasUIMapping, HasCorrespondenceMapping bool
		}{1, m.Table, m.TimestampEncoding, m.TimestampZone, r, a.Username, a.UILocale, a.CorrespondenceLocale, hasRename, hasKey(m.Locales, r.UILocale), hasKey(m.Locales, r.CorrespondenceLocale)})
		prepared = append(prepared, PreparedRow{Original: r, Account: a, LoginKey: key, Fingerprint: sha256.Sum256(canonical)})
	}
	for id := range m.Renames {
		if !seen[id] {
			return nil, invalid(id, "rename names a row outside this batch")
		}
	}
	for from := range m.Locales {
		if !usedLocales[from] {
			return nil, invalid("", "locale mapping is unused in this batch")
		}
	}
	return prepared, nil
}

func hasKey(mapping map[string]string, key string) bool { _, ok := mapping[key]; return ok }

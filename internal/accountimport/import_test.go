package accountimport

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func testManifest() Manifest {
	return Manifest{Version: 1, SourceID: "synthetic-accounts", Table: "accounts", TimestampEncoding: "unix_seconds", TimestampZone: "UTC"}
}

func testRow() Row {
	// Synthetic records are assembled in memory and never written to fixtures or
	// included in assertion diagnostics.
	record := "scrypt$16384$8$1$" + base64.RawURLEncoding.EncodeToString(make([]byte, 16)) + "$" + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	return Row{ID: "1", Username: "Person", PasswordHash: record, Role: "member", DisplayName: "Synthetic person", UILocale: "en", CorrespondenceLocale: "pl", CreatedMS: 1000}
}

func testBatch() Batch { return Batch{Manifest: testManifest(), Rows: []Row{testRow()}} }

func TestValidatedRowsPreserveCredentialsHistoryAndQuarantineUnknownRoles(t *testing.T) {
	b := testBatch()
	last := int64(2000)
	b.Rows[0].LastLoginMS = &last
	for _, role := range []string{"admin", "member", "unrecognized"} {
		b.Rows[0].Role = role
		p, err := Validate(b)
		if err != nil {
			t.Fatal("valid synthetic row refused")
		}
		a := p[0].Account
		if a.PasswordHash != b.Rows[0].PasswordHash || a.CreatedMS != 1000 || a.LastLoginMS == nil || *a.LastLoginMS != 2000 || p[0].Original != b.Rows[0] || p[0].LoginKey != "person" {
			t.Fatal("validated source history or credentials changed")
		}
		want := map[string]string{"admin": "operator", "member": "member", "unrecognized": "none"}[role]
		if a.Role != want || a.Disabled != (role == "unrecognized") {
			t.Fatal("role authority was not mapped conservatively")
		}
	}
	b.Rows[0].Disabled = true
	b.Rows[0].Role = "admin"
	p, err := Validate(b)
	if err != nil || !p[0].Account.Disabled {
		t.Fatal("disabled source operator was enabled")
	}
}

func TestReviewedMappingsAndCanonicalFingerprint(t *testing.T) {
	b := testBatch()
	b.Rows[0].UILocale = "unsupported"
	b.Manifest.Locales = map[string]string{"unsupported": ""}
	b.Manifest.Renames = map[string]string{"1": "Reviewed"}
	p, err := Validate(b)
	if err != nil || p[0].Account.UILocale != "" || p[0].Account.Username != "Reviewed" || p[0].LoginKey != "reviewed" {
		t.Fatal("reviewed mapping was not applied")
	}
	first := p[0].Fingerprint
	p, err = Validate(b)
	if err != nil || p[0].Fingerprint != first {
		t.Fatal("exact input changed its fingerprint")
	}
	second := testRow()
	second.ID = "2"
	second.Username = "Another"
	b.Rows = append(b.Rows, second)
	p, err = Validate(b)
	if err != nil || p[0].Fingerprint != first {
		t.Fatal("unrelated row changed the canonical fingerprint")
	}
	b.Manifest.Renames["1"] = "Another reviewed"
	p, err = Validate(b)
	if err != nil || p[0].Fingerprint == first {
		t.Fatal("changed reviewed mapping retained fingerprint")
	}
	b.Manifest.Renames["1"] = "Reviewed"
	b.Rows[0].PasswordHash = strings.Replace(b.Rows[0].PasswordHash, "$1$", "$2$", 1)
	if _, err = Validate(b); err == nil {
		t.Fatal("unsupported source hash was accepted")
	}
}

func TestNoOpReviewedMappingStillChangesTheRecordedManifest(t *testing.T) {
	b := testBatch()
	plain, err := Validate(b)
	if err != nil {
		t.Fatal("synthetic row refused")
	}
	for _, mutate := range []func(*Manifest){
		func(m *Manifest) { m.Renames = map[string]string{"1": "Person"} },
		func(m *Manifest) { m.Locales = map[string]string{"en": "en"} },
		func(m *Manifest) { m.Locales = map[string]string{"pl": "pl"} },
	} {
		m := testManifest()
		mutate(&m)
		b.Manifest = m
		mapped, err := Validate(b)
		if err != nil || mapped[0].Account != plain[0].Account || mapped[0].Fingerprint == plain[0].Fingerprint {
			t.Fatal("reviewed mapping identity was lost")
		}
	}
}

func TestRowsFailAsAnEntireBoundedBatchWithSanitizedReasons(t *testing.T) {
	cases := map[string]func(*Batch){
		"empty":                       func(b *Batch) { b.Rows = nil },
		"too-many":                    func(b *Batch) { b.Rows = make([]Row, MaxRows+1) },
		"invalid-id":                  func(b *Batch) { b.Rows[0].ID = "credential-like-invalid-id" },
		"duplicate-id":                func(b *Batch) { b.Rows = append(b.Rows, b.Rows[0]) },
		"invalid-name":                func(b *Batch) { b.Rows[0].Username = " untrimmed" },
		"malformed-hash":              func(b *Batch) { b.Rows[0].PasswordHash = "private-sentinel" },
		"invalid-role":                func(b *Batch) { b.Rows[0].Role = string([]byte{255}) },
		"long-role":                   func(b *Batch) { b.Rows[0].Role = strings.Repeat("x", 65) },
		"invalid-display":             func(b *Batch) { b.Rows[0].DisplayName = strings.Repeat("x", 129) },
		"invalid-ui-utf8":             func(b *Batch) { b.Rows[0].UILocale = string([]byte{255}) },
		"long-ui":                     func(b *Batch) { b.Rows[0].UILocale = strings.Repeat("x", 65) },
		"invalid-correspondence-utf8": func(b *Batch) { b.Rows[0].CorrespondenceLocale = string([]byte{255}) },
		"long-correspondence":         func(b *Batch) { b.Rows[0].CorrespondenceLocale = strings.Repeat("x", 65) },
		"negative-time":               func(b *Batch) { b.Rows[0].CreatedMS = -1 },
		"too-late-time":               func(b *Batch) { b.Rows[0].CreatedMS = MaxTimestampMS + 1 },
		"last-before-create":          func(b *Batch) { n := int64(0); b.Rows[0].LastLoginMS = &n },
		"last-too-late":               func(b *Batch) { n := MaxTimestampMS + 1; b.Rows[0].LastLoginMS = &n },
		"collision":                   func(b *Batch) { r := testRow(); r.ID = "2"; r.Username = "PERSON"; b.Rows = append(b.Rows, r) },
		"ui-locale":                   func(b *Batch) { b.Rows[0].UILocale = "unsupported" },
		"correspondence-locale":       func(b *Batch) { b.Rows[0].CorrespondenceLocale = "unsupported" },
		"unused-rename":               func(b *Batch) { b.Manifest.Renames = map[string]string{"2": "Missing"} },
		"unused-locale":               func(b *Batch) { b.Manifest.Locales = map[string]string{"unused": ""} },
		"invalid-manifest":            func(b *Batch) { b.Manifest.Version = 2 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := testBatch()
			mutate(&b)
			p, err := Validate(b)
			if err == nil || p != nil {
				t.Fatal("invalid batch exposed prepared rows")
			}
			if strings.Contains(err.Error(), "private-sentinel") || strings.Contains(err.Error(), "credential-like") || strings.Contains(err.Error(), testRow().PasswordHash) {
				t.Fatal("refusal disclosed source data")
			}
		})
	}
	if (&Error{Reason: "safe"}).Error() != "safe" || (&Error{RowID: "1", Reason: "safe"}).Error() != "source row 1: safe" {
		t.Fatal("sanitized error lost safe row identity")
	}
}

func TestManifestBoundsAndIdentifiers(t *testing.T) {
	for _, value := range []string{"", "-start", "x/y", "has space", "ł", strings.Repeat("a", 65)} {
		if ValidSourceID(value) {
			t.Fatal("invalid source identity accepted")
		}
	}
	for _, value := range []string{"a", "A0.a_b-c:d", strings.Repeat("a", 64)} {
		if !ValidSourceID(value) {
			t.Fatal("safe source identity refused")
		}
	}
	for _, value := range []string{"", "0", "-1", "+1", "01", "1.0", "9223372036854775808"} {
		if ValidRowID(value) {
			t.Fatal("unsafe source row identity accepted")
		}
	}
	if !ValidRowID("9223372036854775807") {
		t.Fatal("positive integer identity refused")
	}
	for _, value := range []string{"", "1table", "bad-name", strings.Repeat("a", 65)} {
		if tableName(value) {
			t.Fatal("unsafe table name accepted")
		}
	}
	if !tableName("_Accounts2") {
		t.Fatal("safe explicit table name refused")
	}
	for name, mutate := range map[string]func(*Manifest){
		"version": func(m *Manifest) { m.Version = 2 }, "source": func(m *Manifest) { m.SourceID = "/" }, "table": func(m *Manifest) { m.Table = "bad-name" },
		"encoding": func(m *Manifest) { m.TimestampEncoding = "guess" }, "zone": func(m *Manifest) { m.TimestampZone = "local" },
		"rename-id": func(m *Manifest) { m.Renames = map[string]string{"00": "Person"} }, "rename-name": func(m *Manifest) { m.Renames = map[string]string{"1": " "} },
		"locale-source-utf8": func(m *Manifest) { m.Locales = map[string]string{string([]byte{255}): ""} }, "locale-source-long": func(m *Manifest) { m.Locales = map[string]string{strings.Repeat("x", 65): ""} },
		"locale-target": func(m *Manifest) { m.Locales = map[string]string{"old": "unsupported"} },
		"rename-bound": func(m *Manifest) {
			m.Renames = map[string]string{}
			for i := 0; i <= MaxRows; i++ {
				m.Renames[strings.Repeat("1", i+1)] = "Person"
			}
		},
		"locale-bound": func(m *Manifest) {
			m.Locales = map[string]string{}
			for i := 0; i <= 2*MaxRows; i++ {
				m.Locales[strings.Repeat("x", i)] = ""
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := testManifest()
			mutate(&m)
			if ValidateManifest(m) == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, errors.New("private-sentinel") }

func TestManifestJSONIsStrictBoundedAndSanitized(t *testing.T) {
	raw, _ := json.Marshal(testManifest())
	m, err := DecodeManifest(bytes.NewReader(raw))
	if err != nil || !reflect.DeepEqual(m, testManifest()) {
		t.Fatal("valid manifest did not round trip")
	}
	inputs := []io.Reader{
		brokenReader{}, strings.NewReader(strings.Repeat(" ", MaxManifestBytes+1)), strings.NewReader("{"),
		strings.NewReader(string(raw) + " {}"), strings.NewReader(`{"source_id":"a","source_id":"b"}`),
		strings.NewReader(`{"Source_ID":"case-variant"}`), strings.NewReader(`{"assignments":[]}`), strings.NewReader(`{"renames":{"1":"a","1":"b"}}`),
		strings.NewReader("{\"source_id\":\"\xff\"}"), strings.NewReader(`{"source_id":"\ud800"}`), strings.NewReader(`{"source_id":"\udc00"}`),
		strings.NewReader(`{"source_id":"\ud800\u0061"}`), strings.NewReader(`{"source_id":"\ud800\uzzzz"}`), strings.NewReader(`{"source_id":"\uzzzz"}`),
		strings.NewReader(`{"source_id":"\u12`), strings.NewReader("\\"), strings.NewReader(`{"source_id":"\ud800`),
		strings.NewReader(`{"version":"wrong"}`), strings.NewReader(`{"renames":[]}`), strings.NewReader(`{"renames":{"1":{}}}`), strings.NewReader(`[]`),
		strings.NewReader(strings.Repeat("[", 18) + strings.Repeat("]", 18)), strings.NewReader(`{"version":1,`), strings.NewReader(`{"version":`),
		strings.NewReader(`{"version":1`), strings.NewReader(`{"renames":[`), strings.NewReader(`{"renames":[1`),
	}
	for i, input := range inputs {
		_, err := DecodeManifest(input)
		if err == nil || strings.Contains(err.Error(), "private-sentinel") {
			t.Fatalf("invalid manifest input %d escaped safe rejection", i)
		}
	}
	var generic struct {
		Values []string          `json:"values"`
		Map    map[string]string `json:"map"`
	}
	if err := DecodeJSON(strings.NewReader(`{"values":["a","\ud83d\ude00","\\u"],"map":null}`), &generic); err != nil || len(generic.Values) != 3 {
		t.Fatal("valid arrays, escapes or null map were refused")
	}
	if err := DecodeJSON(strings.NewReader(`{"values":null,"map":{"a":"value"}}`), &generic); err != nil || generic.Values != nil || generic.Map["a"] != "value" {
		t.Fatal("valid null slice or map was refused")
	}
	for _, input := range []string{`{"values":{}}`, `{"values":[{}]}`, `{"values":[null]}`, `{"map":[]}`, `{"map":{"a":[]}}`} {
		if DecodeJSON(strings.NewReader(input), &generic) == nil {
			t.Fatal("wrongly typed nested fields accepted")
		}
	}
	if DecodeJSON(strings.NewReader(`{}`), nil) == nil {
		t.Fatal("nil decoder destination accepted")
	}
	var withArray struct {
		Values [1]string `json:"values"`
	}
	if DecodeJSON(strings.NewReader(`{"values":["a"]}`), &withArray) != nil || withArray.Values[0] != "a" {
		t.Fatal("fixed array decoding failed")
	}
}

func TestWireRowsRequireEverySourceFieldAndRefusePrimitiveNull(t *testing.T) {
	b := testBatch()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal("could not serialize synthetic batch")
	}
	var decoded Batch
	if DecodeBatchJSON(bytes.NewReader(raw), &decoded) != nil || len(decoded.Rows) != 1 || !reflect.DeepEqual(decoded, b) {
		t.Fatal("complete source row failed strict transport")
	}
	last := int64(2000)
	b.Rows[0].LastLoginMS = &last
	raw, _ = json.Marshal(b)
	if DecodeBatchJSON(bytes.NewReader(raw), &decoded) != nil || decoded.Rows[0].LastLoginMS == nil || *decoded.Rows[0].LastLoginMS != last {
		t.Fatal("source history pointer failed strict transport")
	}
	rowRaw, _ := json.Marshal(testRow())
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(rowRaw, &fields); err != nil {
		t.Fatal("could not decode synthetic row fields")
	}
	for _, name := range []string{"id", "username", "password_hash", "role", "disabled", "display_name", "ui_locale", "correspondence_locale", "created_ms", "last_login_ms"} {
		original := fields[name]
		delete(fields, name)
		missing, _ := json.Marshal(fields)
		var row Row
		if json.Unmarshal(missing, &row) == nil {
			t.Fatal("incomplete source row silently fabricated a field")
		}
		fields[name] = json.RawMessage(`null`)
		null, _ := json.Marshal(fields)
		if name != "last_login_ms" && json.Unmarshal(null, &row) == nil {
			t.Fatal("null source scalar silently became a default")
		}
		fields[name] = original
	}
	var row Row
	if row.UnmarshalJSON([]byte("{")) == nil {
		t.Fatal("invalid row JSON was accepted")
	}
	var generic struct {
		Optional *int           `json:"optional"`
		Values   []*int         `json:"values"`
		Map      map[string]int `json:"map"`
	}
	if DecodeJSON(strings.NewReader(`{"optional":null,"values":[null,1],"map":{"key":2}}`), &generic) != nil || generic.Optional != nil || len(generic.Values) != 2 || generic.Values[0] != nil || *generic.Values[1] != 1 {
		t.Fatal("valid nullable pointer semantics were lost")
	}
	if DecodeJSON(strings.NewReader(`{"map":{"key":null}}`), &generic) == nil {
		t.Fatal("null scalar map entry accepted")
	}
	if DecodeJSON(strings.NewReader(`{"values":[{}]}`), &generic) == nil {
		t.Fatal("wrongly typed slice item accepted")
	}
	if DecodeJSON(strings.NewReader(`null`), &generic) == nil {
		t.Fatal("null root object accepted")
	}
	var plain int
	if DecodeJSON(strings.NewReader(`null`), &plain) == nil {
		t.Fatal("null root scalar accepted")
	}
}

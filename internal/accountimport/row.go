package accountimport

import (
	"bytes"
	"encoding/json"
)

// UnmarshalJSON requires every source field on the trusted CLI-to-hub wire.
// Omitting disabled or a timestamp cannot invent an enabled account or history.
// The nullable last login must be present explicitly, including when it is null.
func (r *Row) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return invalid("", "invalid source row JSON")
	}
	for _, name := range []string{"id", "username", "password_hash", "role", "disabled", "display_name", "ui_locale", "correspondence_locale", "created_ms", "last_login_ms"} {
		if _, ok := fields[name]; !ok {
			return invalid("", "incomplete source row JSON")
		}
	}
	type wireRow Row
	var decoded wireRow
	if err := DecodeBatchJSON(bytes.NewReader(raw), &decoded); err != nil {
		return err
	}
	*r = Row(decoded)
	return nil
}

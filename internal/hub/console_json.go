package hub

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/mateusz-klatt/iris/internal/store"
)

// consoleDecode preserves credential Unicode exactly. encoding/json otherwise
// replaces malformed UTF-8 and unpaired surrogate escapes with U+FFFD before
// password and username validation can see them. ServeHTTP bounds this body.
func consoleDecode(w http.ResponseWriter, r *http.Request, out any) bool {
	raw, err := io.ReadAll(r.Body)
	if err != nil || !validConsoleJSONUnicode(raw) {
		writeError(w, &store.Error{Code: "bad_request", Message: "invalid console request"})
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(out); err != nil {
		writeError(w, &store.Error{Code: "bad_request", Message: "invalid console request"})
		return false
	}
	if decoder.Decode(new(any)) != io.EOF {
		writeError(w, &store.Error{Code: "bad_request", Message: "one JSON object is required"})
		return false
	}
	return true
}

// This scan only adds the Unicode strictness missing from encoding/json. The
// decoder still owns JSON syntax, field types and unknown-field rejection.
func validConsoleJSONUnicode(raw []byte) bool {
	if !utf8.Valid(raw) {
		return false
	}
	quoted := false
	for i := 0; i < len(raw); i++ {
		if raw[i] == '"' {
			quoted = !quoted
			continue
		}
		if !quoted || raw[i] != '\\' {
			continue
		}
		i++
		if i == len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue // Escaped quotes/backslashes do not open strings or Unicode escapes.
		}
		if i+4 >= len(raw) {
			return false
		}
		code, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if code >= 0xd800 && code <= 0xdbff {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		} else if code >= 0xdc00 && code <= 0xdfff {
			return false
		}
	}
	return !quoted
}

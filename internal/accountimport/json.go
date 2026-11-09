package accountimport

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

// DecodeJSON is the manifest decoder. JSON field names are exact, object keys
// cannot repeat, and invalid Unicode cannot be repaired into a different name.
// Refusals never include input text or decoder diagnostics.
func DecodeJSON(reader io.Reader, out any) error {
	return decodeJSON(reader, out, MaxManifestBytes)
}

// DecodeBatchJSON applies the same strict rules to the hub's 64 KiB bounded
// account batch transport. Local mapping manifests retain their smaller bound.
func DecodeBatchJSON(reader io.Reader, out any) error {
	return decodeJSON(reader, out, 64<<10)
}

func decodeJSON(reader io.Reader, out any, limit int64) error {
	raw, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return invalid("", "could not read bounded import manifest")
	}
	if !utf8.Valid(raw) || !validEscapes(raw) {
		return invalid("", "invalid Unicode in import manifest")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	value, err := jsonValue(dec, 0)
	if err != nil {
		return invalid("", "invalid or duplicate import manifest field")
	}
	if _, err = dec.Token(); !errors.Is(err, io.EOF) {
		return invalid("", "trailing import manifest data")
	}
	outType := reflect.TypeOf(out)
	if outType != nil && outType.Kind() == reflect.Pointer {
		outType = outType.Elem()
	}
	if !exactFields(value, outType) {
		return invalid("", "unknown import manifest field")
	}
	dec = json.NewDecoder(bytes.NewReader(raw))
	if err = dec.Decode(out); err != nil {
		return invalid("", "invalid import manifest JSON")
	}
	return nil
}

func validEscapes(raw []byte) bool {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		n, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(raw) || string(raw[i+1:i+3]) != "\\u" {
				return false
			}
			low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}

func jsonValue(dec *json.Decoder, depth int) (any, error) {
	if depth > 16 {
		return nil, errors.New("manifest nesting exceeds bound")
	}
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch token {
	case json.Delim('{'):
		object := map[string]any{}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return nil, err
			}
			name := key.(string) // Decoder guarantees object keys are strings.
			if _, ok := object[name]; ok {
				return nil, errors.New("duplicate object key")
			}
			value, err := jsonValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			object[name] = value
		}
		_, err = dec.Token()
		return object, err
	case json.Delim('['):
		var array []any
		for dec.More() {
			value, err := jsonValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		_, err = dec.Token()
		return array, err
	default:
		return token, nil
	}
}

func exactFields(value any, typ reflect.Type) bool {
	if typ == nil {
		return false
	}
	for typ.Kind() == reflect.Pointer {
		if value == nil {
			return true
		}
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return false
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name != "-" && field.IsExported() {
				fields[name] = field.Type
			}
		}
		for name, item := range object {
			field, ok := fields[name]
			if !ok || !exactFields(item, field) {
				return false
			}
		}
	case reflect.Map:
		if value == nil {
			return true
		}
		object, ok := value.(map[string]any)
		if !ok {
			return false
		}
		for _, item := range object {
			if !exactFields(item, typ.Elem()) {
				return false
			}
		}
	case reflect.Slice, reflect.Array:
		if value == nil {
			return true
		}
		array, ok := value.([]any)
		if !ok {
			return false
		}
		for _, item := range array {
			if !exactFields(item, typ.Elem()) {
				return false
			}
		}
	default:
		if value == nil {
			return false
		}
	}
	return true
}

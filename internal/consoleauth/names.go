package consoleauth

import (
	"errors"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// ValidLocale accepts the console's supported catalog or an unset preference.
// Import mappings must choose one explicitly for unsupported source locales.
func ValidLocale(locale string) bool {
	return locale == "" || slices.Contains(strings.Fields("ar bn bs cs da de el en es fa fi fil fr ga he hi hr hu hy id is it ja ko lt lv ms my-MM nl no pl pt ro ru sk sq sr sv sw th tr uk vi zh-Hant zh"), locale)
}

var (
	ErrInvalidUsername    = errors.New("invalid username")
	ErrInvalidDisplayName = errors.New("invalid display name")
)

// LoginKey validates a display username and computes its unique lookup key using
// the Unicode behavior of the pinned x/text dependency: NFC, case fold, NFC.
// It never trims or rewrites the username saved for display.
func LoginKey(username string) (string, error) {
	if username == "" || !utf8.ValidString(username) || utf8.RuneCountInString(username) > 64 || strings.TrimSpace(username) != username {
		return "", ErrInvalidUsername
	}
	for _, c := range username {
		if unicode.IsControl(c) {
			return "", ErrInvalidUsername
		}
	}
	return norm.NFC.String(cases.Fold().String(norm.NFC.String(username))), nil
}

// ValidateDisplayName bounds text saved for display. An empty display name uses
// the username in views; profile text never supplies authority.
func ValidateDisplayName(name string) error {
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > 128 {
		return ErrInvalidDisplayName
	}
	return nil
}

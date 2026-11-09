package consoleauth

import (
	"errors"
	"strings"
	"testing"
)

func TestLoginKeyUnicodeAndBounds(t *testing.T) {
	for _, test := range []struct{ name, key string }{
		{"Alice", "alice"}, {"Straße", "strasse"}, {"STRASSE", "strasse"},
		{"A\u030angström", "ångström"}, {"ÅNGSTRÖM", "ångström"},
		{"\u212aelvin", "kelvin"}, {"with space", "with space"},
		{strings.Repeat("\U0001f600", 64), strings.Repeat("\U0001f600", 64)},
	} {
		key, err := LoginKey(test.name)
		if err != nil || key != test.key {
			t.Fatal("Unicode login key normalization or allowed name changed")
		}
	}
	for _, name := range []string{"", "\xff", " leading", "trailing ", "\u2003leading", "trailing\u00a0", "tab\tinside", "line\ninside", "nul\x00inside", "delete\x7f", strings.Repeat("a", 65), strings.Repeat("\U0001f600", 65)} {
		if key, err := LoginKey(name); key != "" || !errors.Is(err, ErrInvalidUsername) {
			t.Fatal("invalid username produced a lookup key")
		}
	}
}

func TestDisplayNameBounds(t *testing.T) {
	for _, name := range []string{"", "Account display name", strings.Repeat("\U0001f600", 128)} {
		if ValidateDisplayName(name) != nil {
			t.Fatal("bounded display text refused")
		}
	}
	for _, name := range []string{"\xff", strings.Repeat("a", 129), strings.Repeat("\U0001f600", 129)} {
		if !errors.Is(ValidateDisplayName(name), ErrInvalidDisplayName) {
			t.Fatal("invalid display text accepted")
		}
	}
}

func TestLocalePreferencesRequireCatalogValuesOrExplicitUnset(t *testing.T) {
	for _, locale := range []string{"", "en", "pl", "my-MM", "zh-Hant"} {
		if !ValidLocale(locale) {
			t.Fatal("supported or unset locale refused")
		}
	}
	for _, locale := range []string{"EN", "en-US", " pl", "pl ", "unsupported", "\xff"} {
		if ValidLocale(locale) {
			t.Fatal("unsupported locale silently reinterpreted")
		}
	}
}

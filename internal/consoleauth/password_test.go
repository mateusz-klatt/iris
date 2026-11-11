package consoleauth

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// The synthetic scrypt vectors were computed independently with Python's
// hashlib.scrypt, not by this verifier. They contain no real account data.
const syntheticScrypt = "scrypt$16384$8$1$AAECAwQFBgcICQoLDA0ODw$5UJCybzgeNrUxZTV5mzrOYr8Aj1OlDXLrgTFXHxr1m4"
const emptyScrypt = "scrypt$16384$8$1$AAECAwQFBgcICQoLDA0ODw$0xv24VsXC2JG-Z8qF_PLNl4lrO4CjCW5okKsht1CLoY"

// Independent argon2-cffi vectors use salt bytes 0 through 15 and the approved
// profile. Earlier upgrades could retain these passwords below today's minimum.
const shortArgon = argonPrefix + "AAECAwQFBgcICQoLDA0ODw$h/bekxvFlzSTcN+YWUQEsQ0HPChzNY74xJtz/WE2x04"
const emptyArgon = argonPrefix + "AAECAwQFBgcICQoLDA0ODw$yr2OtlrITpa5bhUEe12Ewh3PDTh976IiDH1Kbdv5Kmw"
const syntheticArgon = argonPrefix + "AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestSyntheticScryptRecordsAreUnreadable(t *testing.T) {
	// Retired records must fail validation before claiming password capacity.
	occupyKDF(t)
	for _, test := range []struct{ password, record string }{{"x", syntheticScrypt}, {"", emptyScrypt}} {
		if !errors.Is(ValidateHash(test.record), ErrInvalidHash) {
			t.Fatal("retired scrypt record passed validation")
		}
		ok, err := VerifyPassword(test.password, test.record)
		if !errors.Is(err, ErrInvalidHash) || ok {
			t.Fatal("retired scrypt record reached password verification")
		}
	}
}

func TestArgonCreationAndExactPasswordBytes(t *testing.T) {
	password := "  untrimmed \U0001f600e\u0301 password  "
	first, err := CreatePassword(password)
	if err != nil || !strings.HasPrefix(first, argonPrefix) {
		t.Fatal("new password did not use approved Argon2id profile")
	}
	second, err := CreatePassword(password)
	if err != nil || first == second {
		t.Fatal("password creation did not generate fresh salt")
	}
	for _, record := range []string{first, second} {
		if ValidateHash(record) != nil {
			t.Fatal("created password record is unusable")
		}
		if ok, err := VerifyPassword(password, record); err != nil || !ok {
			t.Fatal("created password did not verify")
		}
	}
	for _, changed := range []string{strings.TrimSpace(password), strings.ReplaceAll(password, "e\u0301", "é")} {
		if ok, err := VerifyPassword(changed, first); err != nil || ok {
			t.Fatal("verification transformed submitted password")
		}
	}
}

func TestArgonVerificationKeepsExistingPasswordBounds(t *testing.T) {
	for _, test := range []struct{ password, record string }{{"x", shortArgon}, {"", emptyArgon}} {
		if !errors.Is(ValidateNewPassword(test.password), ErrInvalidPassword) {
			t.Fatal("short existing password met the new-password minimum")
		}
		if ok, err := VerifyPassword(test.password, test.record); err != nil || !ok {
			t.Fatal("short existing Argon2id password did not verify")
		}
		if ok, err := VerifyPassword(test.password+"!", test.record); err != nil || ok {
			t.Fatal("incorrect existing Argon2id password was accepted")
		}
	}
	if ok, err := VerifyPassword(strings.Repeat("x", MaxPasswordBytes), shortArgon); err != nil || ok {
		t.Fatal("verification did not accept the maximum byte length as a mismatch")
	}
}

func TestFixedLengthComparisonRejectsEveryChangedKeyPosition(t *testing.T) {
	fields := strings.Split(shortArgon, "$")
	original, err := base64.RawStdEncoding.DecodeString(fields[5])
	if err != nil || len(original) != 32 {
		t.Fatal("invalid synthetic vector fixture")
	}
	for _, index := range []int{0, 15, 31} {
		changed := append([]byte{}, original...)
		changed[index] ^= 1
		fields[5] = base64.RawStdEncoding.EncodeToString(changed)
		if ok, err := VerifyPassword("x", strings.Join(fields, "$")); err != nil || ok {
			t.Fatal("different 32-byte expected key was accepted")
		}
	}
	for _, size := range []int{0, 1, 31, 33, 64} {
		fields[5] = base64.RawStdEncoding.EncodeToString(make([]byte, size))
		if ok, err := VerifyPassword("x", strings.Join(fields, "$")); !errors.Is(err, ErrInvalidHash) || ok {
			t.Fatal("non-32-byte expected key reached password verification")
		}
	}
}

func TestMalformedHashesFailBeforeKDF(t *testing.T) {
	// Saturation proves malformed input is refused before password admission.
	occupyKDF(t)
	fields := strings.Split(syntheticArgon, "$")
	mutate := func(index int, value string) string {
		copyFields := append([]string{}, fields...)
		copyFields[index] = value
		return strings.Join(copyFields, "$")
	}
	invalid := []string{
		"", strings.Repeat("x", MaxHashBytes+1), "$" + syntheticArgon,
		syntheticArgon + "$extra", mutate(0, "argon2id"), mutate(1, "ARGON2ID"),
		mutate(4, "!"), mutate(4, "AAAAAAAAAAAAAAAAAAAAAA=="),
		mutate(4, "AAAAAAAAAAAAAAAAAAAAAB"), // Nonzero unused base64 bits.
		mutate(4, "AAAAAAAAAAAAAAAAAAAAAA\n"),
		mutate(4, "_____________________w"), // URL-safe alphabet is not standard.
		mutate(4, base64.RawStdEncoding.EncodeToString(make([]byte, 15))),
		mutate(4, base64.RawStdEncoding.EncodeToString(make([]byte, 17))),
		mutate(5, "!"), mutate(5, fields[5]+"="),
		argonPrefix + "AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAB",
		"$argon2id$v=18$m=65536,t=3,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"$argon2i$v=19$m=65536,t=3,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$m=4294967296,t=3,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$m=65536,t=0,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$m=65536,t=3,p=0$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$p=1,t=3,m=65536$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	}
	for i, record := range invalid {
		if !errors.Is(ValidateHash(record), ErrInvalidHash) {
			t.Fatalf("malformed record %d passed validation", i)
		}
		if ok, err := VerifyPassword("x", record); !errors.Is(err, ErrInvalidHash) || ok {
			t.Fatalf("malformed record %d reached password work", i)
		}
	}
	// Canonical standard base64 input with both alphabet-specific characters is allowed.
	fields[4] = base64.RawStdEncoding.EncodeToString(bytesOf(16, 0xfb))
	fields[5] = base64.RawStdEncoding.EncodeToString(bytesOf(32, 0xff))
	if ValidateHash(strings.Join(fields, "$")) != nil {
		t.Fatal("canonical standard base64 alphabet refused")
	}
}

func bytesOf(size int, value byte) []byte {
	out := make([]byte, size)
	for i := range out {
		out[i] = value
	}
	return out
}

func occupyKDF(t *testing.T) {
	t.Helper()
	for range MaxConcurrentKDF {
		if !admitKDF() {
			t.Fatal("could not reserve advertised password capacity")
		}
	}
	t.Cleanup(func() {
		for range MaxConcurrentKDF {
			<-kdfSlots
		}
	})
}

func TestPasswordPolicyAndAdmission(t *testing.T) {
	for _, password := range []string{strings.Repeat("a", 15), strings.Repeat("\U0001f600", 15), strings.Repeat("\U0001f600", 256), strings.Repeat(" ", 15)} {
		if ValidateNewPassword(password) != nil {
			t.Fatal("password within Unicode and byte bounds refused")
		}
	}
	for _, password := range []string{"", strings.Repeat("a", 14), strings.Repeat("\U0001f600", 14), strings.Repeat("\U0001f600", 257), strings.Repeat("a", 1025), "\xff" + strings.Repeat("a", 15)} {
		if !errors.Is(ValidateNewPassword(password), ErrInvalidPassword) {
			t.Fatal("invalid new password passed policy")
		}
		if record, err := CreatePassword(password); record != "" || !errors.Is(err, ErrInvalidPassword) {
			t.Fatal("invalid new password reached password work")
		}
	}
	occupyKDF(t)
	if record, err := CreatePassword("fifteen characters"); record != "" || !errors.Is(err, ErrBusy) {
		t.Fatal("saturated creation did not refuse immediately")
	}
	if ok, err := VerifyPassword("x", syntheticArgon); ok || !errors.Is(err, ErrBusy) {
		t.Fatal("saturated verification did not refuse immediately")
	}
	for _, password := range []string{strings.Repeat("a", 1025), "\xff"} {
		if ok, err := VerifyPassword(password, syntheticArgon); ok || !errors.Is(err, ErrInvalidPassword) {
			t.Fatal("invalid verification input reached password admission")
		}
	}
}

func BenchmarkArgonCreate(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := CreatePassword("synthetic benchmark password"); err != nil {
			b.Fatal("synthetic Argon2id creation failed")
		}
	}
}

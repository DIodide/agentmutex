package mutex

import (
	"strings"
	"testing"
)

func TestValidateKey(t *testing.T) {
	valid := []string{
		"deploy:staging",
		"account:12345",
		"repo:frontend/file:auth.ts",
		"a",
		"service:api:database",
		"x_y-z.9",
	}
	for _, k := range valid {
		if err := ValidateKey(k); err != nil {
			t.Errorf("ValidateKey(%q) = %v, want nil", k, err)
		}
	}
	invalid := []string{
		"",
		":leading",
		"-leading",
		"has space",
		"has\ttab",
		"emoji🔒",
		strings.Repeat("k", MaxKeyLen+1),
	}
	for _, k := range invalid {
		if err := ValidateKey(k); err == nil {
			t.Errorf("ValidateKey(%q) = nil, want error", k)
		}
	}
}

func TestEncodeDecodeRoundtrip(t *testing.T) {
	keys := []string{
		"deploy:staging",
		"account:12345",
		"repo:frontend/file:auth.ts",
		"plain",
		"dots.and_underscores-ok",
	}
	for _, k := range keys {
		enc := EncodeKey(k)
		if strings.ContainsAny(enc, ":/\\") {
			t.Errorf("EncodeKey(%q) = %q contains unsafe characters", k, enc)
		}
		dec, err := DecodeKey(enc)
		if err != nil {
			t.Fatalf("DecodeKey(%q): %v", enc, err)
		}
		if dec != k {
			t.Errorf("roundtrip: %q -> %q -> %q", k, enc, dec)
		}
	}
}

func TestEncodeWindowsHazards(t *testing.T) {
	// Reserved device names and trailing dots must not survive encoding
	// verbatim, and must still round-trip.
	hazards := []string{"con", "NUL", "com1", "con.backup", "lpt9:deploy", "a."}
	for _, k := range hazards {
		enc := EncodeKey(k)
		stem := strings.ToLower(enc)
		if i := strings.IndexByte(stem, '.'); i >= 0 {
			stem = stem[:i]
		}
		if windowsReserved[stem] {
			t.Errorf("EncodeKey(%q) = %q is still a reserved Windows name", k, enc)
		}
		if strings.HasSuffix(enc, ".") {
			t.Errorf("EncodeKey(%q) = %q has a trailing dot", k, enc)
		}
		dec, err := DecodeKey(enc)
		if err != nil || dec != k {
			t.Errorf("roundtrip: %q -> %q -> %q (%v)", k, enc, dec, err)
		}
	}
}

func TestEncodeDistinct(t *testing.T) {
	// Distinct keys must never collide after encoding.
	a, b := EncodeKey("a:b"), EncodeKey("a%3Ab")
	if a == b {
		t.Errorf("EncodeKey collision: %q vs %q", a, b)
	}
}

func TestNewToken(t *testing.T) {
	a, b := NewToken(), NewToken()
	if len(a) != 32 || a == b {
		t.Errorf("NewToken: want distinct 32-char tokens, got %q, %q", a, b)
	}
}

package mutex

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// MaxKeyLen bounds key length so encoded directory names stay well under
// filesystem name limits (255 bytes) even when every byte is %XX-escaped.
const MaxKeyLen = 80

// ValidateKey checks that key is a usable semantic key. Keys are structured
// namespaces such as "deploy:staging" or "account:12345:balance". They must
// start with an alphanumeric character and may contain letters, digits and
// the separators ". _ - : /".
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("key must not be empty")
	}
	if len(key) > MaxKeyLen {
		return fmt.Errorf("key too long (%d bytes, max %d)", len(key), MaxKeyLen)
	}
	if !isAlnum(key[0]) {
		return fmt.Errorf("key must start with a letter or digit: %q", key)
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if isAlnum(c) || c == '.' || c == '_' || c == '-' || c == ':' || c == '/' {
			continue
		}
		return fmt.Errorf("key contains invalid character %q (allowed: letters, digits, . _ - : /): %q", c, key)
	}
	return nil
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// windowsReserved are device names that Windows refuses (or worse, aliases)
// as file names, with or without an extension.
var windowsReserved = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// EncodeKey converts a semantic key into a filesystem-safe directory name.
// Bytes outside [A-Za-z0-9._-] are percent-encoded, so the mapping is
// reversible and safe on every platform (":" is not legal on Windows).
// Windows-reserved device names and trailing dots are escaped too.
func EncodeKey(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	for i := 0; i < len(key); i++ {
		c := key[i]
		if isAlnum(c) || c == '.' || c == '_' || c == '-' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	name := b.String()
	// "con", "nul", "com1.backup", … are reserved on Windows; escaping the
	// first byte defuses them while DecodeKey still round-trips.
	stem := strings.ToLower(name)
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	if windowsReserved[stem] {
		name = fmt.Sprintf("%%%02X", name[0]) + name[1:]
	}
	// Windows strips trailing dots from names, aliasing "a." to "a".
	if strings.HasSuffix(name, ".") {
		name = name[:len(name)-1] + "%2E"
	}
	return name
}

// DecodeKey reverses EncodeKey.
func DecodeKey(name string) (string, error) {
	var b strings.Builder
	b.Grow(len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c != '%' {
			b.WriteByte(c)
			continue
		}
		if i+2 >= len(name) {
			return "", fmt.Errorf("truncated escape in %q", name)
		}
		v, err := hex.DecodeString(name[i+1 : i+3])
		if err != nil {
			return "", fmt.Errorf("bad escape in %q: %v", name, err)
		}
		b.WriteByte(v[0])
		i += 2
	}
	return b.String(), nil
}

// NewToken returns a fresh lease token: 128 bits of randomness, hex-encoded.
// Only the holder of the token can release or renew the lease.
func NewToken() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand never fails on supported platforms; if it somehow
		// does, panicking beats handing out a guessable token.
		panic(fmt.Sprintf("agentmutex: cannot generate token: %v", err))
	}
	return hex.EncodeToString(buf[:])
}

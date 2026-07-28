package util

import (
	"crypto/md5" //nolint:gosec // MD5 is required by the BookOrbit x-auth-key protocol, not for security.
	"encoding/hex"
	"strings"
)

// MD5Hex returns the lowercase hex MD5 digest of the input string.
//
// BookOrbit authenticates with a static `x-auth-key` header whose value is the
// lowercase hex MD5 of the account password. This is a fixed property of the
// BookOrbit wire protocol, not a security choice on our part.
func MD5Hex(s string) string {
	sum := md5.Sum([]byte(s)) //nolint:gosec // see package note above
	return hex.EncodeToString(sum[:])
}

// IsMD5Hex reports whether s looks like a lowercase hex MD5 digest
// (32 hexadecimal characters). It accepts upper-case hex too, since the digest
// value is case-insensitive before transmission.
func IsMD5Hex(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, r := range s {
		isDigit := r >= '0' && r <= '9'
		isHex := (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if !isDigit && !isHex {
			return false
		}
	}
	return true
}

// LowerNormal returns s lower-cased and trimmed, used to normalize a pre-hashed
// userkey before it is sent on the wire.
func LowerNormal(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

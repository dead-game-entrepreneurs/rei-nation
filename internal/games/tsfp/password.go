package tsfp

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strconv"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/pbkdf2"
)

const legacyPasswordPrefix = "tsfp-pbkdf2-sha256$"

// verifyPassword accepts both Rei Nation bcrypt hashes and the original TSFP
// server's PBKDF2-SHA256 hashes. Legacy hashes include their binary salt in the
// encoded database value so migrated accounts keep their existing passwords.
func verifyPassword(stored, password string) bool {
	if !strings.HasPrefix(stored, legacyPasswordPrefix) {
		return bcrypt.CompareHashAndPassword([]byte(stored), []byte(password)) == nil
	}
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != strings.TrimSuffix(legacyPasswordPrefix, "$") {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < 1 || iterations > 1_000_000 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(want) == 0 || len(want) > sha256.Size {
		return false
	}
	got := pbkdf2.Key([]byte(password), salt, iterations, len(want), sha256.New)
	return subtle.ConstantTimeCompare(got, want) == 1
}

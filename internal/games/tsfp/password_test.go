package tsfp

import (
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestVerifyPassword(t *testing.T) {
	legacy := "tsfp-pbkdf2-sha256$200000$ABEiM0RVZneImaq7zN3u/w$zIxkCkk8EKxKh6AIEb2p1UqdDFhzgSdke893n+kliMU"
	if !verifyPassword(legacy, "game-pass") || verifyPassword(legacy, "wrong") {
		t.Fatal("legacy PBKDF2 password verification failed")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("new-pass"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if !verifyPassword(string(hash), "new-pass") || verifyPassword(string(hash), "wrong") {
		t.Fatal("bcrypt password verification failed")
	}
	password72 := strings.Repeat("x", 72)
	hash, err = bcrypt.GenerateFromPassword([]byte(password72), bcrypt.MinCost)
	if err != nil || !verifyPassword(string(hash), password72) || verifyPassword(string(hash), password72+"x") {
		t.Fatal("bcrypt password length boundary was not enforced")
	}
	for _, malformed := range []string{"tsfp-pbkdf2-sha256$", "tsfp-pbkdf2-sha256$0$AA$AA", "not-a-hash"} {
		if verifyPassword(malformed, "anything") {
			t.Fatalf("accepted malformed hash %q", malformed)
		}
	}
}

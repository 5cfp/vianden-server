package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// Prefixes make secrets recognizable. If a token ever leaks into a log, a chat,
// or a Git commit, people and secret-scanning tools can tell what it is.
const (
	SessionTokenPrefix = "vs_" // Vianden session
	InviteCodePrefix   = "vi_" // Vianden invite
	SetupTokenPrefix   = "vo_" // Vianden owner setup (one-time, printed at startup)
)

// tokenBytes is the amount of randomness in every token: 256 bits.
// Guessing one is impossible in practice, which is why a fast hash (SHA-256)
// is enough to store it, with no salt (see PROJECT_PLAN.md decisions log).
const tokenBytes = 32

// NewToken creates a new random secret, e.g. "vs_3kQ9xZ...".
// It returns the token (give it to the user, never store it) and its hash (store only this).
func NewToken(prefix string) (token string, hash []byte) {
	b := make([]byte, tokenBytes)
	rand.Read(b) // crypto/rand never fails on supported systems (it would crash the program instead)

	// URL-safe base64 without padding: only letters, digits, '-' and '_'.
	token = prefix + base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token)
}

// HashToken returns the SHA-256 hash of a token, as stored in the database.
// To check a token sent by a client, hash it and look the hash up.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

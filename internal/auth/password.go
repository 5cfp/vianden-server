// Package auth contains the security building blocks for accounts:
// password hashing (Argon2id) and secret tokens (sessions, invites).
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id settings for NEW hashes (RFC 9106, "second recommended option").
// Each hash takes about 64 MiB of memory and a fraction of a second, which makes
// guessing passwords from a stolen database very expensive.
// Old hashes keep working if these change, because each hash stores its own settings.
const (
	argonTime    = 3         // passes over memory
	argonMemory  = 64 * 1024 // KiB = 64 MiB
	argonThreads = 4
	argonKeyLen  = 32 // bytes of output
	argonSaltLen = 16 // bytes of random salt, new for every hash
)

// maxConcurrentHashes caps how many Argon2id computations run at the same time.
// Each one uses 64 MiB on purpose; without a cap, many simultaneous logins (for example
// from a botnet with many IP addresses, which per-IP rate limits cannot stop) could use
// up all memory and crash the server. Extra requests simply wait for a free slot.
const maxConcurrentHashes = 4

// hashSlots is a "semaphore": a channel with room for 4 tokens. Taking a slot = sending
// into it (blocks when full); giving it back = receiving from it.
var hashSlots = make(chan struct{}, maxConcurrentHashes)

// argonKey runs Argon2id while holding one of the limited slots.
func argonKey(password, salt []byte, t, m uint32, threads uint8, keyLen uint32) []byte {
	hashSlots <- struct{}{}
	defer func() { <-hashSlots }()
	return argon2.IDKey(password, salt, t, m, threads, keyLen)
}

// ErrInvalidHash means a stored hash string is not in the expected format.
var ErrInvalidHash = errors.New("invalid password hash format")

var b64 = base64.RawStdEncoding

// HashPassword returns an Argon2id hash in the standard "PHC" string format:
//
//	$argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>
//
// The salt and settings are stored inside the string, so the database needs only one column.
func HashPassword(password string) string {
	salt := make([]byte, argonSaltLen)
	rand.Read(salt) // crypto/rand never fails on supported systems (it would crash the program instead)

	key := argonKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

// VerifyPassword reports whether password matches the stored hash.
// It returns an error only if the stored hash itself is broken.
func VerifyPassword(password, encodedHash string) (bool, error) {
	p, salt, want, err := decodeHash(encodedHash)
	if err != nil {
		return false, err
	}

	got := argonKey([]byte(password), salt, p.time, p.memory, p.threads, uint32(len(want))) // #nosec G115 -- decodeHash limits len(want) to 16-64

	// Constant-time comparison: takes the same time whether the first byte or the last byte
	// differs, so an attacker cannot learn anything by measuring response times.
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

func decodeHash(encoded string) (p argonParams, salt, key []byte, err error) {
	// "$argon2id$v=19$m=65536,t=3,p=4$salt$key" splits into
	// ["", "argon2id", "v=19", "m=65536,t=3,p=4", "salt", "key"].
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return p, nil, nil, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return p, nil, nil, ErrInvalidHash
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	// Refuse absurd settings (e.g. from a corrupted row) instead of trying to allocate huge memory.
	if p.memory == 0 || p.memory > 256*1024 || p.time == 0 || p.time > 16 || p.threads == 0 { // max 256 MiB
		return p, nil, nil, ErrInvalidHash
	}

	// Sane lengths only: a corrupted or malicious row must not make every login for that
	// user do a huge amount of work (key length decides how much Argon2 output is computed).
	if salt, err = b64.DecodeString(parts[4]); err != nil || len(salt) < 8 || len(salt) > 64 {
		return p, nil, nil, ErrInvalidHash
	}
	if key, err = b64.DecodeString(parts[5]); err != nil || len(key) < 16 || len(key) > 64 {
		return p, nil, nil, ErrInvalidHash
	}
	return p, salt, key, nil
}

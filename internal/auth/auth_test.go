package auth

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
)

func TestHashAndVerifyPassword(t *testing.T) {
	hash := HashPassword("correct horse battery staple")

	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Errorf("unexpected hash format: %s", hash)
	}

	ok, err := VerifyPassword("correct horse battery staple", hash)
	if err != nil || !ok {
		t.Errorf("correct password: ok=%v err=%v, want ok=true", ok, err)
	}

	for _, wrong := range []string{"", "correct horse battery stapl", "Correct horse battery staple", "correct horse battery staple "} {
		ok, err := VerifyPassword(wrong, hash)
		if err != nil || ok {
			t.Errorf("wrong password %q: ok=%v err=%v, want ok=false", wrong, ok, err)
		}
	}
}

func TestSamePasswordGetsDifferentHashes(t *testing.T) {
	// Thanks to the random salt, two users with the same password have different hashes.
	first, second := HashPassword("summer2026"), HashPassword("summer2026")
	if first == second {
		t.Error("two hashes of the same password are identical: salt is not random")
	}
}

func TestVerifyUsesSettingsStoredInHash(t *testing.T) {
	// A hash made with OTHER settings (like an older server version) must still verify.
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte("pw"), salt, 1, 8*1024, 1, 32)
	old := "$argon2id$v=19$m=8192,t=1,p=1$" + b64.EncodeToString(salt) + "$" + b64.EncodeToString(key)

	ok, err := VerifyPassword("pw", old)
	if err != nil || !ok {
		t.Errorf("ok=%v err=%v, want ok=true", ok, err)
	}
}

func TestVerifyRejectsBrokenHashes(t *testing.T) {
	broken := []string{
		"",
		"plaintext-password",
		"$argon2i$v=19$m=65536,t=3,p=4$c2FsdA$a2V5",        // wrong algorithm
		"$argon2id$v=16$m=65536,t=3,p=4$c2FsdA$a2V5",       // wrong version
		"$argon2id$v=19$m=abc,t=3,p=4$c2FsdA$a2V5",         // bad numbers
		"$argon2id$v=19$m=65536,t=3,p=4$!!!$a2V5",          // bad base64
		"$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$",           // empty key
		"$argon2id$v=19$m=99999999,t=3,p=4$c2FsdA$a2V5",    // absurd memory
		"$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$a2V5$extra", // too many parts
	}
	for _, h := range broken {
		ok, err := VerifyPassword("pw", h)
		if ok || !errors.Is(err, ErrInvalidHash) {
			t.Errorf("hash %q: ok=%v err=%v, want ok=false and ErrInvalidHash", h, ok, err)
		}
	}
}

func TestNewToken(t *testing.T) {
	token, hash := NewToken(SessionTokenPrefix)

	if !strings.HasPrefix(token, "vs_") {
		t.Errorf("token %q is missing its prefix", token)
	}
	// 32 bytes in base64 without padding = 43 characters, plus the 3-character prefix.
	if len(token) != 3+43 {
		t.Errorf("token length = %d, want 46", len(token))
	}
	if strings.ContainsAny(token[3:], "+/=") {
		t.Errorf("token %q is not URL-safe", token)
	}
	if !bytes.Equal(hash, HashToken(token)) {
		t.Error("returned hash does not match HashToken(token)")
	}
	if len(hash) != 32 {
		t.Errorf("hash length = %d, want 32 (SHA-256)", len(hash))
	}
}

func TestTokensAreUnique(t *testing.T) {
	seen := make(map[string]bool)
	for range 10000 {
		token, _ := NewToken(InviteCodePrefix)
		if seen[token] {
			t.Fatalf("duplicate token generated: %s", token)
		}
		seen[token] = true
	}
}

func TestHashTokenIsDeterministic(t *testing.T) {
	if !bytes.Equal(HashToken("vs_abc"), HashToken("vs_abc")) {
		t.Error("same token gave different hashes; database lookups would fail")
	}
	if bytes.Equal(HashToken("vs_abc"), HashToken("vs_abd")) {
		t.Error("different tokens gave the same hash")
	}
}

// BenchmarkHashPassword shows how long one hash takes: go test -bench . ./internal/auth
func BenchmarkHashPassword(b *testing.B) {
	for b.Loop() {
		HashPassword("benchmark password")
	}
}

func TestAtMostFourHashesRunAtOnce(t *testing.T) {
	// Fill all slots, then check that a fifth hash has to wait for a free one.
	for range maxConcurrentHashes {
		hashSlots <- struct{}{}
	}
	done := make(chan struct{})
	go func() {
		HashPassword("waits for a slot")
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("a hash ran although all slots were taken")
	case <-time.After(200 * time.Millisecond):
	}

	<-hashSlots // free one slot
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting hash did not run after a slot was freed")
	}
	for range maxConcurrentHashes - 1 {
		<-hashSlots
	}
}

func TestStoredHashLengthsAreBounded(t *testing.T) {
	salt16 := b64.EncodeToString(make([]byte, 16))
	for name, h := range map[string]string{
		"tiny salt": "$argon2id$v=19$m=65536,t=3,p=4$" + b64.EncodeToString(make([]byte, 4)) + "$" + b64.EncodeToString(make([]byte, 32)),
		"tiny key":  "$argon2id$v=19$m=65536,t=3,p=4$" + salt16 + "$" + b64.EncodeToString(make([]byte, 8)),
		"huge key":  "$argon2id$v=19$m=65536,t=3,p=4$" + salt16 + "$" + b64.EncodeToString(make([]byte, 100000)),
	} {
		if _, err := VerifyPassword("pw", h); !errors.Is(err, ErrInvalidHash) {
			t.Errorf("%s: err = %v, want ErrInvalidHash", name, err)
		}
	}
}

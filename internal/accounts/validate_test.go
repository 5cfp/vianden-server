package accounts

import (
	"strings"
	"testing"
)

func TestValidateUsername(t *testing.T) {
	valid := []string{"osama", "abc", "user_1", "a.b-c", "0day", strings.Repeat("a", 32)}
	for _, u := range valid {
		if err := validateUsername(u); err != nil {
			t.Errorf("%q should be valid: %v", u, err)
		}
	}

	invalid := []string{
		"", "ab", strings.Repeat("a", 33), // length
		"_osama", ".osama", "-osama", // must start with a letter or digit
		"Osama",                  // uppercase (callers normalize first)
		"os ama",                 // space
		"osаma",                  // Cyrillic "а" that looks like Latin "a"
		"osama!", "o@", "../etc", // other symbols
	}
	for _, u := range invalid {
		if err := validateUsername(u); err == nil {
			t.Errorf("%q should be invalid", u)
		}
	}
}

func TestNormalizeUsername(t *testing.T) {
	if got := NormalizeUsername("  Osama "); got != "osama" {
		t.Errorf("got %q, want osama", got)
	}
}

func TestValidateDisplayName(t *testing.T) {
	zwj := string(rune(0x200D)) // zero-width joiner, used inside emoji sequences
	valid := []string{"Osama", "أسامة", "Osama 🎮", "👨" + zwj + "👩" + zwj + "👧 family", strings.Repeat("x", 32)}
	for _, n := range valid {
		if err := validateDisplayName(n); err != nil {
			t.Errorf("%q should be valid: %v", n, err)
		}
	}

	invalid := []string{
		"", strings.Repeat("x", 33),
		"evil" + string(rune(0x202E)) + "gnp.exe", // right-to-left override: displays as "evilexe.png"
		"line\nbreak",                           // control character
		"zero" + string(rune(0x200B)) + "width", // zero-width space (invisible)
	}
	for _, n := range invalid {
		if err := validateDisplayName(n); err == nil {
			t.Errorf("%q should be invalid", n)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	valid := []string{"12345678", "correct horse battery staple", "pässwörd", "🔑🔑🔑🔑🔑🔑🔑🔑", strings.Repeat("a", 256)}
	for _, p := range valid {
		if err := validatePassword(p); err != nil {
			t.Errorf("%q should be valid: %v", p, err)
		}
	}

	invalid := []string{"", "1234567", "🔑🔑🔑", strings.Repeat("a", 257), "bad\xffutf8xx"}
	for _, p := range invalid {
		if err := validatePassword(p); err == nil {
			t.Errorf("%q should be invalid", p)
		}
	}
}

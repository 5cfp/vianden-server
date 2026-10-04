package accounts

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Length limits (also documented in docs/API.md).
const (
	MinPasswordLength    = 8
	MaxPasswordLength    = 256
	MaxDisplayNameLength = 32
)

// Usernames: 3-32 characters, lowercase letters, digits, '_', '.', '-', starting with a letter or digit.
// Restricting the alphabet prevents look-alike tricks (e.g. Cyrillic "о" instead of Latin "o").
var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{2,31}$`)

// ValidationError means one input field is invalid. Message is safe to show to the user.
type ValidationError struct {
	Field   string // "username", "display_name", or "password"
	Message string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }

// NormalizeUsername trims spaces and lowercases, so "  Osama " and "osama" are the same account.
func NormalizeUsername(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func validateUsername(username string) error {
	if !usernamePattern.MatchString(username) {
		return &ValidationError{"username",
			"must be 3-32 characters: letters, digits, '_', '.', '-', starting with a letter or digit"}
	}
	return nil
}

func validateDisplayName(name string) error {
	n := utf8.RuneCountInString(name)
	if n < 1 || n > MaxDisplayNameLength {
		return &ValidationError{"display_name", "must be 1-32 characters"}
	}
	if hasHiddenCharacters(name) {
		return &ValidationError{"display_name", "contains invisible or control characters"}
	}
	return nil
}

// hasHiddenCharacters reports control characters and invisible formatting characters. Some
// of them (like U+202E "right-to-left override") can make text display reversed, a classic
// spoofing trick. U+200D (zero-width joiner) is allowed because emoji need it.
func hasHiddenCharacters(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) || (unicode.Is(unicode.Cf, r) && r != 0x200D) {
			return true
		}
	}
	return false
}

// validatePassword follows NIST SP 800-63B: length matters, forced complexity rules do not.
// Any characters are allowed, including spaces and emoji.
func validatePassword(password string) error {
	if !utf8.ValidString(password) {
		return &ValidationError{"password", "is not valid text"}
	}
	n := utf8.RuneCountInString(password)
	if n < MinPasswordLength || n > MaxPasswordLength {
		return &ValidationError{"password", "must be 8-256 characters"}
	}
	return nil
}

// Package codec generates and validates short codes for links.
//
// The default alphabet is the URL-safe base62 (0-9, A-Z, a-z) which:
//   - is case-sensitive (gives 62^7 ≈ 3.5 trillion codes for length 7)
//   - contains no characters that require percent-encoding in a URL path
//   - is unambiguous when printed (no 0/O, 1/l/I confusion)
//
// The default code length is 7, which makes accidental collisions
// astronomically rare at any realistic scale.
package codec

import (
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
)

// Alphabet is the set of characters used in short codes.
// Re-declared as a var (not const) so tests can swap it if needed.
var Alphabet = []byte("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz")

// DefaultLength is the number of characters in a generated short code.
const DefaultLength = 7

// MaxAliasLength caps user-provided aliases to keep the URL sane.
const MaxAliasLength = 64

// ErrInvalidCode is returned when Validate fails.
var ErrInvalidCode = errors.New("codec: invalid short code")

// validCodeRe is a permissive check: only the alphabet, 1..MaxAliasLength.
var validCodeRe = regexp.MustCompile(`^[0-9A-Za-z]{1,64}$`)

// Generate returns a random short code of the given length.
//
// We read random bytes from crypto/rand (NOT math/rand) so codes are
// cryptographically unpredictable. We use rejection sampling to eliminate
// modulo bias, ensuring every character in the alphabet has an identical
// uniform probability of selection.
func Generate(length int) (string, error) {
	if length <= 0 {
		length = DefaultLength
	}
	alphabetLen := len(Alphabet)
	if alphabetLen == 0 {
		return "", errors.New("codec: alphabet is empty")
	}
	// Rejection threshold to eliminate modulo bias:
	// discard values >= maxValid so each alphabet character has an equal probability.
	maxValid := byte(256 - (256 % alphabetLen))

	out := make([]byte, length)
	buf := make([]byte, length+(length/4)+1)
	idx := 0

	for idx < length {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("codec: read random bytes: %w", err)
		}
		for _, b := range buf {
			if b < maxValid {
				out[idx] = Alphabet[int(b)%alphabetLen]
				idx++
				if idx == length {
					break
				}
			}
		}
	}
	return string(out), nil
}

// Validate returns nil if the given string is a syntactically valid code,
// or ErrInvalidCode wrapped with the offending value otherwise.
func Validate(code string) error {
	if !validCodeRe.MatchString(code) {
		return fmt.Errorf("%w: %q", ErrInvalidCode, code)
	}
	return nil
}

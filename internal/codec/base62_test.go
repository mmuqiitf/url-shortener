package codec

import (
	"errors"
	"testing"
)

func TestGenerate_Length(t *testing.T) {
	cases := []struct {
		name   string
		length int
	}{
		{"default_length", 0}, // 0 -> DefaultLength
		{"explicit_default", DefaultLength},
		{"small", 3},
		{"medium", 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, err := Generate(tc.length)
			if err != nil {
				t.Fatalf("Generate(%d) error: %v", tc.length, err)
			}
			want := tc.length
			if want <= 0 {
				want = DefaultLength
			}
			if len(code) != want {
				t.Errorf("got length %d, want %d (code=%q)", len(code), want, code)
			}
		})
	}
}

func TestGenerate_AlphabetOnly(t *testing.T) {
	t.Parallel()
	code, err := Generate(64)
	if err != nil {
		t.Fatalf("Generate error: %v", err)
	}
	for i, c := range code {
		ok := false
		for _, a := range Alphabet {
			if byte(c) == a {
				ok = true
				break
			}
		}
		if !ok {
			t.Fatalf("char at %d (%q) is not in alphabet", i, c)
		}
	}
}

func TestGenerate_Unique(t *testing.T) {
	t.Parallel()
	// generating 1000 codes of length 7 should produce no duplicates.
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		c, err := Generate(7)
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if _, dup := seen[c]; dup {
			t.Fatalf("duplicate code: %q", c)
		}
		seen[c] = struct{}{}
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		code    string
		wantErr bool
	}{
		{"simple_alpha", "abcDEF", false},
		{"numeric", "123456", false},
		{"mixed", "aB3xZ9", false},
		{"empty", "", true},
		{"with_dash", "abc-def", true},
		{"with_underscore", "abc_def", true},
		{"too_long", string(make([]byte, 65)), true},
		{"with_space", "abc def", true},
		{"unicode", "abcé", true},
		{"single_char", "x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := Validate(tc.code)
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate(%q) err = %v, wantErr = %v", tc.code, err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidCode) {
				t.Errorf("Validate(%q) err = %v, want wraps ErrInvalidCode", tc.code, err)
			}
		})
	}
}

package secretgen

import (
	"regexp"
	"testing"
)

// urlSafe matches only the base64.RawURLEncoding alphabet — no quotes,
// backslashes, padding, or whitespace.
var urlSafe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func TestPassword_AlphabetAndLength(t *testing.T) {
	pw, err := Password(DBPasswordBytes)
	if err != nil {
		t.Fatalf("Password: %v", err)
	}
	// 24 bytes → 32 url-safe chars (ceil(24*8/6) = 32, no padding).
	if got := len(pw); got != 32 {
		t.Errorf("len = %d, want 32", got)
	}
	if len(pw) < 24 {
		t.Errorf("password shorter than the 24-char floor: %q", pw)
	}
	if !urlSafe.MatchString(pw) {
		t.Errorf("password %q contains a non-url-safe char", pw)
	}
}

func TestDBPassword_UrlSafeAndStrong(t *testing.T) {
	pw, err := DBPassword()
	if err != nil {
		t.Fatalf("DBPassword: %v", err)
	}
	if len(pw) < 24 {
		t.Errorf("DBPassword len = %d, want >= 24", len(pw))
	}
	if !urlSafe.MatchString(pw) {
		t.Errorf("DBPassword %q is not url-safe", pw)
	}
	// Must never equal the shipped weak default.
	if pw == "vornik" {
		t.Errorf("DBPassword returned the weak default 'vornik'")
	}
}

func TestPassword_Uniqueness(t *testing.T) {
	seen := make(map[string]struct{}, 256)
	for i := 0; i < 256; i++ {
		pw, err := Password(DBPasswordBytes)
		if err != nil {
			t.Fatalf("Password: %v", err)
		}
		if _, dup := seen[pw]; dup {
			t.Fatalf("duplicate password generated: %q", pw)
		}
		seen[pw] = struct{}{}
	}
}

func TestPassword_RejectsNonPositive(t *testing.T) {
	for _, n := range []int{0, -1, -24} {
		if _, err := Password(n); err == nil {
			t.Errorf("Password(%d) = nil error, want error", n)
		}
	}
}

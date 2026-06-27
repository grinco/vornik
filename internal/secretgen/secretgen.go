// Package secretgen mints high-entropy, URL-safe secrets for
// install-time provisioning (e.g. the database password gen-config
// injects into the live config). The output alphabet is restricted to
// [A-Za-z0-9-_] (base64.RawURLEncoding), so the secret never contains a
// quote, backslash, or whitespace — it is safe to drop verbatim into a
// YAML scalar, a libpq DSN, or a single-quoted SQL literal without
// escaping. Keeping this out of internal/apikey avoids coupling a
// generic secret generator to the api-key wire format.
package secretgen

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// DBPasswordBytes is the number of raw random bytes behind a generated
// database password. 24 bytes → 32 url-safe characters under
// base64.RawURLEncoding (no padding), carrying ~192 bits of entropy —
// far beyond brute-force reach.
const DBPasswordBytes = 24

// Password returns a freshly-minted secret of nBytes of crypto/rand
// entropy, encoded with base64.RawURLEncoding so the result is limited
// to [A-Za-z0-9-_] with no padding. nBytes must be positive.
//
// The url-safe alphabet is deliberate: the secret carries no ', ", \,
// or whitespace, so it slots into a YAML value, a libpq key=value DSN,
// and a single-quoted SQL literal without any escaping surprises.
func Password(nBytes int) (string, error) {
	if nBytes <= 0 {
		return "", fmt.Errorf("secretgen: nBytes must be positive, got %d", nBytes)
	}
	raw := make([]byte, nBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("secretgen: read crypto/rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// DBPassword is the convenience wrapper used by gen-config: a
// DBPasswordBytes-byte (≈32-char) url-safe database password.
func DBPassword() (string, error) {
	return Password(DBPasswordBytes)
}

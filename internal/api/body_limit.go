package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
)

// defaultJSONBodyLimit caps small JSON request bodies on handlers that
// decode a struct directly. 64 KiB is generous for the config/control
// payloads these endpoints accept while bounding the memory a single
// authenticated request can force the daemon to buffer.
//
// Used via limitJSONBody on handlers that previously called
// json.NewDecoder(r.Body).Decode with no MaxBytesReader, letting an
// authenticated caller force the daemon to buffer an arbitrarily large
// JSON value (memory-pressure DoS) — bug sweep 2026-06-04.
const defaultJSONBodyLimit = 64 * 1024

func readLimitedBody(w http.ResponseWriter, r *http.Request, maxBytes int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, bodyTooLargeError{limit: maxBytes}
		}
		return nil, err
	}
	return body, nil
}

// limitJSONBody wraps r.Body in a MaxBytesReader capped at
// defaultJSONBodyLimit so a subsequent json.NewDecoder(r.Body).Decode
// can't be forced to buffer an unbounded payload. It preserves the
// caller's existing decode semantics (no DisallowUnknownFields, no
// multiple-value rejection) — it only bounds the size.
func limitJSONBody(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, defaultJSONBodyLimit)
}

type bodyTooLargeError struct {
	limit int64
}

func (e bodyTooLargeError) Error() string {
	return fmt.Sprintf("request body exceeds %d bytes", e.limit)
}

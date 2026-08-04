package mcpauth

import (
	"encoding/json"
	"io"
	"net/http"
)

// writeJSON / readJSON keep the test servers in this package readable.
func writeJSON(w io.Writer, v any) error {
	return json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 256*1024))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

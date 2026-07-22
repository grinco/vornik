package api

import (
	"context"
	"testing"
)

func TestCheckWebWritesInsecure(t *testing.T) {
	cases := map[string]string{
		"insecure": "WARNING",
		"on":       "OK",
		"off":      "OK",
		"":         "OK",
	}
	for mode, wantStatus := range cases {
		h := &DoctorHandlers{webWritesMode: mode}
		got := h.checkWebWritesInsecure(context.Background(), false)
		if got.Name != "web_writes_mode" {
			t.Errorf("mode %q: name = %q, want web_writes_mode", mode, got.Name)
		}
		if got.Status != wantStatus {
			t.Errorf("mode %q: status = %q, want %q", mode, got.Status, wantStatus)
		}
	}
}

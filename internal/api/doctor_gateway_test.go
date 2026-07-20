package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckGatewayHealthy_SkippedWhenUnconfigured(t *testing.T) {
	h := &DoctorHandlers{}
	c := h.checkGatewayHealthy(context.Background(), false)
	if c.Status != "SKIPPED" {
		t.Errorf("status = %q, want SKIPPED", c.Status)
	}
}

func TestCheckGatewayHealthy_OKWhenReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	h := &DoctorHandlers{}
	h.SetGatewayURL(srv.URL)
	c := h.checkGatewayHealthy(context.Background(), false)
	if c.Status != "OK" {
		t.Errorf("status = %q, want OK (msg=%q)", c.Status, c.Message)
	}
}

func TestCheckGatewayHealthy_ErrorWhenUnreachable(t *testing.T) {
	h := &DoctorHandlers{}
	h.SetGatewayURL("http://127.0.0.1:1") // nothing listening
	c := h.checkGatewayHealthy(context.Background(), false)
	if c.Status != "ERROR" {
		t.Errorf("status = %q, want ERROR", c.Status)
	}
}

// review B1: the doctor must never dial a non-loopback gateway address, even
// with its short-timeout client — a non-loopback URL is misconfiguration and
// probing it would be an SSRF the doctor itself performs. It must ERROR on the
// host string alone, WITHOUT making any HTTP request. We prove no dial occurred
// by asserting the returned message is the guard's message ("...must be
// loopback") and NOT the transport-error message ("gateway unreachable: ...")
// that the dial branch would produce.
func TestCheckGatewayHealthy_NonLoopbackErrorsWithoutDialing(t *testing.T) {
	for _, addr := range []string{"http://169.254.169.254:8010", "http://example.com:8010"} {
		h := &DoctorHandlers{}
		h.SetGatewayURL(addr)
		c := h.checkGatewayHealthy(context.Background(), false)
		if c.Status != "ERROR" {
			t.Errorf("addr %q: status = %q, want ERROR", addr, c.Status)
		}
		if !strings.Contains(c.Message, "loopback") {
			t.Errorf("addr %q: message = %q, want the loopback-guard message (proves no dial)", addr, c.Message)
		}
		if strings.Contains(c.Message, "unreachable") {
			t.Errorf("addr %q: message = %q, doctor dialed the host (transport error) instead of rejecting on the host string", addr, c.Message)
		}
	}
}

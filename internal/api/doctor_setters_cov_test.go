package api

// Coverage for the trivial DoctorHandlers wiring setters + the
// package-singleton Wire* helpers, plus the exported operator-scope
// wrappers in handlers.go. These are one-liners that production wires
// at boot but no unit test exercised, leaving them at 0%.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDoctorHandlers_Setters_NilReceiverSafe(t *testing.T) {
	var h *DoctorHandlers // nil receiver — every setter must no-op, not panic.
	assert.NotPanics(t, func() {
		h.SetAPIMetrics(nil)
		h.SetLeaderLockRepository(nil)
		h.SetServer(nil)
		h.SetConfigReloader(nil)
	})
}

func TestDoctorHandlers_Setters_AssignOnRealHandler(t *testing.T) {
	h := NewDoctorHandlers(nil)
	// Each setter stores into a private field; assert via the path the
	// production code reads (the field is only consumed deeper, so we
	// just confirm the call path runs without panic + the singleton
	// wiring threads through).
	assert.NotPanics(t, func() {
		h.SetLeaderLockRepository(nil)
		h.SetConfigReloader(nil)
		h.SetServer(&Server{})
	})
	assert.NotNil(t, h.server, "SetServer must store the back-reference")
}

func TestWireDoctorServer_And_Reloader_SingletonThreading(t *testing.T) {
	// Save + restore the package singleton so we don't disturb other
	// tests that rely on it.
	prev := doctorHandlers
	t.Cleanup(func() { doctorHandlers = prev })

	// Nil singleton → Wire* must be a safe no-op.
	doctorHandlers = nil
	assert.NotPanics(t, func() {
		WireDoctorServer(&Server{})
		WireDoctorReloader(nil)
	})

	// Real singleton → Wire* threads the back-reference through.
	h := NewDoctorHandlers(nil)
	doctorHandlers = h
	WireDoctorServer(&Server{})
	WireDoctorReloader(nil)
	assert.NotNil(t, h.server, "WireDoctorServer must set the server on the singleton")
}

func TestRequestOperatorWrappers_DelegateToUnexported(t *testing.T) {
	// Auth-on (context flag true) + no principal → empty identity, scope
	// check denies even a named operator.
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, true))
	assert.Equal(t, "", RequestOperatorID(req))
	assert.False(t, RequestAllowsOperator(req, "someone"))
	assert.False(t, RequestAllowsOperator(req, ""), "empty operatorID denies under auth-on")

	// Auth-off with X-Operator-Id header → identity flows through; the
	// scope check short-circuits to allow (single-operator dev mode).
	req2 := authDisabledReq(httptest.NewRequest(http.MethodGet, "/x", nil))
	req2.Header.Set("X-Operator-Id", "op-7")
	assert.Equal(t, "op-7", RequestOperatorID(req2))
	assert.True(t, RequestAllowsOperator(req2, "op-7"))
	assert.True(t, RequestAllowsOperator(req2, "anyone"), "auth-off allows any operator scope")
}

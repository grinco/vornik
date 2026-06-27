package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestResolveRealIP_ParsesNewBlock(t *testing.T) {
	const y = `
server:
  real_ip:
    enabled: true
    trusted_proxies: ["10.0.0.5/32"]
    header: CF-Connecting-IP
`
	var c Config
	if err := yaml.Unmarshal([]byte(y), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	r := c.ResolveRealIP()
	if !r.Enabled {
		t.Fatal("want enabled")
	}
	if len(r.TrustedProxies) != 1 || r.TrustedProxies[0] != "10.0.0.5/32" {
		t.Fatalf("trusted_proxies parse: got %v", r.TrustedProxies)
	}
	if r.Header != "CF-Connecting-IP" {
		t.Fatalf("header: got %q", r.Header)
	}
	if r.DeprecatedFallback {
		t.Fatal("must not flag deprecated fallback when new block is set")
	}
	if !c.RealIPConfigured() {
		t.Fatal("RealIPConfigured: want true")
	}
}

func TestResolveRealIP_DeprecatedFallback(t *testing.T) {
	const y = `
api:
  rate_limit:
    per_ip:
      trusted_proxies: ["10.0.0.9/32"]
`
	var c Config
	if err := yaml.Unmarshal([]byte(y), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	r := c.ResolveRealIP()
	if !r.DeprecatedFallback {
		t.Fatal("want DeprecatedFallback when only the deprecated key is set")
	}
	if !r.Enabled {
		t.Fatal("deprecated fallback must enable resolution to preserve behaviour")
	}
	if len(r.TrustedProxies) != 1 || r.TrustedProxies[0] != "10.0.0.9/32" {
		t.Fatalf("fallback trust list: got %v", r.TrustedProxies)
	}
}

func TestResolveRealIP_NewBlockBeatsDeprecated(t *testing.T) {
	const y = `
server:
  real_ip:
    enabled: true
    trusted_proxies: ["10.0.0.5/32"]
api:
  rate_limit:
    per_ip:
      trusted_proxies: ["10.0.0.9/32"]
`
	var c Config
	if err := yaml.Unmarshal([]byte(y), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	r := c.ResolveRealIP()
	if r.DeprecatedFallback {
		t.Fatal("new block must win without flagging deprecation")
	}
	if r.TrustedProxies[0] != "10.0.0.5/32" {
		t.Fatalf("new block must win: got %v", r.TrustedProxies)
	}
}

func TestRealIPConfigured_FalseWhenUnset(t *testing.T) {
	var c Config
	if c.RealIPConfigured() {
		t.Fatal("RealIPConfigured: want false for empty config")
	}
}

func TestResolveRealIP_BadCIDRSurfacedByRealipConstructor(t *testing.T) {
	// Config-layer resolve is purely string-shuffling; the actual CIDR
	// validation lives in realip.NewConfig and is exercised there. This
	// test just asserts the resolve helper passes raw entries through
	// untouched so a bad entry reaches the constructor's loader.
	const y = `
server:
  real_ip:
    enabled: true
    trusted_proxies: ["not-a-cidr"]
`
	var c Config
	if err := yaml.Unmarshal([]byte(y), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	r := c.ResolveRealIP()
	if len(r.TrustedProxies) != 1 || !strings.Contains(r.TrustedProxies[0], "not-a-cidr") {
		t.Fatalf("resolve must pass raw entries through: got %v", r.TrustedProxies)
	}
}

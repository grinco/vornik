package cli

import (
	"strings"
	"testing"
)

func TestRenderFeatureTable(t *testing.T) {
	features := []featureStatusDTO{
		{
			ID:      "instinct",
			Title:   "Continuous-Learning Instinct Layer",
			Summary: "Learns confidence-scored patterns from telemetry.",
			Status:  "ready",
			GatesOn: false,
		},
		{
			ID:      "auth",
			Title:   "API authentication",
			Summary: "Per-project API keys + admin-key gating.",
			Status:  "blocked",
			GatesOn: false,
		},
		{
			ID:      "memory-rag",
			Title:   "Memory consolidation + RAG caches",
			Summary: "LLM consolidation and embedding caches.",
			Status:  "ok",
			GatesOn: true,
		},
	}

	out := renderFeatureTable(features)

	// Must contain all three feature titles.
	for _, want := range []string{"Continuous-Learning Instinct Layer", "API authentication", "Memory consolidation + RAG caches"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing title %q", want)
		}
	}
	// Must contain status values.
	for _, want := range []string{"ready", "blocked", "ok"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing status %q", want)
		}
	}
	// Must contain a header.
	if !strings.Contains(out, "STATUS") {
		t.Error("table missing STATUS header")
	}
	if !strings.Contains(out, "FEATURE") {
		t.Error("table missing FEATURE header")
	}
}

func TestRenderFeatureTable_Empty(t *testing.T) {
	out := renderFeatureTable(nil)
	if !strings.Contains(out, "no features") {
		t.Errorf("expected 'no features' placeholder, got: %q", out)
	}
}

func TestRenderFeatureDetail(t *testing.T) {
	unmet := prereqResultDTO{
		Name:        "admin key present",
		OK:          false,
		Fixable:     false,
		Detail:      "admin-key.txt missing",
		Remediation: "create admin-key.txt before enabling auth",
	}
	met := prereqResultDTO{
		Name: "something fine",
		OK:   true,
	}
	f := featureStatusDTO{
		ID:      "auth",
		Title:   "API authentication",
		Summary: "Per-project API keys.",
		Status:  "blocked",
		Prereqs: []prereqResultDTO{unmet, met},
	}

	out := renderFeatureDetail(f)

	if !strings.Contains(out, "API authentication") {
		t.Error("detail missing title")
	}
	if !strings.Contains(out, "blocked") {
		t.Error("detail missing status")
	}
	// Unmet prereq remediation must appear.
	if !strings.Contains(out, "create admin-key.txt") {
		t.Error("detail missing remediation for unmet prereq")
	}
	// Met prereq should show OK.
	if !strings.Contains(out, "something fine") {
		t.Error("detail missing met prereq name")
	}
}

func TestRenderFeatureDetail_WithVerify(t *testing.T) {
	verifyOK := prereqResultDTO{
		Name:   "verify",
		OK:     true,
		Detail: "all systems go",
	}
	f := featureStatusDTO{
		ID:     "auth",
		Title:  "API authentication",
		Status: "ok",
		Verify: &verifyOK,
	}

	out := renderFeatureDetail(f)

	if !strings.Contains(out, "all systems go") {
		t.Error("detail missing verify detail")
	}
}

func TestFeatureStatusIcon(t *testing.T) {
	cases := []struct {
		status string
		want   string
	}{
		{"ok", "OK "},
		{"ready", "RDY"},
		{"blocked", "BLK"},
		{"degraded", "DGR"},
		{"disabled", "OFF"},
		{"unknown", "?  "},
		{"", "?  "},
	}
	for _, tc := range cases {
		if got := featureStatusIcon(tc.status); got != tc.want {
			t.Errorf("featureStatusIcon(%q) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

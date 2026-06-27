// Package instinctmodel contains the pure data types shared between the CE
// memetic layer and the EE instinct engine: Trigger, TriggerKey, and
// MarshalTrigger.
//
// Motivation: internal/instinct is EE IP (it will be relocated to
// internal/enterprise in the CE/EE Phase 1c domain move). internal/memetic
// is CE and must not import internal/instinct. The three symbols below are
// model-only (hashing + JSON serialisation of a plain struct) and carry zero
// EE IP, so they live here for CE use. internal/instinct type-aliases them so
// all existing internal callers continue to compile without change.
package instinctmodel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// Trigger is the structured "situation" half of an instinct: the conditions
// under which the learned action/observation held. It maps to the trigger_json
// column. All fields are optional — an extractor fills only the ones that
// define its pattern.
type Trigger struct {
	Role       string `json:"role,omitempty"`
	ErrorClass string `json:"error_class,omitempty"`
	TaskType   string `json:"task_type,omitempty"`
	Model      string `json:"model,omitempty"`
	// RepoScope is the memory repo partition a retrieval-domain instinct keys
	// on (the scope queried when the retrieved chunks preceded an ok/failure
	// step). Empty for non-retrieval triggers.
	RepoScope string `json:"repo_scope,omitempty"`
	// Signal disambiguates hypotheses within a domain that share the same
	// role. Used by the budget domain to distinguish over_provisioned from
	// under_provisioned. Empty for all other domains.
	Signal string `json:"signal,omitempty"`
	StepID string `json:"step_id,omitempty"`
}

// empty reports whether the trigger carries no distinguishing fields.
func (t Trigger) empty() bool {
	return t.Role == "" && t.ErrorClass == "" && t.TaskType == "" &&
		t.StepID == "" && t.Model == "" && t.RepoScope == "" && t.Signal == ""
}

// TriggerKey returns the canonical dedup key for (domain, trigger). It is
// deterministic and stable: the same situation always hashes to the same key,
// so a recurring pattern updates one instinct row rather than spawning
// duplicates.
//
// The key incorporates the domain so the same trigger fields under different
// domains (e.g. a recovery vs. a quality instinct for the same role+error)
// never collide on the dedup index.
//
// Construction: a sorted list of non-empty "field=value" pairs, joined with
// ';', prefixed with "domain|", then SHA-256 hex. Fields are emitted in a
// fixed canonical order, so the key is independent of how the Trigger struct
// was populated.
func TriggerKey(domain string, t Trigger) string {
	pairs := make([]string, 0, 6)
	add := func(k, v string) {
		v = strings.TrimSpace(v)
		if v != "" {
			pairs = append(pairs, k+"="+v)
		}
	}
	// Fixed canonical order (alphabetically sorted by key name).
	add("error_class", t.ErrorClass)
	add("model", t.Model)
	add("repo_scope", t.RepoScope)
	add("role", t.Role)
	add("signal", t.Signal)
	add("step_id", t.StepID)
	add("task_type", t.TaskType)

	canonical := domain + "|" + strings.Join(pairs, ";")
	sum := sha256.Sum256([]byte(canonical))
	return "tk_" + hex.EncodeToString(sum[:])
}

// MarshalTrigger serialises a Trigger to the JSON bytes stored in the
// trigger_json column. Returns nil for an empty trigger so the column stays
// NULL/empty rather than carrying a no-op "{}".
func MarshalTrigger(t Trigger) (json.RawMessage, error) {
	if t.empty() {
		return nil, nil
	}
	b, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

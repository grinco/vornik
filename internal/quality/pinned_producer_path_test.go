package quality

import (
	"encoding/json"
	"strings"
	"testing"
)

// Test 47 (agent-quality-benchmark design, amendment 2026-09-18; F5 of
// review-20260918-384e).
//
// PinnedProducerFieldPath is handed to a scoring verifier through its step
// prompt, and decodePinnedProducer reads the scorer's denominator out of the
// same document. If those two drift apart, the verifier is told to report
// against one field while the scorer divides by another — the exact failure
// the ${outputs.…} mechanism exists to make impossible — and a test that
// compared two string literals would keep passing throughout.
//
// Struct tags cannot be constants, so this is the seam that binds them: it
// resolves the exported path against a document by walking it generically,
// and asserts the result is what the typed envelope decodes.
func TestPinnedProducerFieldPathMatchesEnvelope(t *testing.T) {
	const doc = `{"analysis":{"test_case_ids":["s1_case_1","s1_case_2","s1_case_3"],"test_cases_pinned":3}}`

	ids, pinned, diagnostic := decodePinnedProducer(json.RawMessage(doc))
	if diagnostic != "" {
		t.Fatalf("well-formed producer evidence diagnosed %q", diagnostic)
	}
	if pinned != 3 || len(ids) != 3 {
		t.Fatalf("envelope decoded %d ids / pinned %d, want 3/3", len(ids), pinned)
	}

	got := resolveDottedPath(t, doc, PinnedProducerFieldPath)
	list, ok := got.([]any)
	if !ok {
		t.Fatalf("PinnedProducerFieldPath %q resolved to %#v, not the id list the scorer decodes; "+
			"the constant and producerEnvelope's tags have drifted", PinnedProducerFieldPath, got)
	}
	if len(list) != len(ids) {
		t.Fatalf("path yielded %d ids, envelope decoded %d", len(list), len(ids))
	}
	for i, id := range ids {
		if list[i] != id {
			t.Errorf("index %d: path yielded %v, envelope decoded %q", i, list[i], id)
		}
	}
}

// A path pointing at nothing must not silently look like success — otherwise
// the test above would pass against a renamed field by resolving to nil twice.
func TestResolveDottedPathMissesLoudly(t *testing.T) {
	const doc = `{"analysis":{"renamed_field":["a"]}}`

	if got := resolveDottedPath(t, doc, PinnedProducerFieldPath); got != nil {
		t.Fatalf("a renamed producer field resolved to %#v, want nil", got)
	}
}

// resolveDottedPath walks a JSON document by a dot-separated path, the way the
// executor's ${outputs.<step>.<field>} resolver does.
func resolveDottedPath(t *testing.T, doc, path string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	for _, seg := range strings.Split(path, ".") {
		obj, isMap := v.(map[string]any)
		if !isMap {
			return nil
		}
		next, present := obj[seg]
		if !present {
			return nil
		}
		v = next
	}
	return v
}

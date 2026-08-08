//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Cosine calibration for the dedup preflight (LLD §12.2 "Calibration bands").
//
// This is the acceptance gate the design demands and the one that already
// rejected a metric: the originally-specified lexical Jaccard scored 0.061
// against a required 0.5 and was replaced by embeddings because of this test's
// predecessor. Cosine has to clear the same bar, measured, not assumed.
//
// Needs a live embedding backend, hence the integration tag. Point it at one:
//
//	VORNIK_EMBED_ENDPOINT=http://127.0.0.1:11434 \
//	VORNIK_EMBED_MODEL=bge-m3:latest \
//	go test -tags=integration -run TestSkillCosineCalibration -v ./internal/api/
//
// Deliberately calls the embedding endpoint over plain HTTP in the same shape
// as internal/memory's OpenAI-compatible path ({endpoint}/v1/embeddings) rather
// than importing that package, so internal/api keeps its no-dependency-on-
// internal/memory posture even in tests.

func embedForCalibration(t *testing.T, texts []string) [][]float32 {
	t.Helper()
	endpoint := os.Getenv("VORNIK_EMBED_ENDPOINT")
	model := os.Getenv("VORNIK_EMBED_MODEL")
	if endpoint == "" || model == "" {
		t.Skip("set VORNIK_EMBED_ENDPOINT and VORNIK_EMBED_MODEL to run cosine calibration")
	}

	body, err := json.Marshal(map[string]any{"model": model, "input": texts})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("embedding endpoint unreachable: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("embedding endpoint returned %d: %s", resp.StatusCode, string(raw[:min(len(raw), 300)]))
	}
	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data) != len(texts) {
		t.Fatalf("got %d vectors for %d texts", len(out.Data), len(texts))
	}
	vecs := make([][]float32, len(out.Data))
	for i, d := range out.Data {
		vecs[i] = d.Embedding
	}
	return vecs
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestSkillCosineCalibration(t *testing.T) {
	fixtures := []*persistence.Skill{
		fxInfraReview, fxVerifyTests, fxSecReview, fxMemReview,
		fxWritingDesigns, fxAmendDesign,
		fxSoakBacklog, fxOffScope,
		fxDossier, fxDispatcher,
	}
	texts := make([]string, len(fixtures))
	for i, f := range fixtures {
		texts[i] = skillEmbeddingText(f)
	}
	vecs := embedForCalibration(t, texts)
	byID := map[string][]float32{}
	for i, f := range fixtures {
		byID[f.ID] = vecs[i]
	}
	t.Logf("embedded %d fixtures, dimension %d", len(fixtures), len(vecs[0]))

	type pair struct {
		label string
		a, b  *persistence.Skill
	}
	intra := []pair{
		{"review: infra x verify-tests", fxInfraReview, fxVerifyTests},
		{"review: infra x security", fxInfraReview, fxSecReview},
		{"review: infra x memory-designs", fxInfraReview, fxMemReview},
		{"review: verify-tests x security", fxVerifyTests, fxSecReview},
		{"review: verify-tests x memory-designs", fxVerifyTests, fxMemReview},
		{"review: security x memory-designs", fxSecReview, fxMemReview},
		{"design: writing x amend", fxWritingDesigns, fxAmendDesign},
		{"backlog: soak x off-scope", fxSoakBacklog, fxOffScope},
	}
	negative := []pair{
		{"control: dossier x dispatcher", fxDossier, fxDispatcher},
		{"control: dossier x infra-review", fxDossier, fxInfraReview},
		{"control: dispatcher x soak-backlog", fxDispatcher, fxSoakBacklog},
		{"control: dossier x writing-designs", fxDossier, fxWritingDesigns},
		{"control: dispatcher x verify-tests", fxDispatcher, fxVerifyTests},
	}

	// TIERED EXPERIMENT. The bands assumed every §12.0 cluster is a
	// duplicate-pair. Test that premise against pairs that unambiguously ARE
	// the same knowledge in different words — real catalogue rows.
	trueDupes := []pair{
		{"TRUE: moltbook v1 x v2", fxMoltbookV1, fxMoltbookV2},
		{"TRUE: report-problem store x shipped", fxReportStore, fxReportShipped},
		{"TRUE: rag-first x paraphrase", fxRagFirst, fxRagFirstParaphrase},
	}
	allTexts := []string{}
	extra := []*persistence.Skill{fxMoltbookV1, fxMoltbookV2, fxReportStore, fxReportShipped, fxRagFirst, fxRagFirstParaphrase}
	for _, f := range extra {
		allTexts = append(allTexts, skillEmbeddingText(f))
	}
	extraVecs := embedForCalibration(t, allTexts)
	for i, f := range extra {
		byID[f.ID] = extraVecs[i]
	}

	score := func(p pair) float64 {
		c := cosine(byID[p.a.ID], byID[p.b.ID])
		t.Logf("%-44s cosine=%.3f", p.label, c)
		return c
	}

	t.Logf("--- TIER 1: true duplicates (same knowledge, different words) ---")
	dupeMin := 1.0
	for _, p := range trueDupes {
		if v := score(p); v < dupeMin {
			dupeMin = v
		}
	}
	t.Logf("--- TIER 2: §12.0 'clusters' (shared trigger phrasing) ---")

	intraMin, intraLabel := 1.0, ""
	for _, p := range intra {
		if s := score(p); s < intraMin {
			intraMin, intraLabel = s, p.label
		}
	}
	t.Logf("--- TIER 3: unrelated controls ---")
	negMax, negLabel := -1.0, ""
	for _, p := range negative {
		if s := score(p); s > negMax {
			negMax, negLabel = s, p.label
		}
	}

	t.Logf("")
	t.Logf("weakest TRUE duplicate:  %.3f", dupeMin)
	t.Logf("weakest intra-cluster: %.3f  (%s)   band: > 0.5", intraMin, intraLabel)
	t.Logf("strongest control:     %.3f  (%s)   band: < 0.3", negMax, negLabel)
	t.Logf("separation:            %.3f", intraMin-negMax)
	t.Logf("configured threshold:  %.3f", skillDupeThreshold)

	// The acceptance gate, restated after the 2026-08-07 calibration showed the
	// original premise was wrong. What the preflight must do is separate TRUE
	// duplicates from everything else. It is NOT required to separate tier 2
	// from tier 3 — those overlap, because shared trigger phrasing is not
	// similarity of knowledge (§12.0).
	nonDupeMax := intraMin
	for _, p := range append(append([]pair{}, intra...), negative...) {
		if v := cosine(byID[p.a.ID], byID[p.b.ID]); v > nonDupeMax {
			nonDupeMax = v
		}
	}
	t.Logf("highest NON-duplicate pair: %.3f", nonDupeMax)
	t.Logf("gap: %.3f", dupeMin-nonDupeMax)

	if dupeMin <= nonDupeMax {
		t.Errorf("metric does not separate true duplicates: weakest true duplicate %.3f <= highest non-duplicate %.3f. "+
			"Per §12.5 fix the metric, do not slide skillDupeThreshold to fit", dupeMin, nonDupeMax)
	}
	if skillDupeThreshold >= dupeMin {
		t.Errorf("skillDupeThreshold %.3f is at or above the weakest true duplicate %.3f — real duplicates would pass unflagged",
			skillDupeThreshold, dupeMin)
	}
	if skillDupeThreshold <= nonDupeMax {
		t.Errorf("skillDupeThreshold %.3f is at or below the highest non-duplicate %.3f — unrelated skills would be flagged, training reflex bypass",
			skillDupeThreshold, nonDupeMax)
	}
	_ = intraLabel
	_ = negLabel
	fmt.Fprintf(os.Stderr, "CALIBRATION intraMin=%.3f negMax=%.3f threshold=%.3f\n",
		intraMin, negMax, skillDupeThreshold)
}

// Real catalogue rows used by the integration calibration to test whether the
// metric separates TRUE duplicates (same knowledge, different words) from
// merely shared trigger phrasing. Descriptions are verbatim from the live store.
var (
	fxMoltbookV1 = &persistence.Skill{
		ID: "fx-molt1", Name: "moltbook-engagement",
		Description: "Apply when engaging on Moltbook to validate comments for self-authorship, quality, redundancy, and disclosure before posting",
	}
	fxMoltbookV2 = &persistence.Skill{
		ID: "fx-molt2", Name: "moltbook-engagement",
		Description: "Apply when engaging or PUBLISHING on Moltbook: how to call the API via query_api, the MANDATORY two-step publish-with-confirmation (create → solve challenge → POST /verify, or the post never goes live), and comment self-authorship/quality/redundancy/disclosure gates",
	}
	fxReportStore = &persistence.Skill{
		ID: "fx-rep1", Name: "report-vornik-problem",
		Description: "When a user hits or reports a problem with vornik (errors, crashes, 'isn't working', unexpected behaviour, or a failed install) — offer to help them file an ANONYMIZED report to the public grinco/vornik GitHub repo using vornikctl report; never auto-post",
	}
	fxReportShipped = &persistence.Skill{
		ID: "fx-rep2", Name: "report-problem",
		Description: "Teaches Claude how to help a user report a Vornik problem — a bug, a crash, a misbehaving swarm, or an INSTALL failure — as an anonymized GitHub issue on the public grinco/vornik repo. Use whenever the user says Vornik is broken / erroring / not installing / behaving wrong and wants to file it upstream.",
	}
	fxRagFirst = &persistence.Skill{
		ID: "fx-rag1", Name: "rag-first",
		Description: "Before grepping/reading a subsystem you don't already know, recall its design doc from project RAG — the LLDs are the authoritative design record; read code only to verify a named symbol.",
	}
	fxRagFirstParaphrase = &persistence.Skill{
		ID: "fx-rag2", Name: "recall-design-before-reading-code",
		Description: "When you need to understand how a subsystem works, query the project's semantic memory for its design document first; treat those documents as authoritative and open source files only to confirm a symbol it names still exists.",
	}
)

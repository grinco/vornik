package membench

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Comparability key (design §5.6).
//
// Model choice per role is configuration with no default, per operator decision.
// The risk that creates — runs that are not comparable to each other — is
// handled mechanically here rather than by discipline: two runs whose keys
// differ cannot be diffed, and the error names the field that differs.

// ComparabilityFields is the complete set of inputs that change what a run's
// numbers mean. Enumerated explicitly rather than derived, so adding a knob
// without deciding whether it affects comparability is a visible omission.
type ComparabilityFields struct {
	HarnessVersion string `json:"harness_version"`
	DatasetName    string `json:"dataset_name"`
	DatasetSHA256  string `json:"dataset_sha256"`
	ItemSelection  string `json:"item_selection"`

	// OurExtractionModel is the model our memory subsystem uses to extract and
	// consolidate at ingest — it changes what we STORE, not just how we answer.
	OurExtractionModel string `json:"our_extraction_model"`
	// TheirExtractionModel is the same for the comparison system. Empty on a
	// single-system run.
	TheirExtractionModel string `json:"their_extraction_model,omitempty"`

	AnswerModel string `json:"answer_model"`
	JudgeModel  string `json:"judge_model"`

	RecallParams string `json:"recall_params"`
	// ObservedRecallMethod is the retrieval path the SYSTEM reported taking, not
	// one an operator declared — the same observed-over-declared rule as
	// ObservedEmbedder, and for the same reason. Empty when the system cannot
	// report it, which marks the key partial: a run that cannot say whether it
	// reranked is not comparable to one that can.
	ObservedRecallMethod string `json:"observed_recall_method,omitempty"`
	// RetrievalPathUnverified marks a tier-2 run that did NOT establish its
	// retrieval path was deterministic — permitted only via
	// --accept-unverified-path, and required for any external system, which cannot
	// report our retrieval method. In the key because a proven-deterministic run
	// and an assumed one are different experiments.
	RetrievalPathUnverified bool `json:"retrieval_path_unverified,omitempty"`
	// CorpusSHA256 identifies the external haystack a shared-corpus run ingested.
	// Empty when the dataset carries its own, which dataset_sha256 covers.
	CorpusSHA256 string `json:"corpus_sha256,omitempty"`
	// ObservedEmbedder is the embedder the SYSTEM reported, not one an operator
	// declared. Empty when the system cannot report it, which marks the key
	// partial — two runs on different embedding models would otherwise match,
	// which is exactly how a titan-versus-cohere comparison once came out clean.
	ObservedEmbedder   string `json:"observed_embedder,omitempty"`
	AnswerPromptSHA256 string `json:"answer_prompt_sha256"`
	JudgePromptSHA256  string `json:"judge_prompt_sha256"`

	// ExternalConfigSHA256 is the comparison system's own effective
	// configuration as it reports it — model versions, retrieval parameters.
	//
	// Round-2 review found this missing: we captured our models but not theirs,
	// so the external system could swap its embedding model between runs and the
	// key would still match, producing two runs that look comparable and are
	// not. Empty means we could not read it, which makes the key PARTIAL rather
	// than matching.
	ExternalConfigSHA256 string `json:"external_config_sha256,omitempty"`

	// Tier2Only marks a run that scored RETRIEVAL ONLY — no answer generated, no
	// verdict sought. It belongs in the key for the same reason RecallMethod does:
	// it is a different experiment, not a cheaper version of the same one, and its
	// accuracy is undefined rather than low.
	//
	// Without it a tier-2-only run and a judged run over the same dataset and
	// models would produce a byte-identical key and compare clean — exactly how
	// the 2026-08-11 pre-fix and post-fix reranker runs did.
	Tier2Only bool `json:"tier2_only,omitempty"`

	// SingleSystem marks a run with no external system, so the absence of
	// external fields is by design rather than a verification gap.
	SingleSystem bool `json:"single_system,omitempty"`
}

// fieldPairs returns the (name, value) list in a fixed order. One source of
// truth for both hashing and diffing, so the two can never disagree about which
// fields matter.
func (f ComparabilityFields) fieldPairs() [][2]string {
	return [][2]string{
		{"harness_version", f.HarnessVersion},
		{"dataset_name", f.DatasetName},
		{"dataset_sha256", f.DatasetSHA256},
		{"item_selection", f.ItemSelection},
		{"our_extraction_model", f.OurExtractionModel},
		{"their_extraction_model", f.TheirExtractionModel},
		{"answer_model", f.AnswerModel},
		{"judge_model", f.JudgeModel},
		{"recall_params", f.RecallParams},
		{"observed_recall_method", f.ObservedRecallMethod},
		{"retrieval_path_unverified", fmt.Sprintf("%t", f.RetrievalPathUnverified)},
		{"corpus_sha256", f.CorpusSHA256},
		{"observed_embedder", f.ObservedEmbedder},
		{"answer_prompt_sha256", f.AnswerPromptSHA256},
		{"judge_prompt_sha256", f.JudgePromptSHA256},
		{"external_config_sha256", f.ExternalConfigSHA256},
		{"tier2_only", strconv.FormatBool(f.Tier2Only)},
		{"single_system", strconv.FormatBool(f.SingleSystem)},
	}
}

// Key is the SHA-256 over every field, name-tagged and delimited.
//
// The delimiter is not decoration: concatenating values alone would let
// ("ab","c") and ("a","bc") hash alike, which is entirely plausible across model
// names and selection strings. Including the field NAME in the digest means a
// future reordering of fieldPairs cannot silently change a key either.
func (f ComparabilityFields) Key() string {
	return ComparabilityKeyOf(f.fieldPairs())
}

// ComparabilityKeyOf hashes an ordered (name, value) list into a run key.
//
// Extracted from ComparabilityFields.Key so a SECOND benchmark family can key
// its runs with this exact implementation instead of a lookalike
// (2026-08-13-agent-quality-benchmark-design.md §6.1 shares the guard and the
// comparability key, and nothing else, because those are the two places where
// two implementations means one wrong answer). The byte layout is unchanged, so
// every already-published memory key still verifies.
func ComparabilityKeyOf(pairs [][2]string) string {
	h := sha256.New()
	for _, kv := range pairs {
		// The \x00 cannot appear in any of these values, so no value can forge a
		// field boundary. hash.Hash.Write never returns an error, so the writes
		// are unchecked by contract rather than by omission.
		_, _ = h.Write([]byte(kv[0]))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(kv[1]))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// DiffComparabilityPairs names every differing field between two ordered pair
// lists, or nil when they match.
//
// Listing all of them rather than the first matters in practice: an operator who
// fixes one difference and re-runs only to hit the next has been sent round a
// loop the tool could have short-circuited.
func DiffComparabilityPairs(a, b [][2]string) []string {
	var diffs []string
	shared := len(a)
	if len(b) < shared {
		shared = len(b)
	}
	for i := 0; i < shared; i++ {
		if a[i][0] != b[i][0] {
			diffs = append(diffs, fmt.Sprintf("field[%d] name (%q vs %q)", i, a[i][0], b[i][0]))
			continue
		}
		if a[i][1] != b[i][1] {
			diffs = append(diffs, fmt.Sprintf("%s (%q vs %q)", a[i][0], a[i][1], b[i][1]))
		}
	}
	for i := shared; i < len(a); i++ {
		diffs = append(diffs, fmt.Sprintf("%s (%q vs missing)", a[i][0], a[i][1]))
	}
	for i := shared; i < len(b); i++ {
		diffs = append(diffs, fmt.Sprintf("%s (missing vs %q)", b[i][0], b[i][1]))
	}
	return diffs
}

// Partial reports whether the key covers everything it should. A partial key
// means the run's comparability is unverified, which is NOT the same as
// verified-identical and must be surfaced as such on the manifest.
func (f ComparabilityFields) Partial() bool {
	// An unreported embedder leaves the key unverified even for a single system:
	// two runs on different embedding models produce the same key, which is how a
	// titan-versus-cohere comparison once matched clean. Admitted rather than
	// assumed unchanged, on the same principle as an unreadable external config.
	if f.ObservedEmbedder == "" || f.ObservedRecallMethod == "" {
		return true
	}
	if f.SingleSystem {
		return false
	}
	return f.ExternalConfigSHA256 == ""
}

// CheckComparable reports whether two runs may be diffed, naming every field
// that differs.
//
// Listing all of them rather than the first matters in practice: an operator who
// fixes one difference and re-runs only to hit the next has been sent round a
// loop the tool could have short-circuited.
func CheckComparable(a, b ComparabilityFields) error {
	if a.Key() == b.Key() {
		return nil
	}
	diffs := DiffComparabilityPairs(a.fieldPairs(), b.fieldPairs())
	if len(diffs) == 0 {
		// Keys differ but no enumerated field does: fieldPairs has drifted out of
		// sync with Key. Report it rather than claiming the runs are comparable.
		return fmt.Errorf("comparability keys differ but no enumerated field does — " +
			"fieldPairs() is out of sync with Key()")
	}
	return fmt.Errorf("runs are not comparable; differing: %s", strings.Join(diffs, ", "))
}

// CorpusDigest identifies the haystack a shared-corpus run actually ingested.
//
// The gold set's sha256 was the only input the key recorded, which left the corpus
// itself an uncontrolled variable: the native dataset reads its haystack from a
// directory on disk, so editing that directory produced runs that shared a
// byte-identical key while retrieving from different documents. That is the same
// hole the missing rerank field left, and it was found the same way — by tripping
// over it, editing the design-doc tree that IS the haystack while benchmarking
// against it and watching the chunk count move mid-run.
//
// Digesting what was INGESTED rather than listing the directory is deliberate: it
// covers the content the run actually saw, so a file the loader skipped or a
// transformation applied on the way in cannot hide from the key.
//
// Order-independent, because the ingest order of a directory walk is not a property
// of the corpus and must not split otherwise identical runs. Id and content are
// length-prefixed rather than concatenated, so a rename cannot compensate for an
// edit.
//
// Empty for a dataset that carries its own haystack — there is no external corpus,
// and dataset_sha256 already covers it. An empty string says that; a hash of
// nothing would look like a real corpus.
func CorpusDigest(items []Item) string {
	if len(items) == 0 {
		return ""
	}
	perDoc := make([]string, 0, len(items))
	for _, it := range items {
		eventTime := ""
		if !it.EventTime.IsZero() {
			eventTime = it.EventTime.UTC().Format(time.RFC3339Nano)
		}
		payload := fmt.Sprintf("%d:%s%d:%s%d:%s%d:%s",
			len(it.DocumentID), it.DocumentID,
			len(it.Context), it.Context,
			len(it.Content), it.Content,
			len(eventTime), eventTime)
		h := sha256.Sum256([]byte(payload))
		perDoc = append(perDoc, hex.EncodeToString(h[:]))
	}
	sort.Strings(perDoc)
	roll := sha256.New()
	for _, d := range perDoc {
		_, _ = roll.Write([]byte(d))
	}
	return hex.EncodeToString(roll.Sum(nil))
}

package membench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Dataset loaders (design §5.4).
//
// Three datasets, three roles: the two public sets give head-to-head
// comparability, and the native set over our own corpus is the regression gate,
// because conversational personal-fact recall is not the job our memory
// subsystem actually has.
//
// Per-dataset scores are never averaged together. They measure different things,
// and a blended number would hide exactly the signal the gate exists to provide.

// ---------------------------------------------------------------------------
// LongMemEval
// ---------------------------------------------------------------------------

// LongMemEval loads the LongMemEval dataset: one question per item, each with
// its own haystack of dated chat sessions.
type LongMemEval struct{}

// Name is the stable identifier recorded in manifests and the comparability key.
func (LongMemEval) Name() string { return "longmemeval" }

type lmeItem struct {
	QuestionID   string `json:"question_id"`
	QuestionType string `json:"question_type"`
	Question     string `json:"question"`
	// Answer is json.Number-tolerant: 32 of 500 items in longmemeval-cleaned
	// answer with a bare number (counting questions), and a plain string field
	// makes encoding/json reject the entire file.
	Answer     lmeAnswer          `json:"answer"`
	SessionIDs []string           `json:"haystack_session_ids"`
	Dates      []string           `json:"haystack_dates"`
	Sessions   [][]map[string]any `json:"haystack_sessions"`
}

// lmeAnswer decodes LongMemEval's `answer`, which is a string for most items and
// a bare JSON number for the counting questions.
//
// It keeps the DIGITS rather than going through float64: `any` would decode 3 as
// float64(3), and a formatter change turning that into "3e+00" would have the judge
// mark every counting question wrong for a reason invisible in the results.
type lmeAnswer string

func (a *lmeAnswer) UnmarshalJSON(b []byte) error {
	// A JSON string: decode normally so escapes are handled.
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*a = lmeAnswer(s)
		return nil
	}
	// A number (or anything else scalar): keep the literal text verbatim.
	*a = lmeAnswer(strings.TrimSpace(string(b)))
	return nil
}

// Load reads the dataset and applies limits.
func (l LongMemEval) Load(path string, lim Limits) ([]BenchItem, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var items []lmeItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	out := make([]BenchItem, 0, len(items))
	for _, in := range items {
		bi, err := in.toBenchItem()
		if err != nil {
			return nil, err
		}
		out = append(out, bi)
	}
	return applyLimits(out, lim), nil
}

// toBenchItem converts one raw dataset entry into a BenchItem.
func (in lmeItem) toBenchItem() (BenchItem, error) {
	bi := BenchItem{ID: in.QuestionID, Category: in.QuestionType}

	// The three parallel arrays are the dataset's shape; a length mismatch is a
	// corrupt file, so clip to the shortest rather than index out of range.
	n := len(in.Sessions)
	if len(in.Dates) < n {
		n = len(in.Dates)
	}
	if len(in.SessionIDs) < n {
		n = len(in.SessionIDs)
	}

	var gold []string
	for i := 0; i < n; i++ {
		// Document ids MUST be question-scoped: session ids repeat across items,
		// so a bare id would collide and cross-attribute gold documents.
		docID := in.QuestionID + "_" + in.SessionIDs[i]
		eventTime := parseLMEDate(in.Dates[i])

		turns, hasAnswer := stripAnswerLabels(in.Sessions[i])
		body, err := json.Marshal(turns)
		if err != nil {
			return bi, fmt.Errorf("encode %s session %s: %w", in.QuestionID, docID, err)
		}

		bi.Haystack = append(bi.Haystack, Item{
			DocumentID: docID,
			Content:    string(body),
			// The date is stated in-band too, because a system with no event-time
			// concept of its own could not otherwise answer a dated question at
			// all. Both systems receive identical framing.
			Context: fmt.Sprintf(
				"Session %s — you are the assistant in this conversation — happened on %s UTC.",
				docID, eventTime.Format("2006-01-02 15:04:05")),
			EventTime: eventTime,
		})
		if hasAnswer {
			// The gold DOCUMENT is the session a has_answer turn belongs to.
			gold = append(gold, docID)
		}
	}

	bi.QAs = []QA{{
		Question:        in.Question,
		GoldAnswer:      string(in.Answer),
		GoldDocumentIDs: gold,
	}}
	return bi, nil
}

// stripAnswerLabels removes the has_answer key from every turn and reports
// whether any turn carried it.
//
// has_answer is the LABEL. Leaving it in the ingested text would tell the system
// under test which session holds the answer, which is straightforwardly cheating.
func stripAnswerLabels(session []map[string]any) (turns []map[string]any, hasAnswer bool) {
	turns = make([]map[string]any, 0, len(session))
	for _, turn := range session {
		if v, ok := turn["has_answer"].(bool); ok && v {
			hasAnswer = true
		}
		clean := make(map[string]any, len(turn))
		for k, v := range turn {
			if k == "has_answer" {
				continue
			}
			clean[k] = v
		}
		turns = append(turns, clean)
	}
	return turns, hasAnswer
}

// lmeDateRE matches the dataset's "2023/05/14 (Sun) 09:30" form. The weekday is
// decorative and deliberately not validated — a mismatched one is not a reason to
// drop a session.
var lmeDateRE = regexp.MustCompile(`(\d{4})/(\d{2})/(\d{2})(?:\s*\([A-Za-z]+\))?(?:\s+(\d{1,2}):(\d{2}))?`)

// parseLMEDate extracts a UTC timestamp. An unparseable date yields the zero
// time, which the write path stores as NULL — "unknown", not "now". Substituting
// the load time would be worse than useless: it would make every session look
// contemporaneous and quietly break the temporal categories.
func parseLMEDate(s string) time.Time {
	m := lmeDateRE.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}
	}
	year := atoiSafe(m[1])
	month := atoiSafe(m[2])
	day := atoiSafe(m[3])
	hour, minute := atoiSafe(m[4]), atoiSafe(m[5])
	if year == 0 || month == 0 || day == 0 {
		return time.Time{}
	}
	return time.Date(year, time.Month(month), day, hour, minute, 0, 0, time.UTC)
}

// ---------------------------------------------------------------------------
// LoCoMo
// ---------------------------------------------------------------------------

// LoCoMo loads the LoCoMo multi-session dialogue dataset. Sessions arrive as
// numbered sibling keys rather than an array, so the loader discovers them.
type LoCoMo struct{}

// Name is the stable identifier recorded in manifests.
func (LoCoMo) Name() string { return "locomo" }

// Load reads the dataset and applies limits.
func (l LoCoMo) Load(path string, lim Limits) ([]BenchItem, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var convs []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &convs); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	out := make([]BenchItem, 0, len(convs))
	for _, conv := range convs {
		var sampleID string
		if v, ok := conv["sample_id"]; ok {
			_ = json.Unmarshal(v, &sampleID)
		}
		bi := BenchItem{ID: sampleID, Category: "locomo"}
		// dialogueOwner maps a dia_id to the session document containing it, so a
		// QA's evidence ids resolve to gold DOCUMENTS rather than turn ids.
		bi.Haystack, bi.QAs = locomoSessions(conv, sampleID)
		out = append(out, bi)
	}
	return applyLimits(out, lim), nil
}

// locomoSessions builds one conversation's haystack and its QAs.
func locomoSessions(conv map[string]json.RawMessage, sampleID string) ([]Item, []QA) {
	var haystack []Item
	dialogueOwner := map[string]string{}

	for _, key := range sortedSessionKeys(conv) {
		var turns []struct {
			Speaker string `json:"speaker"`
			DiaID   string `json:"dia_id"`
			Text    string `json:"text"`
		}
		if err := json.Unmarshal(conv[key], &turns); err != nil {
			continue
		}
		var when time.Time
		if v, ok := conv[key+"_date_time"]; ok {
			var str string
			_ = json.Unmarshal(v, &str)
			when = parseLoCoMoDate(str)
		}

		docID := sampleID + "_" + key
		var b strings.Builder
		for _, tr := range turns {
			fmt.Fprintf(&b, "%s: %s\n", tr.Speaker, tr.Text)
			if tr.DiaID != "" {
				dialogueOwner[tr.DiaID] = docID
			}
		}
		haystack = append(haystack, Item{
			DocumentID: docID,
			Content:    b.String(),
			Context: fmt.Sprintf("Conversation %s, %s — happened on %s UTC.",
				sampleID, key, when.Format("2006-01-02 15:04:05")),
			EventTime: when,
		})
	}
	return haystack, locomoQAs(conv, dialogueOwner)
}

// locomoQAs resolves each QA's evidence turn ids to the documents holding them.
func locomoQAs(conv map[string]json.RawMessage, dialogueOwner map[string]string) []QA {
	var raw []struct {
		Question string   `json:"question"`
		Answer   any      `json:"answer"`
		Evidence []string `json:"evidence"`
		Category int      `json:"category"`
	}
	if v, ok := conv["qa"]; ok {
		_ = json.Unmarshal(v, &raw)
	}
	out := make([]QA, 0, len(raw))
	for _, qa := range raw {
		gold := make([]string, 0, len(qa.Evidence))
		seen := map[string]bool{}
		for _, ev := range qa.Evidence {
			if doc, ok := dialogueOwner[ev]; ok && !seen[doc] {
				gold = append(gold, doc)
				seen[doc] = true
			}
		}
		out = append(out, QA{
			Question:        qa.Question,
			GoldAnswer:      fmt.Sprintf("%v", qa.Answer),
			GoldDocumentIDs: gold,
		})
	}
	return out
}

// sessionKeyRE matches "session_1" but not "session_1_date_time", so the date
// siblings are not mistaken for transcripts.
var sessionKeyRE = regexp.MustCompile(`^session_(\d+)$`)

// sortedSessionKeys returns session keys in numeric order. Map iteration is
// random in Go, and session order is chronological meaning — shuffling it would
// make a multi-session dataset incoherent.
func sortedSessionKeys(conv map[string]json.RawMessage) []string {
	var keys []string
	for k := range conv {
		if sessionKeyRE.MatchString(k) {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		return sessionNumber(keys[i]) < sessionNumber(keys[j])
	})
	return keys
}

func sessionNumber(key string) int {
	m := sessionKeyRE.FindStringSubmatch(key)
	if m == nil {
		return 0
	}
	return atoiSafe(m[1])
}

// locomoDateRE matches "1:56 pm on 8 May, 2023".
var locomoDateRE = regexp.MustCompile(`(?i)(\d{1,2}):(\d{2})\s*(am|pm)\s+on\s+(\d{1,2})\s+([A-Za-z]+),?\s+(\d{4})`)

func parseLoCoMoDate(s string) time.Time {
	m := locomoDateRE.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}
	}
	hour := atoiSafe(m[1])
	minute := atoiSafe(m[2])
	if strings.EqualFold(m[3], "pm") && hour < 12 {
		hour += 12
	}
	if strings.EqualFold(m[3], "am") && hour == 12 {
		hour = 0
	}
	day := atoiSafe(m[4])
	month, ok := monthByName(m[5])
	if !ok {
		return time.Time{}
	}
	return time.Date(atoiSafe(m[6]), month, day, hour, minute, 0, 0, time.UTC)
}

// ---------------------------------------------------------------------------
// Native
// ---------------------------------------------------------------------------

// Native loads the Vornik-native dataset: our own document corpus as the
// haystack, plus a hand-authored, version-controlled gold file.
//
// This is the set the CI gate scores, because it measures the retrieval we
// actually depend on rather than conversational personal-fact recall.
type Native struct {
	// CorpusDir holds the documents that form the shared haystack.
	CorpusDir string
}

// Name is the stable identifier recorded in manifests.
func (Native) Name() string { return "native" }

type nativeGoldset struct {
	Version   int `json:"version"`
	Questions []struct {
		ID            string   `json:"id"`
		Category      string   `json:"category"`
		Question      string   `json:"question"`
		GoldAnswer    string   `json:"gold_answer"`
		GoldDocuments []string `json:"gold_documents"`
		Rubric        string   `json:"rubric,omitempty"`
	} `json:"questions"`
}

// Load reads the gold set and pairs every question with the corpus haystack.
//
// Unlike the public datasets, every native question shares ONE haystack: the
// discrimination task is finding the right document among the whole corpus,
// which is the task an agent actually faces here.
func (n Native) Load(path string, lim Limits) ([]BenchItem, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read gold set %s: %w", path, err)
	}
	var gs nativeGoldset
	if err := json.Unmarshal(raw, &gs); err != nil {
		return nil, fmt.Errorf("parse gold set %s: %w", path, err)
	}

	haystack, err := n.loadCorpus()
	if err != nil {
		return nil, err
	}

	out := make([]BenchItem, 0, len(gs.Questions))
	for _, q := range gs.Questions {
		out = append(out, BenchItem{
			ID:       q.ID,
			Category: q.Category,
			Haystack: haystack,
			QAs: []QA{{
				Question:        q.Question,
				GoldAnswer:      q.GoldAnswer,
				GoldDocumentIDs: q.GoldDocuments,
				Rubric:          q.Rubric,
			}},
		})
	}
	return applyLimits(out, lim), nil
}

// loadCorpus reads every markdown file in CorpusDir as one haystack document,
// keyed on its base name so a gold entry can name a file without a path.
func (n Native) loadCorpus() ([]Item, error) {
	if n.CorpusDir == "" {
		return nil, fmt.Errorf("native dataset: CorpusDir is required")
	}
	entries, err := os.ReadDir(n.CorpusDir)
	if err != nil {
		return nil, fmt.Errorf("read corpus %s: %w", n.CorpusDir, err)
	}
	var out []Item
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(n.CorpusDir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read corpus file %s: %w", e.Name(), err)
		}
		out = append(out, Item{
			DocumentID: e.Name(),
			Content:    string(body),
			Context:    fmt.Sprintf("Document %s from the project's design corpus.", e.Name()),
			// No event time: these are living documents whose content does not
			// pertain to a single moment. Leaving it zero is honest; stamping the
			// file mtime would assert an event time we do not have.
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DocumentID < out[j].DocumentID })
	return out, nil
}

// ---------------------------------------------------------------------------
// shared
// ---------------------------------------------------------------------------

// applyLimits filters by category and caps counts. Order is preserved so a
// capped run is a prefix of the full one, which keeps two runs at different
// caps comparable on the items they share.
func applyLimits(items []BenchItem, lim Limits) []BenchItem {
	out := make([]BenchItem, 0, len(items))
	perCat := map[string]int{}
	for _, it := range items {
		if lim.Category != "" && it.Category != lim.Category {
			continue
		}
		if lim.MaxItemsPerCategory > 0 && perCat[it.Category] >= lim.MaxItemsPerCategory {
			continue
		}
		if lim.MaxItems > 0 && len(out) >= lim.MaxItems {
			break
		}
		perCat[it.Category]++
		out = append(out, it)
	}
	return out
}

func atoiSafe(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

var monthNamesByPrefix = map[string]time.Month{
	"jan": time.January, "feb": time.February, "mar": time.March,
	"apr": time.April, "may": time.May, "jun": time.June,
	"jul": time.July, "aug": time.August, "sep": time.September,
	"oct": time.October, "nov": time.November, "dec": time.December,
}

func monthByName(s string) (time.Month, bool) {
	if len(s) < 3 {
		return 0, false
	}
	m, ok := monthNamesByPrefix[strings.ToLower(s[:3])]
	return m, ok
}

// SharedHaystack marks the native dataset as sharing one haystack across every
// question (see SharedHaystackDataset). Its questions are all asked against the
// same document corpus, so ingesting per item would re-upload the whole corpus
// once per question.
func (Native) SharedHaystack() bool { return true }

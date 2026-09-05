package membench

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// Journal and resume (design §5.8).
//
// Append-only JSONL, one line per item per phase. A killed run costs only the
// in-flight item, which is what makes an abort on ErrQuotaExhausted cheap enough
// to be the correct response rather than a catastrophe.

// Phase is how far one item got.
type Phase string

// The phases an item passes through. Only PhaseJudged is terminal: an item
// counts as complete when, and only when, it has a verdict. The intermediate
// phases exist so a resumed run can report where an interrupted item stopped
// rather than only that it was unfinished.
const (
	PhasePrepared Phase = "prepared"
	PhaseIngested Phase = "ingested"
	PhaseRecalled Phase = "recalled"
	PhaseAnswered Phase = "answered"
	PhaseJudged   Phase = "judged"
)

// JournalEntry is one line.
type JournalEntry struct {
	ItemID   string  `json:"item_id"`
	Phase    Phase   `json:"phase"`
	Category string  `json:"category,omitempty"`
	Outcome  Outcome `json:"outcome,omitempty"`
	// Detail carries a failure reason for error/invalid outcomes so a resumed
	// run can report WHY without re-running the failure.
	Detail string `json:"detail,omitempty"`

	// HaystackLoss is the fraction of this item's haystack the system refused at
	// ingest, journaled so a RESUMED run can carry it forward.
	//
	// AssessTrust gates Trustworthy on two signals, and until this field existed
	// only one of them survived --resume: counts are seeded from the journal so a
	// resumed run "reports over the whole population", but worstLoss restarted at
	// 0.0 and every already-Completed item was skipped. So a resumed run could be
	// stamped trustworthy on exactly the evidence that would have failed it had
	// it run in one pass — the harness reporting a WEAKER gate on the same work,
	// with nothing saying so.
	//
	// omitempty is deliberate and safe: the zero value is "no loss", which is
	// what a journal line written before this field existed also means for the
	// max, so old journals resume with their trust unchanged rather than refused.
	HaystackLoss float64 `json:"haystack_loss,omitempty"`
}

// Journal appends entries to a run's journal file.
type Journal struct {
	f *os.File
	w *bufio.Writer
}

// OpenJournal opens a journal for appending, creating it if absent.
//
// Append rather than truncate, deliberately: truncating on reopen would discard
// exactly the work that resume exists to preserve.
func OpenJournal(path string) (*Journal, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open journal %s: %w", path, err)
	}
	return &Journal{f: f, w: bufio.NewWriter(f)}, nil
}

// Record appends one entry and flushes it.
//
// Flushed per entry on purpose. A buffered journal loses its tail on the very
// event it is meant to survive — a kill, a crash, an OOM — so trading a little
// throughput for durability is the whole point of having it.
func (j *Journal) Record(e JournalEntry) error {
	if j == nil {
		return nil
	}
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal journal entry: %w", err)
	}
	if _, err := j.w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write journal entry: %w", err)
	}
	return j.w.Flush()
}

// Close flushes and closes.
func (j *Journal) Close() error {
	if j == nil {
		return nil
	}
	if err := j.w.Flush(); err != nil {
		_ = j.f.Close()
		return err
	}
	return j.f.Close()
}

// Replay is a loaded journal: what already finished, and with what verdicts.
type Replay struct {
	completed map[string]bool
	byCat     map[string]OutcomeCounts
	worstLoss float64
}

// Completed reports whether an item reached a verdict and can be skipped.
func (r Replay) Completed(itemID string) bool {
	if r.completed == nil {
		return false
	}
	return r.completed[itemID]
}

// CountsByCategory returns the verdicts recovered from the journal, so a resumed
// run reports over the whole population rather than only what it re-ran.
func (r Replay) CountsByCategory() map[string]OutcomeCounts {
	if r.byCat == nil {
		return map[string]OutcomeCounts{}
	}
	return r.byCat
}

// WorstHaystackLoss is the largest per-item haystack loss recovered from the
// journal, so a resumed run carries the trust evidence its earlier pass
// recorded instead of restarting the measurement at zero.
func (r Replay) WorstHaystackLoss() float64 { return r.worstLoss }

// LoadJournal reads a journal. A missing file is an empty replay, not an error,
// so --resume is safe to pass on a first run.
//
// A truncated trailing line is TOLERATED: a killed process leaves a half-written
// final entry, and refusing to load would make a crash unrecoverable — defeating
// the journal's purpose. Only the trailing line is forgiven; a malformed line in
// the middle indicates real corruption and is an error.
func LoadJournal(path string) (Replay, error) {
	r := Replay{completed: map[string]bool{}, byCat: map[string]OutcomeCounts{}}

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return r, nil
		}
		return r, fmt.Errorf("open journal %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	// Journal lines are small, but a pathological Detail could exceed the
	// default 64 KiB token; raise the ceiling rather than fail a resume on it.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return r, fmt.Errorf("read journal %s: %w", path, err)
	}

	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e JournalEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			if i == len(lines)-1 {
				// Truncated tail from an interrupted write. Stop here; everything
				// before it is intact.
				break
			}
			return r, fmt.Errorf("journal %s line %d is corrupt: %w", path, i+1, err)
		}
		if e.Phase != PhaseJudged {
			continue
		}
		r.completed[e.ItemID] = true
		c := r.byCat[e.Category]
		c.Add(e.Outcome)
		r.byCat[e.Category] = c
		if e.HaystackLoss > r.worstLoss {
			r.worstLoss = e.HaystackLoss
		}
	}
	return r, nil
}

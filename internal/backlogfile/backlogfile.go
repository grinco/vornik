// Package backlogfile owns every read-modify-write of a project's
// BACKLOG.md. It is the single place that understands the marker
// grammar ([ ] pending, [?] proposed, [x] done, [!] failed) and
// serialises concurrent writers per project, so callers (the
// deposit HTTP endpoint, the autonomy manager's backlog tick) never
// race each other or the operator hand-editing the file.
package backlogfile

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
)

// FormatHeader is written at the top of a freshly created
// BACKLOG.md so the marker grammar is self-documenting for an
// operator opening the file in an editor.
const FormatHeader = "<!-- vornik backlog v1 — markers: [ ] pending | [?] proposed | [x] done | [!] failed -->"

// itemRE matches any of the four marker states. It is a superset of
// the autonomy package's backlogPendingRE (which matches only the
// pending `[ ]` box) — that regex MUST remain compatible with this
// grammar; see internal/autonomy/manager.go's backlogPendingRE.
//
// Capture groups: 1) leading indent + bullet + spaces before `[`,
// 2) the single marker character, 3) the text after the box.
var itemRE = regexp.MustCompile(`^(\s*[-*]\s+)\[([ ?x!])\]\s+(.*)$`)

// Item is one parsed checklist line from BACKLOG.md.
type Item struct {
	Marker byte   // ' ', '?', 'x', '!'
	Text   string // text after the marker box
	Line   int    // 0-based line index in the file
}

// Store serialises all BACKLOG.md mutations per project (round-2
// F2). Each project ID gets its own *sync.Mutex, obtained lazily via
// sync.Map.LoadOrStore, so unrelated projects never contend on the
// same lock.
type Store struct {
	mus sync.Map // projectID (string) -> *sync.Mutex
}

// NewStore returns an empty Store ready for use.
func NewStore() *Store {
	return &Store{}
}

func (s *Store) lockFor(projectID string) *sync.Mutex {
	v, _ := s.mus.LoadOrStore(projectID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// Append adds a new proposed item ("- [?] <itemText>") to the end of
// path, creating the file with FormatHeader if it doesn't exist yet.
// itemText must not contain a newline — flattening multi-line
// proposals into a single line is the caller's responsibility.
func (s *Store) Append(path, projectID, itemText string) error {
	if strings.Contains(itemText, "\n") {
		return fmt.Errorf("backlogfile: item text must not contain a newline: %q", itemText)
	}

	mu := s.lockFor(projectID)
	mu.Lock()
	defer mu.Unlock()

	content, existed, err := readFile(path)
	if err != nil {
		return err
	}
	if !existed {
		content = FormatHeader + "\n\n"
	}

	content = appendLine(content, "- [?] "+itemText)

	return os.WriteFile(path, []byte(content), 0o600)
}

// Items parses every marker line in path. It returns nil, nil if the
// file does not exist — an absent BACKLOG.md is an empty backlog,
// not an error.
func (s *Store) Items(path, projectID string) ([]Item, error) {
	mu := s.lockFor(projectID)
	mu.Lock()
	defer mu.Unlock()

	content, existed, err := readFile(path)
	if err != nil {
		return nil, err
	}
	if !existed {
		return nil, nil
	}
	return parseItems(content), nil
}

// PeekNext returns the text of the first consumable ("- [ ]")
// item, without modifying the file. ok is false when no such item
// exists (including when the file is absent).
func (s *Store) PeekNext(path, projectID string) (text string, ok bool, err error) {
	mu := s.lockFor(projectID)
	mu.Lock()
	defer mu.Unlock()

	content, existed, err := readFile(path)
	if err != nil {
		return "", false, err
	}
	if !existed {
		return "", false, nil
	}
	for _, it := range parseItems(content) {
		if it.Marker == ' ' {
			return it.Text, true, nil
		}
	}
	return "", false, nil
}

// MarkConsumed finds the first line whose marker is pending (' ')
// and whose text equals text exactly, rewrites its marker to 'x',
// and appends " (task: <taskID>)" to the text. It returns
// (false, nil) when no line matches — e.g. the operator already
// hand-edited the line, or it was consumed by a concurrent caller.
func (s *Store) MarkConsumed(path, projectID, text, taskID string) (bool, error) {
	mu := s.lockFor(projectID)
	mu.Lock()
	defer mu.Unlock()

	content, existed, err := readFile(path)
	if err != nil {
		return false, err
	}
	if !existed {
		return false, nil
	}

	lines := strings.Split(content, "\n")
	for i, line := range lines {
		m := itemRE.FindStringSubmatch(line)
		if m == nil || m[2] != " " || m[3] != text {
			continue
		}
		lines[i] = m[1] + "[x] " + text + fmt.Sprintf(" (task: %s)", taskID)
		return true, os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600)
	}
	return false, nil
}

// MarkFailed finds the line whose marker is 'x' and whose text ends
// with "(task: <taskID>)", rewrites its marker to '!', and appends
// ", failed" inside that annotation (producing
// "(task: <taskID>, failed)"). It returns (false, nil) when no line
// matches — including on a second call for the same taskID, since
// the line's marker is then '!' and no longer matches the 'x' search.
func (s *Store) MarkFailed(path, projectID, taskID string) (bool, error) {
	mu := s.lockFor(projectID)
	mu.Lock()
	defer mu.Unlock()

	content, existed, err := readFile(path)
	if err != nil {
		return false, err
	}
	if !existed {
		return false, nil
	}

	suffix := fmt.Sprintf("(task: %s)", taskID)
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		m := itemRE.FindStringSubmatch(line)
		if m == nil || m[2] != "x" || !strings.HasSuffix(m[3], suffix) {
			continue
		}
		newText := strings.TrimSuffix(m[3], ")") + ", failed)"
		lines[i] = m[1] + "[!] " + newText
		return true, os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600)
	}
	return false, nil
}

// readFile reads path, reporting existed=false (with a nil error)
// when the file is absent so callers can distinguish "empty
// backlog" from a real I/O failure.
func readFile(path string) (content string, existed bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return string(data), true, nil
}

// appendLine appends line to content, ensuring exactly one newline
// separates it from prior content and that the result ends with a
// trailing newline.
func appendLine(content, line string) string {
	if content == "" {
		return line + "\n"
	}
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return content + line + "\n"
}

// parseItems splits content into lines and collects every line
// matching itemRE, in file order.
func parseItems(content string) []Item {
	var items []Item
	for i, line := range strings.Split(content, "\n") {
		m := itemRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		items = append(items, Item{
			Marker: m[2][0],
			Text:   m[3],
			Line:   i,
		})
	}
	return items
}

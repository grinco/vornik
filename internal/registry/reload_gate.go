package registry

import (
	"fmt"
	"strings"
)

// ReloadRejectedError reports a reload refused because the staged tree could
// not be loaded in full. The previously active configuration is untouched and
// still serving.
//
// It carries the per-file diagnosis §11 produces, not just a count: an operator
// who is told "reload refused" and nothing else has to go and find out which
// file and why, which is the gap §11 closed and this must not reopen.
type ReloadRejectedError struct {
	Rejected []RejectedFile
}

func (e *ReloadRejectedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "config reload REFUSED: %d file(s) in the tree could not be loaded, "+
		"so activating it would drop what they define. The running configuration is unchanged "+
		"and still serving.\n", len(e.Rejected))
	for _, r := range e.Rejected {
		fmt.Fprintf(&b, "\n  %s %s:\n%s", r.Kind, r.Path, indentLines(r.Error, "    "))
	}
	b.WriteString("\nFix the file(s) above and reload again. If a key is unknown to this " +
		"binary rather than misspelt, the config is ahead of the binary: deploy the binary first, " +
		"then the config (loader-validator agreement design §10).")
	return b.String()
}

func indentLines(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n") + "\n"
}

// stagedRejections returns the files the staged load refused, or nil.
func (r *Registry) stagedRejections() []RejectedFile {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.staged == nil || r.staged.index == nil {
		return nil
	}
	return r.staged.index.Rejected
}

// discardStaged drops a staged snapshot that must not be activated, so a later
// ActivateStaged cannot promote the very tree this refused.
func (r *Registry) discardStaged() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.staged = nil
}

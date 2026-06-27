package untrusted

import (
	"strings"
	"testing"
)

func TestWrapLabeled_EdgeCases(t *testing.T) {
	// An all-whitespace/sanitises-to-empty label falls back to the
	// unlabeled Wrap.
	if got := WrapLabeled("   ", "payload"); got != Wrap("payload") {
		t.Errorf("empty-after-sanitise label should fall back to Wrap; got %q", got)
	}

	// A valid label with EMPTY content emits just the open+close markers
	// (no escaping pass over an empty body).
	got := WrapLabeled("scraped_page", "")
	if !strings.Contains(got, `source="scraped_page"`) {
		t.Errorf("labeled wrap should carry the source attr; got %q", got)
	}
	if !strings.HasSuffix(got, closeTag) {
		t.Errorf("labeled empty content should end with the close tag; got %q", got)
	}
	if strings.Count(got, "\n") != 1 {
		t.Errorf("empty labeled content should have exactly one newline (open\\nclose); got %q", got)
	}
}

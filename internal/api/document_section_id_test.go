package api

import "testing"

// TestValidSectionID — defense-in-depth section_id validation: accept normal
// extractor-emitted ids, reject traversal / path-separator / control-byte ids.
func TestValidSectionID(t *testing.T) {
	ok := []string{"intro", "section-1", "chapter_2", "a.b.c", "OUTLINE-04ab"}
	for _, id := range ok {
		if !validSectionID(id) {
			t.Errorf("expected %q valid", id)
		}
	}
	bad := []string{
		"", "../etc/passwd", "a/../b", "foo/bar", `foo\bar`,
		"a\x00b", "line\nbreak", string(make([]byte, 129)),
	}
	for _, id := range bad {
		if validSectionID(id) {
			t.Errorf("expected %q rejected", id)
		}
	}
}

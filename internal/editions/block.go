package editions

import "strings"

// Marker pair delimiting the generated matrix inside docs/public/editions.md.
// Shared by the generator (cmd/docs-gen, which writes the block) and the linter
// (cmd/lint-lld-contracts, which checks the committed block hasn't drifted).
const (
	MatrixBeginMarker = "<!-- BEGIN GENERATED editions-matrix -->"
	MatrixEndMarker   = "<!-- END GENERATED editions-matrix -->"
)

// ExtractMatrix returns the content between the matrix markers (exclusive),
// trimmed of surrounding whitespace. ok is false if either marker is absent or
// the end precedes the begin.
func ExtractMatrix(doc string) (inner string, ok bool) {
	bi := strings.Index(doc, MatrixBeginMarker)
	if bi < 0 {
		return "", false
	}
	ei := strings.Index(doc, MatrixEndMarker)
	if ei < 0 || ei < bi {
		return "", false
	}
	return strings.TrimSpace(doc[bi+len(MatrixBeginMarker) : ei]), true
}

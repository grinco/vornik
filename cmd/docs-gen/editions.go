package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/editions"
)

const editionsPage = "docs/public/editions.md"

// The markers live in the editions package so the generator (here) and the
// drift linter share one definition. The page is mostly hand-written prose;
// only the table between these markers is generator OUTPUT — edit the editions
// matrix source in code, not the table.
const (
	editionsBeginMarker = editions.MatrixBeginMarker
	editionsEndMarker   = editions.MatrixEndMarker
)

// replaceMatrixBlock swaps the content between the editions-matrix markers
// (markers kept) for inner, surrounded by blank lines so the Markdown renders.
// It errors if either marker is missing or the end marker precedes the begin.
func replaceMatrixBlock(doc, inner string) (string, error) {
	bi := strings.Index(doc, editionsBeginMarker)
	if bi < 0 {
		return "", fmt.Errorf("begin marker %q not found", editionsBeginMarker)
	}
	ei := strings.Index(doc, editionsEndMarker)
	if ei < 0 {
		return "", fmt.Errorf("end marker %q not found", editionsEndMarker)
	}
	if ei < bi {
		return "", fmt.Errorf("end marker precedes begin marker")
	}
	return doc[:bi] + editionsBeginMarker + "\n\n" + inner + "\n" + doc[ei:], nil
}

// writeEditions regenerates the matrix block inside docs/public/editions.md.
func writeEditions(root string, deny []string) {
	path := filepath.Join(root, editionsPage)
	existing, err := os.ReadFile(path)
	if err != nil {
		fatal(fmt.Errorf("read %s: %w", editionsPage, err))
	}
	updated, err := replaceMatrixBlock(string(existing), editions.RenderMatrix())
	if err != nil {
		fatal(fmt.Errorf("%s: %w (add the generated-matrix markers to the page)", editionsPage, err))
	}
	writePage(path, updated, deny)
}

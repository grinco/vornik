package main

import (
	"strings"
	"testing"
)

func TestReplaceBlock_ReplacesInnerPreservingSurroundings(t *testing.T) {
	doc := "intro prose\n\n" + editionsBeginMarker + "\nOLD TABLE\n" + editionsEndMarker + "\n\noutro prose\n"
	out, err := replaceMatrixBlock(doc, "NEW TABLE")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "NEW TABLE") {
		t.Errorf("expected new inner content; got:\n%s", out)
	}
	if strings.Contains(out, "OLD TABLE") {
		t.Errorf("old inner content should be gone; got:\n%s", out)
	}
	if !strings.Contains(out, "intro prose") || !strings.Contains(out, "outro prose") {
		t.Errorf("surrounding prose must be preserved; got:\n%s", out)
	}
	if !strings.Contains(out, editionsBeginMarker) || !strings.Contains(out, editionsEndMarker) {
		t.Errorf("markers must be preserved; got:\n%s", out)
	}
}

func TestReplaceBlock_ErrorsWhenMarkerMissing(t *testing.T) {
	if _, err := replaceMatrixBlock("no markers here", "X"); err == nil {
		t.Error("expected an error when markers are absent")
	}
}

func TestReplaceBlock_ErrorsWhenEndBeforeBegin(t *testing.T) {
	doc := editionsEndMarker + "\nstuff\n" + editionsBeginMarker
	if _, err := replaceMatrixBlock(doc, "X"); err == nil {
		t.Error("expected an error when end marker precedes begin marker")
	}
}

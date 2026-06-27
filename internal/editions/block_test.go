package editions

import (
	"strings"
	"testing"
)

func TestExtractMatrix_ReturnsInnerBetweenMarkers(t *testing.T) {
	doc := "prose\n" + MatrixBeginMarker + "\n\nTABLE\n\n" + MatrixEndMarker + "\nmore\n"
	inner, ok := ExtractMatrix(doc)
	if !ok {
		t.Fatal("expected ok=true when both markers present")
	}
	if !strings.Contains(inner, "TABLE") {
		t.Errorf("expected inner to contain TABLE; got %q", inner)
	}
	if strings.Contains(inner, "prose") || strings.Contains(inner, "more") {
		t.Errorf("inner must not include surrounding prose; got %q", inner)
	}
}

func TestExtractMatrix_FalseWhenMarkersMissing(t *testing.T) {
	if _, ok := ExtractMatrix("no markers"); ok {
		t.Error("expected ok=false when markers absent")
	}
}

// The committed editions.md block must equal RenderMatrix() — round-trip guard.
func TestExtractMatrix_RoundTripsRenderMatrix(t *testing.T) {
	doc := MatrixBeginMarker + "\n\n" + RenderMatrix() + "\n" + MatrixEndMarker
	inner, ok := ExtractMatrix(doc)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if strings.TrimSpace(inner) != strings.TrimSpace(RenderMatrix()) {
		t.Errorf("extracted inner should equal RenderMatrix()\ninner: %q\nwant:  %q", inner, RenderMatrix())
	}
}

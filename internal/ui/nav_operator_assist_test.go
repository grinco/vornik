package ui

import (
	"testing"

	"vornik.io/vornik/internal/configassist"
)

// TestControlPlaneHub_LinksConfigAssistant pins that the configuration
// assistant is reachable by NAVIGATION — as a tab on the control-plane hub,
// where it belongs: every assist run files a control-plane proposal.
//
// Regression: the 2026-09-13 config-assistant feature shipped the door wired
// (ui.WithConfigAssistant in container_http.go) and rendering, but nothing
// linked to it, so the only way to reach /ui/operator/assist was to already
// know the path. The reference-host operator ran every assist through
// `vornikctl assist` and asked where the UI was.
//
// The tab LINKS OUT rather than rendering inside the hub on purpose: design
// §6.3 puts this door on the CE operator shell (operatorCapable), never in the
// EE admin router, which answers 501 EDITION_UNSUPPORTED in Community.
func TestControlPlaneHub_LinksConfigAssistant(t *testing.T) {
	s := NewServer(WithConfigAssistant(&configassist.Engine{}))
	var found *AdminCPSection
	for i, sec := range s.cpSections(cpSectionOverview) {
		if sec.Href == "/ui/operator/assist" {
			found = &s.cpSections(cpSectionOverview)[i]
		}
	}
	if found == nil {
		t.Fatal("control-plane hub has no tab linking to /ui/operator/assist — the assistant is unreachable except by typing the URL")
	}
	if found.Label == "" {
		t.Error("the assistant hub tab needs a label")
	}
}

// TestControlPlaneHub_OmitsAssistantWhenUnwired asserts the tab is data-driven
// like Diagnose: a daemon without the assistant engine must not advertise a
// tab that leads to a "not available" page.
func TestControlPlaneHub_OmitsAssistantWhenUnwired(t *testing.T) {
	s := NewServer()
	for _, sec := range s.cpSections(cpSectionOverview) {
		if sec.Href == "/ui/operator/assist" {
			t.Fatal("assistant tab must not be offered when the engine is not wired")
		}
	}
}

// TestOperatorAssist_HighlightsControlPlaneInRail pins that the assistant page
// lights up Control plane in the nav rail. The page has no nav destination of
// its own by design (it is a hub tab), so without this its CurrentPage names no
// nav key and the rail renders with nothing active.
func TestOperatorAssist_HighlightsControlPlaneInRail(t *testing.T) {
	var found bool
	for _, area := range navModel() {
		for _, d := range area.Dests {
			if d.Key == operatorAssistNavKey {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("operatorAssistNavKey = %q matches no nav destination key — the rail will highlight nothing on the assistant page", operatorAssistNavKey)
	}
}

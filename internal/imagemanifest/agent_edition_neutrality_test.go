package imagemanifest

// The blocking prerequisite for publishing the agent image from the PUBLIC CE
// repo to a public registry
// (https://docs.vornik.io §3.1).
//
// The agent image is a CE image (it is in baseImages, not manifest_enterprise),
// and publishing it publicly is safe ONLY because the binaries it carries —
// cmd/mcp-bridge and cmd/agent-helper — reach no Enterprise package. That is a
// property of the import graph today, held by nothing. internal/chat and
// internal/mcp carry no edition gates and give a contributor no reason to treat
// them as sensitive, so the first EE import added there would publish
// proprietary code to a public registry, irreversibly.
//
// Cheap guard, unbounded downside. That asymmetry is the whole argument.

import (
	"os/exec"
	"strings"
	"testing"
)

// agentBinaries are the commands baked into the published agent image
// (images/vornik-agent/Containerfile builds exactly these two).
var agentBinaries = []string{
	"vornik.io/vornik/cmd/mcp-bridge",
	"vornik.io/vornik/cmd/agent-helper",
}

// enterpriseOnlyPackages are the import paths the CE export prunes
// (scripts/export-public-ce.sh). A dependency on any of them means the binary
// cannot be built from the public tree — and, worse here, that shipping the
// image built from the private tree would publish Enterprise code.
//
// Suffix-matched against the full import path, so "internal/enterprise" cannot
// accidentally match "internal/enterprisey".
var enterpriseOnlyPackages = []string{
	"/internal/enterprise",
	"/internal/broker",
	"/internal/eula",
	"/services/",
}

// enterpriseDepsOf returns the Enterprise-only packages reachable from pkg.
func enterpriseDepsOf(t *testing.T, pkg string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", pkg, err, out)
	}
	var found []string
	for _, dep := range strings.Fields(string(out)) {
		for _, ee := range enterpriseOnlyPackages {
			// Trailing separator on the path so a prefix cannot match a
			// longer sibling name.
			if strings.Contains(dep+"/", ee+"/") {
				found = append(found, dep)
				break
			}
		}
	}
	return found
}

// TestAgentBinariesAreEditionNeutral is the gate. It must hold before the agent
// image may be published to a public registry, and it must keep holding
// afterwards — a regression here is a disclosure, not a build failure.
func TestAgentBinariesAreEditionNeutral(t *testing.T) {
	for _, pkg := range agentBinaries {
		if ee := enterpriseDepsOf(t, pkg); len(ee) > 0 {
			t.Errorf("%s reaches Enterprise-only package(s) %v.\n"+
				"The agent image built from these binaries is published to a PUBLIC registry, "+
				"so this would disclose Enterprise code. Either remove the dependency or stop "+
				"publishing the image — see 2026-08-28-packaged-image-provenance-design.md §3.1.",
				pkg, ee)
		}
	}
}

// TestEditionNeutralityDetectorDetects proves the gate above is not vacuous.
//
// A guard that cannot fail is not a guard, and this project has shipped one
// before: the 2026-08-14 gold manifest passed every test while holding CLI help
// text in its digest field, because every fixture used a placeholder that
// exercised the path and not the check. So the detector is pointed at a binary
// KNOWN to import Enterprise code, and must report it. If this test ever goes
// green-by-emptiness, TestAgentBinariesAreEditionNeutral is worthless.
func TestEditionNeutralityDetectorDetects(t *testing.T) {
	const eeMain = "vornik.io/vornik/cmd/vornik-enterprise"

	// This file also ships to the public CE tree, where cmd/vornik-enterprise
	// is pruned and there is no Enterprise code for the detector to find. Skip
	// there, LOUDLY: a silent skip is how a gate stops proving anything without
	// anyone noticing.
	if err := exec.Command("go", "list", eeMain).Run(); err != nil {
		t.Skipf("no Enterprise main in this tree (CE export) — "+
			"the detector cannot be exercised here; it is proven in the Enterprise repo, "+
			"where %s exists", eeMain)
	}

	if ee := enterpriseDepsOf(t, eeMain); len(ee) == 0 {
		t.Fatalf("detector reported %s as edition-neutral, which cannot be true: "+
			"it is the Enterprise daemon. The detector is broken, and "+
			"TestAgentBinariesAreEditionNeutral is therefore proving nothing.", eeMain)
	}
}

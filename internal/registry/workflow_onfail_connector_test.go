package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A RECOVERY STEP THAT NEEDS THE THING THAT BROKE.
//
// A step named as an `on_fail` target can itself call connector-backed tools.
// If the connector that failed the primary step is the one the recovery step
// needs — or another connector that is also down — the recovery fails
// circularly and routes to its own `on_fail`. That behaviour is correct; what
// was missing is that nothing said so at authoring time.
//
// The rule has been written down twice (the connector-auth design §7 and the
// public docs) and enforced nowhere, which is the "documented behaviour nothing
// implements" shape: it stops the next person looking.
//
// WARNING, never an error: the pattern is legal and sometimes deliberate (a
// recovery step that files a Jira ticket is depending on a connector ON
// PURPOSE, and the daemon-side operator alert reports the outage regardless).
// BACKLOG 2026-08-26.

func workflowWithRecovery(t *testing.T, recoveryRequireTools string) []byte {
	t.Helper()
	return []byte(`---
name: order-flow
description: test fixture
version: 1.0.0
steps:
  fetch:
    type: agent
    role: researcher
    prompt: fetch the order
    on_fail: report_problem
  report_problem:
    type: agent
    role: reporter
    prompt: say what broke
` + recoveryRequireTools + `
---

# Order flow
`)
}

// connectorFindings returns this check's findings only. Named for the one code
// it filters rather than parameterised: a `code string` argument that every
// caller passes the same literal to is a generalisation nothing asked for, and
// the linter is right to say so.
func connectorFindings(r *WorkflowMDValidationReport) []WorkflowMDFinding {
	var out []WorkflowMDFinding
	for _, f := range r.Findings {
		if f.Code == "on_fail_connector_dependency" {
			out = append(out, f)
		}
	}
	return out
}

func TestValidateWorkflow_OnFailStepRequiringAConnectorWarns(t *testing.T) {
	got := ValidateWorkflowMarkdown(workflowWithRecovery(t, "    require_tools:\n      - mcp__atlassian__create_issue"), "order-flow.md")

	hits := connectorFindings(got)
	if len(hits) != 1 {
		t.Fatalf("want exactly one on_fail_connector_dependency finding, got %d: %+v", len(hits), got.Findings)
	}
	if hits[0].Severity != SeverityWarning {
		t.Errorf("severity = %s, want WARNING — the pattern is legal and sometimes deliberate", hits[0].Severity)
	}
	if !strings.Contains(hits[0].Message, "report_problem") {
		t.Errorf("the finding must name the step: %q", hits[0].Message)
	}
	if !strings.Contains(hits[0].Message, "mcp__atlassian__create_issue") {
		t.Errorf("the finding must name the tool that creates the dependency: %q", hits[0].Message)
	}
	// The rule text is quoted VERBATIM from the design so the warning and the
	// documented rule cannot drift apart — which is the failure mode that put
	// this rule in two documents and no code.
	if !strings.Contains(hits[0].Message, "The daemon-side operator alert is not an MCP tool") {
		t.Errorf("the finding must quote the documented rule: %q", hits[0].Message)
	}
	if got.HasErrors() {
		t.Errorf("a legal pattern must not be an error: %+v", got.Findings)
	}
}

// A step that is NOT a recovery target may require whatever it likes. The
// warning is about the position in the graph, not about connector use.
func TestValidateWorkflow_OrdinaryStepRequiringAConnectorIsSilent(t *testing.T) {
	content := []byte(`---
name: order-flow
description: test fixture
version: 1.0.0
steps:
  fetch:
    type: agent
    role: researcher
    prompt: fetch the order
    require_tools:
      - mcp__atlassian__search
---

# Order flow
`)
	got := ValidateWorkflowMarkdown(content, "order-flow.md")
	if hits := connectorFindings(got); len(hits) != 0 {
		t.Fatalf("an ordinary step must not warn: %+v", hits)
	}
}

// A recovery step requiring a BUILT-IN tool is the shape the rule recommends —
// it reports failure without depending on anything that can be logged out.
func TestValidateWorkflow_OnFailStepRequiringABuiltinIsSilent(t *testing.T) {
	got := ValidateWorkflowMarkdown(workflowWithRecovery(t, "    require_tools:\n      - file_write"), "order-flow.md")
	if hits := connectorFindings(got); len(hits) != 0 {
		t.Fatalf("a recovery step on built-in tools must not warn: %+v", hits)
	}
}

// A step reachable BOTH as a recovery target and on the normal path is not
// primarily a reporter — warning there would fire on ordinary re-use of a step
// and teach authors to ignore the code.
func TestValidateWorkflow_StepAlsoReachableOnSuccessIsSilent(t *testing.T) {
	content := []byte(`---
name: order-flow
description: test fixture
version: 1.0.0
steps:
  fetch:
    type: agent
    role: researcher
    prompt: fetch the order
    on_success: publish
    on_fail: publish
  publish:
    type: agent
    role: publisher
    prompt: publish it
    require_tools:
      - mcp__atlassian__create_issue
---

# Order flow
`)
	got := ValidateWorkflowMarkdown(content, "order-flow.md")
	if hits := connectorFindings(got); len(hits) != 0 {
		t.Fatalf("a step on the success path too must not warn: %+v", hits)
	}
}

// Every shipped workflow must be clean, or the check is noise on the day it
// lands and authors learn to ignore the code.
func TestValidateWorkflow_ShippedWorkflowsHaveNoConnectorRecoveryWarning(t *testing.T) {
	entries, err := os.ReadDir(shippedWorkflowsDir)
	if err != nil {
		t.Fatalf("read %s: %v", shippedWorkflowsDir, err)
	}
	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(shippedWorkflowsDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		checked++
		if hits := connectorFindings(ValidateWorkflowMarkdown(content, e.Name())); len(hits) != 0 {
			t.Errorf("%s: %+v", e.Name(), hits)
		}
	}
	if checked == 0 {
		t.Fatal("no shipped workflows were examined; this assertion proves nothing")
	}
}

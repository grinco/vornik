package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// Diagnoser structured config_change (LLD 2026-07-11 §4.6): a verdict may
// select one allowlisted change kind; the daemon validates + renders it into
// an applyable proposal. Any validation/render failure degrades to the
// prose-only review proposal — never dropped, never half-applyable.

func newActionableDiagnoser(t *testing.T, llmJSON string) (*Diagnoser, persistence.ProposalRepository) {
	t.Helper()
	repo := newTuneTestRepo(t)
	return &Diagnoser{
		LLM:       &fakeProvider{content: llmJSON},
		Observe:   &fakeObserver{bundle: &DiagnoseBundle{Focus: "janka", ProjectID: "janka"}},
		Proposals: repo,
		Actionize: testActionizer(map[string]string{
			"configs/workflows/wf-a.md":   "---\nsteps:\n  slow:\n    type: agent\n    timeout: \"300s\"\n---\nbody\n",
			"configs/swarms/dev-swarm.md": "---\nroles:\n  - name: coder\n    model: \"old-model\"\n---\nbody\n",
			"config.yaml":                 "mcp:\n  servers:\n    - name: scraper\n      timeout_seconds: 30\n",
		}),
		Logger: zerolog.Nop(),
	}, repo
}

func diagnoseAndGet(t *testing.T, d *Diagnoser, repo persistence.ProposalRepository) *persistence.ControlPlaneProposal {
	t.Helper()
	_, id, err := d.Diagnose(context.Background(), "janka", true)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if id == "" {
		t.Fatal("expected a proposal to be filed")
	}
	p, err := repo.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	return p
}

func TestDiagnose_ConfigChangeStepTimeoutRendersApplyable(t *testing.T) {
	d, repo := newActionableDiagnoser(t, `{"root_cause":"step slow is being truncated","confidence":"high",
		"evidence":["e1"],"suggested_change":"raise the slow step timeout",
		"config_change":{"kind":"workflow_step_timeout","workflow":"wf-a","step":"slow","timeout":"9m"}}`)
	p := diagnoseAndGet(t, d, repo)
	if p.ApplyTarget != "configs/workflows/wf-a.md" || p.ApplyContent == "" || p.Diff == "" {
		t.Fatalf("proposal must be applyable: %+v", p)
	}
	if !strings.Contains(p.ApplyContent, "9m") {
		t.Fatalf("content must carry the 9m timeout:\n%s", p.ApplyContent)
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(p.Evidence), &ev); err != nil || ev["base_hash"] == nil || ev["change"] == nil {
		t.Fatalf("evidence must carry base_hash + change: %s (err %v)", p.Evidence, err)
	}
}

func TestDiagnose_ConfigChangeRoleModelSwarmScope(t *testing.T) {
	d, repo := newActionableDiagnoser(t, `{"root_cause":"model too slow","confidence":"high",
		"evidence":["e1"],"suggested_change":"switch coder to new-model",
		"config_change":{"kind":"swarm_role_model","swarm":"dev-swarm","role":"coder","model":"new-model"}}`)
	p := diagnoseAndGet(t, d, repo)
	if p.ApplyTarget != "configs/swarms/dev-swarm.md" || p.BlastRadius != persistence.ProposalScopeSwarm {
		t.Fatalf("swarm-scope applyable expected: %+v", p)
	}
}

func TestDiagnose_ConfigChangeMCPTimeout(t *testing.T) {
	d, repo := newActionableDiagnoser(t, `{"root_cause":"scraper calls time out","confidence":"high",
		"evidence":["e1"],"suggested_change":"raise scraper timeout",
		"config_change":{"kind":"mcp_server_timeout","server":"scraper","timeout_seconds":120}}`)
	p := diagnoseAndGet(t, d, repo)
	if p.ApplyTarget != "config.yaml" || !p.LiveApply || p.BlastRadius != persistence.ProposalScopeDaemon {
		t.Fatalf("daemon live-apply expected: %+v", p)
	}
	if !strings.Contains(p.ApplyContent, "timeout_seconds: 120") {
		t.Fatalf("content:\n%s", p.ApplyContent)
	}
}

// Each degradation path: unknown kind, hallucinated model, missing step,
// out-of-universe values — files the prose-only review proposal instead.
func TestDiagnose_ConfigChangeDegradesToProse(t *testing.T) {
	cases := map[string]string{
		"unknown kind":         `{"kind":"delete_everything","workflow":"wf-a"}`,
		"hallucinated model":   `{"kind":"swarm_role_model","swarm":"dev-swarm","role":"coder","model":"hallucinated-model"}`,
		"missing step":         `{"kind":"workflow_step_timeout","workflow":"wf-a","step":"ghost","timeout":"9m"}`,
		"unparseable timeout":  `{"kind":"workflow_step_timeout","workflow":"wf-a","step":"slow","timeout":"banana"}`,
		"raise-only violation": `{"kind":"mcp_server_timeout","server":"scraper","timeout_seconds":10}`,
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			d, repo := newActionableDiagnoser(t, `{"root_cause":"rc","confidence":"high","evidence":["e1"],
				"suggested_change":"do a thing","config_change":`+change+`}`)
			p := diagnoseAndGet(t, d, repo)
			if p.ApplyTarget != "" || p.ApplyContent != "" {
				t.Fatalf("%s must degrade to prose-only, got target %q", name, p.ApplyTarget)
			}
		})
	}
}

// The pre-existing gates still run FIRST: a suggested_change carrying an
// external URL files nothing at all, even with a valid config_change.
func TestDiagnose_URLGateStillWins(t *testing.T) {
	d, repo := newActionableDiagnoser(t, `{"root_cause":"rc","confidence":"high","evidence":["e1"],
		"suggested_change":"point it at https://evil.example.com",
		"config_change":{"kind":"mcp_server_timeout","server":"scraper","timeout_seconds":120}}`)
	_, id, err := d.Diagnose(context.Background(), "janka", true)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if id != "" {
		t.Fatal("URL-bearing suggestion must file no proposal")
	}
	if ps, _ := repo.List(context.Background(), persistence.ProposalListFilter{}); len(ps) != 0 {
		t.Fatalf("ledger must stay empty, got %d", len(ps))
	}
}

// Nil Actionizer (CE harness / not wired): behaviour is exactly the prior
// prose-only proposal.
func TestDiagnose_NilActionizerFilesProse(t *testing.T) {
	d, repo := newActionableDiagnoser(t, `{"root_cause":"rc","confidence":"high","evidence":["e1"],
		"suggested_change":"raise the slow step timeout",
		"config_change":{"kind":"workflow_step_timeout","workflow":"wf-a","step":"slow","timeout":"9m"}}`)
	d.Actionize = nil
	p := diagnoseAndGet(t, d, repo)
	if p.ApplyTarget != "" {
		t.Fatalf("nil actionizer must file prose-only, got %q", p.ApplyTarget)
	}
}

package configassist

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"vornik.io/vornik/internal/registry"
)

// Refusal is a named refusal. The assistant refuses with a reason the
// operator can act on; it never files a proposal it could not validate
// (design §5: "a bundle that fails any gate is never filed").
type Refusal struct {
	Code    string
	Message string
	// Findings carry the per-file detail (a skew diagnosis, a validation
	// finding, the file a secret sits in).
	Findings []string
}

func (r *Refusal) Error() string {
	if len(r.Findings) == 0 {
		return r.Code + ": " + r.Message
	}
	return r.Code + ": " + r.Message + " — " + strings.Join(r.Findings, "; ")
}

// Refusal codes.
const (
	RefusePaused            = "ASSISTANT_PAUSED"
	RefuseSubjectDisabled   = "ASSISTANT_SUBJECT_DISABLED"
	RefuseClassDisabled     = "ASSISTANT_CLASS_DISABLED"
	RefuseSecretHygiene     = "CONFIG_SECRET_HYGIENE"
	RefuseSchema            = "CONFIG_SCHEMA"
	RefuseSkew              = "CONFIG_VERSION_SKEW"
	RefuseDiffScope         = "DIFF_OUT_OF_SCOPE"
	RefuseEntrypointCeiling = "ENTRYPOINT_CLASS_CEILING"
	RefuseClassECap         = "CLASS_E_DAILY_CAP"
	RefuseOutputCap         = "MAX_OUTPUT_BYTES"
	RefuseTurnCap           = "MAX_TOOL_TURNS"
	RefuseNoChange          = "NO_CHANGE"
	RefusePlaceholder       = "SECRET_PLACEHOLDER_TAMPERED"
	RefuseIdentity          = "IDENTITY_UNAVAILABLE"
	RefuseBudget            = "BUDGET"
	RefuseModelFamily       = "JUDGE_SAME_FAMILY"
	RefuseAdmission         = "ADMISSION"
	// RefuseIdempotencyConflict is returned when an idempotency key that
	// already names a filed proposal is presented with a DIFFERENT request
	// (audit 2026-09-15 CA-16). Replaying the stored proposal would answer
	// a question the caller did not ask.
	RefuseIdempotencyConflict = "IDEMPOTENCY_KEY_CONFLICT"
)

// Gate results are refusals; nil means the bundle passed every
// deterministic gate.

// GateDeclaredScope is design §5.1: the finished overlay may touch only the
// files the plan declared. Test 4.
func GateDeclaredScope(declared []string, ops []Op, ignoredDeletions []string) *Refusal {
	allowed := map[string]bool{}
	for _, d := range declared {
		allowed[path.Clean(strings.TrimPrefix(d, "./"))] = true
	}
	var out []string
	for _, op := range ops {
		if !allowed[path.Clean(op.Path)] {
			out = append(out, op.Path)
		}
	}
	for _, d := range ignoredDeletions {
		out = append(out, d+" (deletion attempted; the assistant cannot delete files)")
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return &Refusal{Code: RefuseDiffScope, Message: "the edit touched files outside the declared plan", Findings: out}
}

// GateWorkspaceContext keeps the virtual workspace context bridge narrow:
// it may touch only this project's PROJECT_CONTEXT.md, and it may not be
// mixed with config-tree edits until a transactional cross-root apply design
// exists.
func GateWorkspaceContext(projectID string, ops []Op) *Refusal {
	workspaceTouches := 0
	var findings []string
	for _, op := range ops {
		if isWorkspaceVirtualPath(projectID, op.Path) {
			workspaceTouches++
			continue
		}
		if strings.HasSuffix(op.Path, "/PROJECT_CONTEXT.md") {
			findings = append(findings, op.Path+" is not the workspace context file for project "+projectID)
		}
	}
	if workspaceTouches > 0 && workspaceTouches != len(ops) {
		findings = append(findings, "workspace PROJECT_CONTEXT.md edits may not be mixed with config-tree edits")
	}
	if len(findings) == 0 {
		return nil
	}
	sort.Strings(findings)
	return &Refusal{Code: RefuseDiffScope, Message: "the edit touched files outside the workspace context bridge contract", Findings: findings}
}

// GateSchemas runs the loader's own validators over every op (design §5
// table): strict decode + skew diagnosis for project YAML, workflow
// validation, swarm parsing, feeds rules. Test 3: an unknown project key
// is refused WITH the skew diagnosis and nothing is filed. The
// deploy-ordering rule is spoken here: a key this binary does not know is
// "binary first, then config", never a written file (design §10).
//
//nolint:gocognit // The schema gate is deliberately a compact routing table by file type.
func GateSchemas(ops []Op) *Refusal {
	var schema, skew []string
	for _, op := range ops {
		lower := strings.ToLower(op.Path)
		switch {
		case strings.HasPrefix(lower, "projects/") && (strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml")):
			if err := registry.DecodeProjectStrict([]byte(op.Content)); err != nil {
				d := registry.DiagnoseProjectDecodeError(op.Path, []byte(op.Content), err)
				if d.LikelyVersionSkew || len(d.UnknownKeys) > 0 {
					skew = append(skew, renderSkew(op.Path, d))
				} else {
					schema = append(schema, op.Path+": "+err.Error())
				}
				continue
			}
			if f := feedsFindings(op); len(f) > 0 {
				schema = append(schema, f...)
			}
		case strings.HasPrefix(lower, "workflows/") && strings.HasSuffix(lower, ".md"):
			rep := registry.ValidateWorkflowMarkdown([]byte(op.Content), path.Base(op.Path))
			if rep != nil && rep.HasErrors() {
				for _, f := range rep.Findings {
					if f.Severity == registry.SeverityError {
						schema = append(schema, fmt.Sprintf("%s: %s: %s", op.Path, f.Code, f.Message))
					}
				}
			}
		case strings.HasPrefix(lower, "swarms/") && strings.HasSuffix(lower, ".md"):
			if _, err := registry.ParseSwarmMarkdown([]byte(op.Content), path.Base(op.Path)); err != nil {
				schema = append(schema, op.Path+": "+err.Error())
			}
		}
	}
	if len(skew) > 0 {
		return &Refusal{Code: RefuseSkew, Message: "the edit uses a key this binary does not accept — binary first, then config (2026-09-10 deploy-ordering rule); nothing was filed", Findings: skew}
	}
	if len(schema) > 0 {
		return &Refusal{Code: RefuseSchema, Message: "the edit fails the loader's validation; nothing was filed", Findings: schema}
	}
	return nil
}

func renderSkew(file string, d registry.SkewDiagnosis) string {
	var parts []string
	for _, k := range d.UnknownKeys {
		if s, ok := d.Suggestions[k]; ok {
			parts = append(parts, fmt.Sprintf("unknown key %q (did you mean %q?)", k, s))
		} else {
			parts = append(parts, fmt.Sprintf("unknown key %q (no spelling this binary accepts — likely a newer config than the binary)", k))
		}
	}
	return file + ": " + strings.Join(parts, ", ")
}

// feedsFindings applies the feeds rules (unique slug, positive cadence) the
// registry enforces at load.
func feedsFindings(op Op) []string {
	flat := FlattenPositional([]byte(op.Content))
	seen := map[string]bool{}
	var out []string
	for k, v := range flat {
		if !strings.HasPrefix(k, "autonomy.feeds.") || !strings.HasSuffix(k, ".slug") {
			continue
		}
		if seen[v] {
			out = append(out, fmt.Sprintf("%s: duplicate feed slug %q", op.Path, v))
		}
		seen[v] = true
		cadenceKey := strings.TrimSuffix(k, ".slug") + ".cadence"
		if c, ok := flat[cadenceKey]; ok {
			if d, err := parseLooseDuration(c); err != nil || d <= 0 {
				out = append(out, fmt.Sprintf("%s: feed %q cadence %q is not a positive duration", op.Path, v, c))
			}
		} else {
			out = append(out, fmt.Sprintf("%s: feed %q has no cadence", op.Path, v))
		}
	}
	sort.Strings(out)
	return out
}

// GateEntrypointCeiling is design §6.3: chat and agent entrypoints carry classes A and
// B2 only; a request through a raising entrypoint that resolves to B1, C, D or E
// is refused with the by-hand remedy, naming the class and the entrypoint
// (tests 21, 29).
func GateEntrypointCeiling(entrypoint, class string) *Refusal {
	if entrypoint != EntrypointChat && entrypoint != EntrypointAgent {
		return nil
	}
	if class == ClassA || class == ClassB2 {
		return nil
	}
	return &Refusal{Code: RefuseEntrypointCeiling, Message: fmt.Sprintf("class %s is not available through the %s entrypoint (it carries classes A and B2 only); make this change through the operator REST/CLI or console entrypoint, or by hand", class, entrypoint)}
}

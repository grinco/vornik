package memory

import (
	"strings"
	"testing"
)

func TestSoftMatchClaim(t *testing.T) {
	// Exact substring → score 1.0.
	if ok, score := SoftMatchClaim("make install", "Ran `make install` successfully", 0); !ok || score != 1.0 {
		t.Fatalf("substring: ok=%v score=%v", ok, score)
	}
	// Case-insensitive substring.
	if ok, _ := SoftMatchClaim("Make Install", "ran make install", 0); !ok {
		t.Fatal("case-insensitive failed")
	}
	// Empty inputs.
	if ok, _ := SoftMatchClaim("", "anything", 0); ok {
		t.Fatal("empty claim")
	}
	if ok, _ := SoftMatchClaim("claim", "", 0); ok {
		t.Fatal("empty audit")
	}
	if ok, _ := SoftMatchClaim("   ", "anything", 0); ok {
		t.Fatal("whitespace claim")
	}
	// Threshold ≥ 1 disables fuzzy.
	if ok, _ := SoftMatchClaim("alpha beta gamma", "alpha beta delta", 1.0); ok {
		t.Fatal("threshold=1 must disable fuzzy")
	}
	// Fuzzy hit: structured JSON audit row carrying the same tokens.
	jsonAudit := `{"command":"make install","cwd":"/repo"}`
	if ok, score := SoftMatchClaim("make install", jsonAudit, 0); !ok {
		t.Fatalf("structured json should match")
	} else if score <= 0 || score > 1 {
		t.Fatalf("score out of range: %v", score)
	}
	// No overlap → no match.
	if ok, _ := SoftMatchClaim("apple banana cherry", "zucchini eggplant", 0); ok {
		t.Fatal("unrelated must not match")
	}
}

func TestExtractClaims_BacktickCommands(t *testing.T) {
	content := "Ran `go test ./...` and then `make install` to verify."
	got := ExtractClaims(content)

	if got == nil {
		t.Fatalf("expected claims, got nil")
	}
	cmds := claimsByCategory(got, ClaimBacktickCommand)
	if len(cmds) != 2 {
		t.Fatalf("want 2 commands, got %d (%v)", len(cmds), cmds)
	}
	wantSet := map[string]bool{"go test ./...": false, "make install": false}
	for _, c := range cmds {
		if _, ok := wantSet[c.Value]; !ok {
			t.Errorf("unexpected command %q", c.Value)
			continue
		}
		wantSet[c.Value] = true
	}
	for k, seen := range wantSet {
		if !seen {
			t.Errorf("missing command %q in extraction", k)
		}
	}
}

func TestExtractClaims_FilePaths(t *testing.T) {
	content := "Edited internal/memory/gates.go and docs/release-notes/2026.4.13.md " +
		"plus configs/foo.yaml. The directory cmd/ should NOT match."
	got := ExtractClaims(content)
	paths := claimsByCategory(got, ClaimFilePath)
	want := []string{
		"internal/memory/gates.go",
		"docs/release-notes/2026.4.13.md",
		"configs/foo.yaml",
	}
	if len(paths) != len(want) {
		t.Fatalf("want %d file paths, got %d (%v)", len(want), len(paths), paths)
	}
	for _, w := range want {
		if !containsClaim(paths, w) {
			t.Errorf("missing file path %q in extraction (got %v)", w, paths)
		}
	}
}

func TestExtractClaims_URLs(t *testing.T) {
	content := "See https://example.com/path?q=1 and http://localhost:8080/health."
	got := ExtractClaims(content)
	urls := claimsByCategory(got, ClaimURL)
	if len(urls) != 2 {
		t.Fatalf("want 2 urls, got %d (%v)", len(urls), urls)
	}
	if !containsClaim(urls, "https://example.com/path?q=1") {
		t.Errorf("missing https URL")
	}
	if !containsClaim(urls, "http://localhost:8080/health") {
		t.Errorf("missing http URL")
	}
}

func TestExtractClaims_GitSHA_GatedByCommitContext(t *testing.T) {
	// Bare hex without commit context → no git_sha claims (too noisy).
	noctx := "the deadbeef token shows in our cache key for c0ffee123abc"
	if shas := claimsByCategory(ExtractClaims(noctx), ClaimGitSHA); len(shas) != 0 {
		t.Errorf("bare hex should NOT yield git_sha; got %v", shas)
	}

	// With commit context, hex matches.
	withctx := "Reverted commit deadbeef1234567 after rebase failure."
	got := ExtractClaims(withctx)
	shas := claimsByCategory(got, ClaimGitSHA)
	if len(shas) != 1 {
		t.Fatalf("want 1 sha, got %d (%v)", len(shas), shas)
	}
	if shas[0].Value != "deadbeef1234567" {
		t.Errorf("got sha %q want deadbeef1234567", shas[0].Value)
	}
}

func TestExtractClaims_EntityIDs(t *testing.T) {
	content := "Linked task_20260508110359_c831358c3fccdf26 to execution_abc12345def."
	got := ExtractClaims(content)
	ents := claimsByCategory(got, ClaimEntityID)
	if len(ents) != 2 {
		t.Fatalf("want 2 entity ids, got %d (%v)", len(ents), ents)
	}
}

func TestExtractClaims_Empty(t *testing.T) {
	if got := ExtractClaims(""); got != nil {
		t.Errorf("empty content should yield nil, got %v", got)
	}
}

func TestExtractClaims_Dedup(t *testing.T) {
	// Same command twice in different sentences → one claim.
	content := "Ran `go test`. Then ran `go test` again to be sure."
	got := ExtractClaims(content)
	cmds := claimsByCategory(got, ClaimBacktickCommand)
	if len(cmds) != 1 {
		t.Fatalf("want 1 deduped command, got %d (%v)", len(cmds), cmds)
	}
}

func TestExtractClaims_PureProse(t *testing.T) {
	content := "We discussed the architectural approach during today's standup. " +
		"The team agreed to proceed iteratively. No specific commands or paths."
	got := ExtractClaims(content)
	if len(got) != 0 {
		t.Errorf("pure prose should yield 0 claims, got %d (%v)", len(got), got)
	}
}

// ---- ClaimAuditOverlapGate verdicts ----

func TestClaimAuditOverlapGate_NoClaims_AutoAllow(t *testing.T) {
	c := &IngestCandidate{Content: "prose only"}
	out := ClaimAuditOverlapGate(c, DefaultGateConfig())
	if out.Action != GateAllow {
		t.Errorf("no claims → expected Allow, got %v (%s)", out.Action, out.Detail)
	}
	if out.ShadowSignal {
		t.Errorf("no claims → ShadowSignal should be false")
	}
}

// TestClaimAuditOverlapGate_ZeroOverlap_DefaultShadows confirms the
// new default behaviour: with ClaimAuditMinMatchRatio=0 the gate
// shadow-flags rather than quarantining when no claims grounded.
// Pre-2026-05-21 the gate hard-quarantined here, which buried 32
// research-class writes under "0/N grounded" caused largely by
// URL/path normalisation drift between the writer's narrative and
// the tool's recorded args.
func TestClaimAuditOverlapGate_ZeroOverlap_DefaultShadows(t *testing.T) {
	c := &IngestCandidate{
		ClaimAuditResults: []ClaimMatch{
			{Claim: Claim{Category: ClaimBacktickCommand, Value: "go test"}, Found: false},
			{Claim: Claim{Category: ClaimBacktickCommand, Value: "make install"}, Found: false},
		},
	}
	out := ClaimAuditOverlapGate(c, DefaultGateConfig())
	if out.Action != GateAllow {
		t.Fatalf("default ratio (0) must allow with shadow, got %v", out.Action)
	}
	if !out.ShadowSignal {
		t.Errorf("zero overlap under default ratio must flip ShadowSignal")
	}
	if !strings.Contains(out.Detail, "0/2") {
		t.Errorf("Detail should mention 0/2 overlap, got %q", out.Detail)
	}
}

// TestClaimAuditOverlapGate_ZeroOverlap_StrictRatioQuarantines —
// projects that opt in to strict claim verification (any ratio > 0)
// still get the quarantine on 0/N. Confirms the new knob is
// load-bearing both ways.
func TestClaimAuditOverlapGate_ZeroOverlap_StrictRatioQuarantines(t *testing.T) {
	c := &IngestCandidate{
		ClaimAuditResults: []ClaimMatch{
			{Claim: Claim{Category: ClaimBacktickCommand, Value: "go test"}, Found: false},
		},
	}
	cfg := DefaultGateConfig()
	cfg.ClaimAuditMinMatchRatio = 0.5
	out := ClaimAuditOverlapGate(c, cfg)
	if out.Action != GateQuarantine {
		t.Fatalf("strict ratio must quarantine on 0/1, got %v", out.Action)
	}
	if !strings.Contains(out.Detail, "below min 50%") {
		t.Errorf("Detail should mention the breached ratio, got %q", out.Detail)
	}
}

func TestClaimAuditOverlapGate_FullOverlap_Allow(t *testing.T) {
	c := &IngestCandidate{
		ClaimAuditResults: []ClaimMatch{
			{Claim: Claim{Category: ClaimBacktickCommand, Value: "go test"}, Found: true, AuditRowID: "row-1"},
			{Claim: Claim{Category: ClaimFilePath, Value: "main.go"}, Found: true, AuditRowID: "row-2"},
		},
	}
	out := ClaimAuditOverlapGate(c, DefaultGateConfig())
	if out.Action != GateAllow {
		t.Errorf("full overlap → expected Allow, got %v (%s)", out.Action, out.Detail)
	}
	if out.ShadowSignal {
		t.Errorf("full overlap → ShadowSignal should be false")
	}
	if !strings.Contains(out.Detail, "2/2") {
		t.Errorf("Detail should mention 2/2, got %q", out.Detail)
	}
}

func TestClaimAuditOverlapGate_PartialOverlap_AllowWithShadow(t *testing.T) {
	c := &IngestCandidate{
		ClaimAuditResults: []ClaimMatch{
			{Claim: Claim{Category: ClaimBacktickCommand, Value: "go test"}, Found: true},
			{Claim: Claim{Category: ClaimBacktickCommand, Value: "make deploy"}, Found: false},
			{Claim: Claim{Category: ClaimFilePath, Value: "main.go"}, Found: true},
			{Claim: Claim{Category: ClaimFilePath, Value: "ghost.go"}, Found: false},
		},
	}
	out := ClaimAuditOverlapGate(c, DefaultGateConfig())
	if out.Action != GateAllow {
		t.Fatalf("partial overlap → expected Allow, got %v", out.Action)
	}
	if !out.ShadowSignal {
		t.Errorf("partial overlap → ShadowSignal must be true")
	}
	if !strings.Contains(out.Detail, "partial_audit:") {
		t.Errorf("Detail should use structured prefix, got %q", out.Detail)
	}
	if !strings.Contains(out.Detail, "2/4") {
		t.Errorf("Detail should mention 2/4 overlap, got %q", out.Detail)
	}
}

func TestClaimAuditOverlapGate_NilCandidate(t *testing.T) {
	out := ClaimAuditOverlapGate(nil, DefaultGateConfig())
	if out.Action != GateAllow {
		t.Errorf("nil candidate → expected Allow (defensive), got %v", out.Action)
	}
}

// ---- RunStandardGates with audit overlap wired in ----

func TestRunStandardGates_ClaimAuditQuarantineShortCircuits(t *testing.T) {
	c := &IngestCandidate{
		ProjectID:        "p1",
		SourceArtifactID: "a1",
		ProducerRole:     "coder",
		Content:          strings.Repeat("a", 200), // long enough for min_content
		ClaimAuditResults: []ClaimMatch{
			{Claim: Claim{Category: ClaimBacktickCommand, Value: "fake-cmd"}, Found: false},
		},
	}
	// Opt in to the strict ratio to exercise the short-circuit
	// path. Under the default ratio (0) zero overlap shadow-flags
	// rather than quarantining — see
	// TestClaimAuditOverlapGate_ZeroOverlap_DefaultShadows.
	cfg := DefaultGateConfig()
	cfg.ClaimAuditMinMatchRatio = 0.5
	final, trail := RunStandardGates(c, cfg, nil, 0, nil)
	if final.Action != GateQuarantine {
		t.Fatalf("want Quarantine, got %v (gate=%s detail=%s)", final.Action, final.Gate, final.Detail)
	}
	if final.Gate != GateClaimAuditOverlap {
		t.Errorf("expected gate %s, got %s", GateClaimAuditOverlap, final.Gate)
	}
	// Trail order: schema, provenance, class, secret, policy_match,
	// prompt_injection, claim_audit_overlap → audit gate at slot 6.
	if len(trail) < 7 {
		t.Fatalf("trail too short (%d): %+v", len(trail), trail)
	}
	if trail[6].Gate != GateClaimAuditOverlap {
		t.Errorf("expected slot 6 gate %s, got %s", GateClaimAuditOverlap, trail[6].Gate)
	}
}

func TestRunStandardGates_ShadowSignalLatchedAcrossLaterGates(t *testing.T) {
	c := &IngestCandidate{
		ProjectID:        "p1",
		SourceArtifactID: "a1",
		ProducerRole:     "coder",
		Content: "Ran `go test ./...` and `make install` for the build. " +
			strings.Repeat("filler word ", 30), // ensure min_content passes
		ClaimAuditResults: []ClaimMatch{
			{Claim: Claim{Category: ClaimBacktickCommand, Value: "go test ./..."}, Found: true},
			{Claim: Claim{Category: ClaimBacktickCommand, Value: "make install"}, Found: false},
		},
	}
	final, trail := RunStandardGates(c, DefaultGateConfig(), nil, 0, nil)
	if final.Action != GateAllow {
		t.Fatalf("partial overlap should still allow; got %v (gate=%s)", final.Action, final.Gate)
	}
	if !final.ShadowSignal {
		t.Errorf("expected ShadowSignal latched on final outcome")
	}
	// Walk the trail to find the audit gate's outcome.
	var auditGate *GateOutcome
	for i := range trail {
		if trail[i].Gate == GateClaimAuditOverlap {
			auditGate = &trail[i]
			break
		}
	}
	if auditGate == nil {
		t.Fatalf("trail missing claim_audit_overlap entry: %+v", trail)
	}
	if !auditGate.ShadowSignal {
		t.Errorf("audit gate should have ShadowSignal=true")
	}
}

func TestRunStandardGates_NoClaims_AllowsCleanly(t *testing.T) {
	c := &IngestCandidate{
		ProjectID:        "p1",
		SourceArtifactID: "a1",
		ProducerRole:     "coder",
		Content: "Plain prose about architecture decisions, no commands or paths. " +
			strings.Repeat("filler ", 20),
	}
	final, _ := RunStandardGates(c, DefaultGateConfig(), nil, 0, nil)
	if final.Action != GateAllow {
		t.Errorf("clean prose should allow, got %v (%s)", final.Action, final.Detail)
	}
	if final.ShadowSignal {
		t.Errorf("no claims → ShadowSignal must remain false")
	}
}

// ---- helpers ----

func claimsByCategory(in []Claim, cat ClaimCategory) []Claim {
	var out []Claim
	for _, c := range in {
		if c.Category == cat {
			out = append(out, c)
		}
	}
	return out
}

func containsClaim(in []Claim, val string) bool {
	for _, c := range in {
		if c.Value == val {
			return true
		}
	}
	return false
}

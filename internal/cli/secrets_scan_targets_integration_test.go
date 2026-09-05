//go:build integration

package cli

// The historical redaction of the prompt stores, against a real Postgres.
//
// These cannot be sqlmock tests: what is under test is a re-key — an INSERT
// whose ON CONFLICT matters, a repoint whose CASE arithmetic matters, and a
// transaction whose atomicity is the entire safety argument. A mock that
// matched query text would assert the strings and prove nothing.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// scanTargetsDetector builds the same detector the command builds, with the
// default corpus, so the synthetic credential below is matched by the rule it
// would be matched by in production.
func scanTargetsDetector(t *testing.T) *secrets.MultiDetector {
	t.Helper()
	det, err := secrets.NewMultiDetector(secrets.Config{
		Patterns:  secrets.EffectivePatterns(nil, nil),
		Allowlist: secrets.DefaultAllowlist(),
	})
	if err != nil {
		t.Fatalf("build detector: %v", err)
	}
	return det
}

const scanTargetSecret = "AKIAIOSFODNN7EXAMPLE"

// allRules is the --rules all selection: these tests are about the rewrite
// mechanics, not about scoping, which secrets_sample_test.go already covers.
func allRules() ruleSelection { return ruleSelection{spec: ruleSpecAll, all: true} }

func pqArray(vals ...string) any { return pq.Array(vals) }

func seedChatPrompt(t *testing.T, db *sql.DB, body string) string {
	t.Helper()
	hash := persistence.HashChatSystemPrompt(body)
	if _, err := db.Exec(
		`INSERT INTO chat_system_prompts (hash, body, created_at) VALUES ($1,$2,NOW()) ON CONFLICT (hash) DO NOTHING`,
		hash, body); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	return hash
}

// A prompt body carrying a credential is re-keyed: the redacted body lands
// under the hash of its own bytes, every referring row is repointed, and the
// raw row is gone. The identity of a content-addressed row IS its content, so
// redacting one is a move, not an edit (design §5.1).
func TestScanContentStore_RekeysAndRepoints(t *testing.T) {
	db := dbcovSetup(t)
	ctx := context.Background()
	project := dbcovUniqueProject("scanre")
	t.Cleanup(func() { dbcovCleanupProject(t, db, project) })

	body := "You are the assistant. Use " + scanTargetSecret + " to deploy."
	oldHash := seedChatPrompt(t, db, body)
	turnID := project + "-turn-1"
	if _, err := db.Exec(
		`INSERT INTO chat_audit_log (id, ts, chat_id, project_id, system_prompt_hash)
		 VALUES ($1, NOW(), 'c1', $2, $3)`, turnID, project, oldHash); err != nil {
		t.Fatalf("seed turn: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM chat_audit_log WHERE id = $1`, turnID)
		_, _ = db.Exec(`DELETE FROM chat_system_prompts WHERE hash = $1`, oldHash)
	})

	store := contentStores[0] // chat_system_prompts
	res, err := scanContentStore(ctx, db, store, scanTargetsDetector(t), time.Time{}, true, allRules(), 0)
	if err != nil {
		t.Fatalf("scanContentStore: %v", err)
	}
	if res.RowsMatched == 0 {
		t.Fatal("the seeded body must be matched")
	}

	// The raw row is gone…
	var stillThere int
	if err := db.QueryRow(`SELECT COUNT(*) FROM chat_system_prompts WHERE hash = $1`, oldHash).Scan(&stillThere); err != nil {
		t.Fatal(err)
	}
	if stillThere != 0 {
		t.Error("the raw body must not survive the re-key")
	}

	// …the turn points at a new hash, and that hash names a redacted body
	// whose digest is itself.
	var newHash, newBody string
	if err := db.QueryRow(
		`SELECT c.system_prompt_hash, p.body FROM chat_audit_log c
		   JOIN chat_system_prompts p ON p.hash = c.system_prompt_hash
		  WHERE c.id = $1`, turnID).Scan(&newHash, &newBody); err != nil {
		t.Fatalf("the referrer must resolve after the repoint: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM chat_system_prompts WHERE hash = $1`, newHash) })

	if newHash == oldHash {
		t.Error("the hash must change with the body")
	}
	if newHash != persistence.HashChatSystemPrompt(newBody) {
		t.Error("the stored hash must be the digest of the stored bytes")
	}
	if contains(newBody, scanTargetSecret) {
		t.Errorf("the credential survived: %q", newBody)
	}

	// Idempotent: a second pass finds nothing to do.
	res2, err := scanContentStore(ctx, db, store, scanTargetsDetector(t), time.Time{}, true, allRules(), 0)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	for _, n := range res2.SelectedByType {
		if n > 0 {
			t.Errorf("a second pass must find nothing selected, got %v", res2.SelectedByType)
			break
		}
	}
}

// Two bodies that redact to the SAME bytes collapse onto one row, and both
// referrers follow. ON CONFLICT DO NOTHING makes the insert a no-op; the
// repoint is what has to be right (design §5.1, review round 1 F4).
func TestScanContentStore_CollisionCollapsesOntoOneRow(t *testing.T) {
	db := dbcovSetup(t)
	ctx := context.Background()
	project := dbcovUniqueProject("scancol")
	t.Cleanup(func() { dbcovCleanupProject(t, db, project) })

	// Same prose, different credentials → identical after redaction.
	bodyA := "Deploy with " + scanTargetSecret + " now."
	bodyB := "Deploy with AKIAJJJJJJJJJJJJJJJJ now."
	hashA := seedChatPrompt(t, db, bodyA)
	hashB := seedChatPrompt(t, db, bodyB)
	if hashA == hashB {
		t.Fatal("precondition: the two seeded bodies must differ")
	}
	for i, h := range []string{hashA, hashB} {
		id := project + "-turn-" + string(rune('a'+i))
		if _, err := db.Exec(
			`INSERT INTO chat_audit_log (id, ts, chat_id, project_id, system_prompt_hash)
			 VALUES ($1, NOW(), 'c1', $2, $3)`, id, project, h); err != nil {
			t.Fatalf("seed turn: %v", err)
		}
		t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM chat_audit_log WHERE id = $1`, id) })
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM chat_system_prompts WHERE hash = ANY($1)`, pqArray(hashA, hashB))
	})

	if _, err := scanContentStore(ctx, db, contentStores[0], scanTargetsDetector(t), time.Time{}, true, allRules(), 0); err != nil {
		t.Fatalf("scanContentStore: %v", err)
	}

	var distinct int
	if err := db.QueryRow(
		`SELECT COUNT(DISTINCT system_prompt_hash) FROM chat_audit_log WHERE project_id = $1`, project).Scan(&distinct); err != nil {
		t.Fatal(err)
	}
	if distinct != 1 {
		t.Errorf("both turns must point at the single collapsed row, got %d distinct hashes", distinct)
	}
	var orphans int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM chat_audit_log c
		  WHERE c.project_id = $1
		    AND NOT EXISTS (SELECT 1 FROM chat_system_prompts p WHERE p.hash = c.system_prompt_hash)`,
		project).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Errorf("%d referrer(s) point at a body that is not there", orphans)
	}
}

// chat_audit_log is an ordinary row: its free text is rewritten in place, and
// a column with nothing selected is left exactly as it was.
func TestScanChatAuditHistory_RedactsInPlace(t *testing.T) {
	db := dbcovSetup(t)
	ctx := context.Background()
	project := dbcovUniqueProject("scanchat")
	t.Cleanup(func() { dbcovCleanupProject(t, db, project) })

	id := project + "-turn"
	if _, err := db.Exec(`INSERT INTO chat_audit_log
		(id, ts, chat_id, project_id, user_message, response, tool_calls_json)
		VALUES ($1, NOW(), 'c1', $2, $3, $4, $5)`,
		id, project,
		"deploy with "+scanTargetSecret,
		"an ordinary answer with no credential in it",
		`[{"name":"shell","args":"export K=`+scanTargetSecret+`"}]`); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM chat_audit_log WHERE id = $1`, id) })

	res, err := scanChatAuditHistory(ctx, db, scanTargetsDetector(t), project, time.Time{}, true, allRules(), 0)
	if err != nil {
		t.Fatalf("scanChatAuditHistory: %v", err)
	}
	if res.RowsMatched != 1 {
		t.Fatalf("rows matched = %d, want 1", res.RowsMatched)
	}

	var userMsg, response, toolCalls string
	if err := db.QueryRow(
		`SELECT user_message, response, tool_calls_json FROM chat_audit_log WHERE id = $1`, id,
	).Scan(&userMsg, &response, &toolCalls); err != nil {
		t.Fatal(err)
	}
	if contains(userMsg, scanTargetSecret) || contains(toolCalls, scanTargetSecret) {
		t.Errorf("the credential survived: %q / %q", userMsg, toolCalls)
	}
	if response != "an ordinary answer with no credential in it" {
		t.Errorf("a clean column must be left byte-identical, got %q", response)
	}
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

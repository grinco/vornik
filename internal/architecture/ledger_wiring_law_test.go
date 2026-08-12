package architecture

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Slice A of https://docs.vornik.io
//
// WHY A TEXT-MATCHING GUARD EXISTS AT ALL.
// Every component that bills an LLM call gets its usage recorder from ONE
// assignment in the service container. Delete that line and the component keeps
// working, keeps calling the provider, and silently stops billing. Measured
// 2026-08-12: removing `titler.LLMUsage = c.repos.LLMUsage` leaves
// `go test ./...` green.
//
// That is not a hypothetical failure mode. It has reached production three times
// — the instinct distiller (field assigned only in a test), the memory reranker
// (no field at all), and the embedder (no recording at all) — and every one was
// found by an operator reading a bill rather than by this test suite.
//
// Neither existing law can see it: whether a recorder is non-nil is a runtime
// property of container assembly, invisible to an import law or an AST registry.
//
// THE STOPGAP HALF OF THIS GUARD IS GONE — it deleted itself, as designed.
// Every billing component now takes an internal/llmspend.Recorder at
// construction, so the compiler asks "what happens to this spend?" and a regex
// no longer has to. The retirement check fired on 2026-08-12 the moment the last
// biller migrated, and this file was cut down in response.
//
// What remains is permanent and stronger:
//   - every place handed the usage repository must be CLASSIFIED (reader or
//     biller), so a new one cannot appear silently; and
//   - no entry may be a BILLER, because billing belongs to the seam.
//
// The entries below are therefore all readers — budget checks, cost monitors,
// spend dashboards — documenting who consults the ledger without writing to it.

// ledgerWiring is one classified site where a component is handed (or not handed)
// the usage repository.
type ledgerWiring struct {
	// file is the non-test source file holding the assignment, relative to the
	// module root.
	file string
	// snippet is the assignment text that must be present. Matched as a
	// substring after whitespace collapsing, so gofmt alignment changes do not
	// break the guard, but deleting the line does.
	snippet string
	// bills is true when this wiring makes the component WRITE task_llm_usage
	// rows. False marks a reader (budget checks, cost monitors) — still
	// classified, because a new site of either kind must be a deliberate
	// decision, and because mistaking a reader for a biller is how an audit
	// over-reports coverage.
	bills bool
	// note explains what the component is, and for a reader why it does not
	// bill. Required either way.
	note string
}

// MIGRATED to internal/llmspend and therefore REMOVED from this registry (slice C):
// memory.titler, memory.classifier, memory.narrative, memory.graph,
// chat.remember.ned, and memory.embedder (whose post-construction SetSpend keeps
// its own dedicated guard in internal/memory/embed_usage_test.go, since a call
// that is not a constructor argument is what a compiler cannot check). Their recorders are now
// constructor arguments, so the compiler enforces what a text match used to.
// TestMigratedComponentsLeaveTheRegistry fails if a migrated component is listed
// here, which is what forced these deletions rather than a reviewer noticing.
var ledgerWiringRegistry = map[string]ledgerWiring{

	"workflow_step": {
		file:    "internal/service/container_scheduler.go",
		snippet: "executor.WithLLMUsageRepository(",
		bills:   false,
		note:    "Executor READS the ledger for budget env injection (container.go:1632, :1862). Its step BILLING moved to executor.WithSpend, which is compiler-enforced.",
	},
	"dispatcher": {
		file:    "internal/service/container_dispatcher.go",
		snippet: "dispatcher.WithLLMUsageRepository(c.repos.LLMUsage)",
		bills:   false,
		note:    "Dispatcher READS the ledger for budget.Check / ForecastTask in the tool executor. Its chat-turn BILLING moved to dispatcher.WithSpend.",
	},
	"_authoring": {
		file:    "internal/service/container_http.go",
		snippet: "ui.WithLLMUsageRepository(c.repos.LLMUsage)",
		bills:   false,
		note:    "UI READS the ledger for /ui/spend. The authoring assistant's BILLING moved to Server.assistantSpend, wired from the same option.",
	},

	"external_api": {
		file:    "internal/service/container_http.go",
		snippet: "api.WithLLMUsageRepository(c.repos.LLMUsage)",
		bills:   false,
		note:    "API READS the ledger for spend endpoints. Both writers (the OpenAI-compatible proxy and the agent's streaming upsert) moved to Server.externalAPISpend / workflowStepSpend, wired from the same option.",
	},

	// ---- readers, classified so a new one is still a decision ----
	"telegram.budget_check": {
		file:    "internal/service/container_subsystems.go",
		snippet: "telegram.WithLLMUsageRepository(c.repos.LLMUsage)",
		bills:   false,
		note:    "Telegram bot READS the ledger to enforce per-project budgets in create_task (bot.go:344). Writes no rows.",
	},
	"projectdoctor.smoke": {
		file:    "internal/service/container_http.go",
		snippet: "newSmokeRunner(taskCreator, c.repos.Tasks, c.repos.LLMUsage)",
		bills:   false,
		note:    "Project-doctor smoke runner calls usage.List to report a probe task's cost. Writes no rows.",
	},
	"taskcreate.budget_check": {
		file:    "internal/service/container_http.go",
		snippet: "taskcreate.WithLLMUsageRepository(c.repos.LLMUsage)",
		bills:   false,
		note:    "Task creation READS the ledger for budget.Check / Reserve / ForecastTask (creator.go:339, :407, :468). Writes no rows.",
	},
	"autonomy.repo_local": {
		file:    "internal/service/container_autonomy.go",
		snippet: "llmUsageRepo := c.repos.LLMUsage",
		bills:   false,
		note:    "The local binding feeding autonomy.WithLLMUsageRepository below; the same reader, matched twice by the deliberately broad pattern.",
	},
	"autonomy.budget_check": {
		file:    "internal/service/container_autonomy.go",
		snippet: "autonomy.WithLLMUsageRepository(llmUsageRepo)",
		bills:   false,
		note:    "Autonomy READS the ledger for budget.Check before creating a task (manager.go:812, :1936). It writes no rows.",
	},
	"effective_cost_monitor": {
		file:    "internal/service/container_scheduler.go",
		snippet: "llmRepo := c.repos.LLMUsage",
		bills:   false,
		note:    "Effective-cost monitor: reads spend-per-success to detect drift. Writes no rows.",
	},
	"fix_it_doctor.budget": {
		file:    "internal/service/fixit_doctor_adapter.go",
		snippet: "BudgetRepo:        c.repos.LLMUsage",
		bills:   false,
		note:    "Same repo passed a SECOND time as a budget reader; the biller is the LLMUsage field above.",
	},
}

// wiringSitePattern finds any place the usage repository is handed to something.
// Deliberately broad: the guard's job is to make a new site impossible to add
// silently, so over-matching (and forcing a classification) is the safe error.
var wiringSitePattern = regexp.MustCompile(`(?:=|:)\s*(?:c\.)?repos\.LLMUsage\b|WithLLMUsageRepository\(`)

// discoverWiringSites returns "file\x00collapsed-line" for every match in
// non-test source under internal/ and cmd/.
func discoverWiringSites(t *testing.T, root string) map[string]string {
	t.Helper()
	found := map[string]string{}
	for _, sub := range []string{"internal", "cmd"} {
		base := filepath.Join(root, sub)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				if info.Name() == "testdata" || info.Name() == "vendor" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			for _, line := range strings.Split(string(b), "\n") {
				if !wiringSitePattern.MatchString(line) {
					continue
				}
				// An option DEFINITION (`func WithLLMUsageRepository(...)`) is
				// plumbing, not a decision about a component's spend — the
				// decision is the call site that passes the repo in. Likewise the
				// repository's own package.
				if strings.HasPrefix(collapse(line), "func With") {
					continue
				}
				if strings.Contains(rel, "internal/persistence/") {
					continue
				}
				found[rel+"\x00"+collapse(line)] = rel
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
	}
	return found
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// TestEveryBillingComponentIsWired is the guard proper: each classified
// component's wiring assignment must still be present in its file.
func TestEveryBillingComponentIsWired(t *testing.T) {
	root := moduleRoot(t)
	cache := map[string]string{}

	names := make([]string, 0, len(ledgerWiringRegistry))
	for n := range ledgerWiringRegistry {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		e := ledgerWiringRegistry[name]
		if strings.TrimSpace(e.note) == "" {
			t.Errorf("%s: empty note — an undocumented entry is indistinguishable from an unconsidered one", name)
		}
		src, ok := cache[e.file]
		if !ok {
			b, err := os.ReadFile(filepath.Join(root, e.file))
			if err != nil {
				t.Errorf("%s: cannot read %s: %v", name, e.file, err)
				continue
			}
			src = collapse(string(b))
			cache[e.file] = src
		}
		if !strings.Contains(src, collapse(e.snippet)) {
			verb := "reads"
			if e.bills {
				verb = "BILLS"
			}
			t.Errorf("%s (%s the ledger) has lost its wiring: %q not found in %s.\n"+
				"If this component now takes an llmspend.Recorder, delete its registry entry "+
				"(the compiler enforces it). If the line was removed for any other reason, its "+
				"LLM spend is now invisible — which has reached production three times and was "+
				"always found by an operator reading a bill.", name, verb, e.snippet, e.file)
		}
	}
}

// TestEveryWiringSiteIsClassified is the reverse direction: a NEW place that
// receives the usage repository must be classified, biller or reader.
func TestEveryWiringSiteIsClassified(t *testing.T) {
	root := moduleRoot(t)
	found := discoverWiringSites(t, root)
	if len(found) == 0 {
		t.Fatal("no wiring sites discovered — the pattern is broken, and a guard that finds nothing passes forever")
	}

	claimed := map[string]bool{}
	for key := range found {
		parts := strings.SplitN(key, "\x00", 2)
		file, line := parts[0], parts[1]
		for _, e := range ledgerWiringRegistry {
			if e.file == file && strings.Contains(line, collapse(e.snippet)) {
				claimed[key] = true
				break
			}
		}
	}
	for key := range found {
		if claimed[key] {
			continue
		}
		parts := strings.SplitN(key, "\x00", 2)
		t.Errorf("unclassified ledger wiring in %s:\n    %s\n"+
			"Add it to ledgerWiringRegistry saying whether it BILLS (writes task_llm_usage "+
			"rows) or only READS, with a note. A component handed the usage repo without a "+
			"decision about its spend is the defect this guard exists for.", parts[0], parts[1])
	}
}

// TestMigratedComponentsLeaveTheRegistry is the self-deletion mechanism.
//
// Once a component takes an llmspend.Recorder at construction, the compiler
// enforces its billing and a text-match entry here is stale — worse than absent,
// because it reads as evidence somebody still checks this. So a file that imports
// internal/llmspend must not have registry entries pointing at it.
func TestMigratedComponentsLeaveTheRegistry(t *testing.T) {
	root := moduleRoot(t)
	for name, e := range ledgerWiringRegistry {
		b, err := os.ReadFile(filepath.Join(root, e.file))
		if err != nil {
			continue // reported by TestEveryBillingComponentIsWired
		}
		if strings.Contains(string(b), "internal/llmspend") {
			t.Errorf("%s: %s now imports internal/llmspend, so its recorder is compiler-enforced.\n"+
				"Delete this registry entry — slice C of the ledger-completeness design removes each "+
				"entry as its component migrates, and this file deletes itself once the registry is empty.",
				name, e.file)
		}
	}
}

// TestStopgapRetiresWhenAllBillersMigrate makes the stopgap demand its own
// removal — keyed on BILLERS, not on the registry being empty.
//
// The empty-registry version of this check was wrong, and running the migration
// is what exposed it: READERS (budget checks, cost monitors) never migrate to
// llmspend, because they never write a row. They would sit in the registry
// forever, the registry would never empty, and the guard would never retire —
// the exact "deletion someone will forget" outcome the self-deletion mechanism
// was designed to prevent.
//
// So the condition is: when no BILLER is left, the stopgap's job is done. The
// reader entries are worth keeping as documentation of who reads the ledger, but
// they are not a guard against anything the compiler cannot already see.
// TestNoBillerIsTextGuarded is what this guard BECAME once slice C finished.
//
// It began as a stopgap that asserted each billing component's wiring line still
// existed, and it deleted that half of itself the moment the last biller migrated
// — the retirement check fired on 2026-08-12 and this is the result.
//
// What remains is the stronger, permanent rule: a component that BILLS must go
// through llmspend.Recorder, where the compiler enforces it. If an entry here is
// ever marked bills:true again, someone has hand-rolled a ledger write against a
// raw repository, which is exactly how three components reached production
// unbilled.
//
// The reader entries stay, and the reverse check above (every wiring site must be
// classified) stays with them: a NEW component handed the usage repository still
// has to declare which job it is doing.
func TestNoBillerIsTextGuarded(t *testing.T) {
	for name, e := range ledgerWiringRegistry {
		if e.bills {
			t.Errorf("%s is classified as BILLING via a raw repository wiring (%s).\n"+
				"Billing must go through internal/llmspend.Recorder, which a component takes at "+
				"construction so the compiler asks the question. A hand-rolled "+
				"persistence.TaskLLMUsage write against the repo is the shape that reached "+
				"production unbilled three times.", name, e.file)
		}
	}
}

package pricing

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// 2026-09-17: a corrected pricing.yaml was deployed to both trees and
// `vornikctl config reload` answered cleanly on each — "Pending activation:
// false", no validation errors — while the OLD rates kept billing. The table
// was loaded once in NewContainer and the reload path never touched it. The
// arm that started three minutes later recorded $13.758 where the corrected
// table gives $1.817.
//
// The fix has to reach holders that captured the *Table pointer at
// construction (the judge runner and the chat catalogs have no setter at all),
// so the rates move INSIDE the table rather than the table being replaced.

func TestReplace_MovesRatesForAnExistingPointerHolder(t *testing.T) {
	table := mustLoadString(t, `
models:
  "m1": { input: 1.00, output: 3.00 }
default: { input: 9.99, output: 9.99 }
`)
	// A consumer that captured the pointer at construction, as the executor's
	// cost recorder and the judge runner do.
	holder := table

	got := holder.CostUSD("m1", 1_000_000, 0)
	if got != 1.00 {
		t.Fatalf("pre-swap cost: want 1.00, got %v", got)
	}

	next := mustLoadString(t, `
models:
  "m1": { input: 0.35, output: 2.75 }
default: { input: 9.99, output: 9.99 }
`)
	table.Replace(next)

	if got := holder.CostUSD("m1", 1_000_000, 0); got != 0.35 {
		t.Fatalf("the pointer holder still bills the old rate: want 0.35, got %v", got)
	}
}

// A model that DISAPPEARS from the table must fall back after the swap, not
// keep its old rate from a stale snapshot.
func TestReplace_DroppedModelFallsBack(t *testing.T) {
	table := mustLoadString(t, "models:\n  \"m1\": { input: 1.00, output: 3.00 }\ndefault: { input: 5.00, output: 5.00 }\n")
	table.Replace(mustLoadString(t, "models: {}\ndefault: { input: 5.00, output: 5.00 }\n"))

	if _, known := table.Lookup("m1"); known {
		t.Fatal("a model removed by the swap is still reported as known")
	}
	if got := table.CostUSD("m1", 1_000_000, 0); got != 5.00 {
		t.Fatalf("want the new default 5.00 after the drop, got %v", got)
	}
}

// The unknown-model warn cache is cleared by the swap: a model that has just
// become unpriced must warn again rather than stay silent on the strength of a
// warning issued against a different table.
func TestReplace_ClearsTheWarnCache(t *testing.T) {
	table := mustLoadString(t, "models:\n  \"m1\": { input: 1.00, output: 3.00 }\ndefault: { input: 5.00, output: 5.00 }\n")

	var warned []string
	table.SetWarnHook(func(m string) { warned = append(warned, m) })

	table.Lookup("unpriced")
	table.Lookup("unpriced") // deduped
	if len(warned) != 1 {
		t.Fatalf("want one warning before the swap, got %v", warned)
	}

	table.Replace(mustLoadString(t, "models: {}\ndefault: { input: 5.00, output: 5.00 }\n"))

	table.Lookup("unpriced")
	if len(warned) != 2 {
		t.Fatalf("the swap did not clear the warn cache: %v", warned)
	}
}

// The warn hook itself must SURVIVE the swap — it is wired once at container
// construction, and a swap that dropped it would silence every future
// unknown-model warning.
func TestReplace_KeepsTheWarnHook(t *testing.T) {
	table := mustLoadString(t, "models: {}\ndefault: { input: 1.00, output: 1.00 }\n")
	var count int
	table.SetWarnHook(func(string) { count++ })

	table.Replace(mustLoadString(t, "models: {}\ndefault: { input: 2.00, output: 2.00 }\n"))
	table.Lookup("still-unpriced")

	if count != 1 {
		t.Fatalf("the warn hook did not survive the swap (count=%d)", count)
	}
}

// Readers must not tear while a swap happens. Run under -race.
func TestReplace_ConcurrentReadersAndSwaps(t *testing.T) {
	table := mustLoadString(t, "models:\n  \"m1\": { input: 1.00, output: 1.00 }\ndefault: { input: 1.00, output: 1.00 }\n")
	next := mustLoadString(t, "models:\n  \"m1\": { input: 2.00, output: 2.00 }\ndefault: { input: 2.00, output: 2.00 }\n")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				// Every rate in both tables is 1.00 or 2.00, so any value
				// outside that set means a torn read.
				if got := table.CostUSD("m1", 1_000_000, 0); got != 1.00 && got != 2.00 {
					t.Errorf("torn read: %v", got)
					return
				}
				table.Lookup("unpriced")
				_ = table.IDs()
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				table.Replace(next)
				table.Replace(table)
			}
		}()
	}
	wg.Wait()
}

// Replace(nil) must be a no-op, not a way to silently zero every rate on a
// deployment: the caller that would pass nil is a reload whose parse failed,
// and the design's rule there is that the running rates keep serving.
func TestReplace_NilIsANoOp(t *testing.T) {
	table := mustLoadString(t, "models:\n  \"m1\": { input: 1.00, output: 1.00 }\ndefault: {}\n")
	table.Replace(nil)
	if got := table.CostUSD("m1", 1_000_000, 0); got != 1.00 {
		t.Fatalf("Replace(nil) changed the rates: %v", got)
	}
}

func mustLoadString(t *testing.T, yaml string) *Table {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pricing.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	table, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return table
}

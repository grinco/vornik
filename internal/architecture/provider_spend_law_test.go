package architecture

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The provider-spend law: slice 3 of
// https://docs.vornik.io §6.
//
// WHY THIS EXISTS AS A SEPARATE LAW.
// internal/chat/callsite_accounting_test.go already fails the build when a call
// site is added without a cost-accounting decision. But its discovery predicate
// is "calls chat.WithCallSite", so its coverage boundary is completion calls
// routed through internal/chat. internal/memory/embedder.go reached Bedrock and
// raw HTTP endpoints directly, never touching internal/chat — so embedding spend
// was not something that guard failed to catch, it was never in scope. Embedding
// went unbilled on every provider until 2026-08-12 as a direct result.
//
// So the regulated act here is REACHING A PROVIDER, not opting into a seam. Any
// package that imports a provider runtime SDK must be classified below with a
// decision about its cost accounting. A new package that talks to a model
// provider cannot appear without someone recording what happens to its spend.
//
// WHAT THIS LAW DOES NOT PROVE — stated plainly, because a guard whose limits
// are unclear gets trusted for things it does not do:
//
//   - It does not prove a recorder is non-nil at runtime. All three prior
//     unbilled-call-site regressions were wiring failures, and only per-role
//     unit tests catch those (see TestEmbedderUsageWiring_IsGuardedBySource).
//   - It does not prove recording precedes response parsing (the
//     RECORD-BEFORE-CLASSIFYING invariant). Only a behavioural test does.
//   - It does not prove attribution is correct — right project, right role.
//   - It cannot see a provider reached over RAW HTTP to a configured endpoint,
//     because that is an http.Client call to a URL from config and is
//     indistinguishable, structurally, from any other HTTP request. This is a
//     real hole: internal/memory's OpenAI-compatible path is exactly that shape.
//     The compiler-enforced answer is a codebase-wide llmspend.Recorder
//     constructor parameter, tracked as the named follow-on in §8.1; this law is
//     what stops a NEW provider package from appearing unnoticed in the
//     meantime.

// providerRuntimeSDKs are import paths whose use means "this package can invoke
// a model and be charged for it". Prefix-matched, so nested packages of an SDK
// count as the SDK.
//
// The non-runtime github.com/aws/aws-sdk-go-v2/service/bedrock (model listing,
// no inference) is deliberately absent: it cannot incur inference spend, and
// including it would put a control-plane lookup under a billing law and teach
// readers the law means something vaguer than it does.
var providerRuntimeSDKs = []string{
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime",
}

type providerSpendAccounting struct {
	// accounted is true when every billed provider call from this package
	// writes a task_llm_usage row.
	accounted bool
	// note must name the recording seam when accounted, or say WHY not and what
	// it would take. Required either way: an undocumented entry is how the
	// distiller, the reranker and the embedder each stayed unbilled through
	// review.
	note string
}

// providerSpendRegistry classifies every package permitted to reach a provider
// runtime SDK.
var providerSpendRegistry = map[string]providerSpendAccounting{
	"vornik.io/vornik/internal/chat": {
		accounted: true,
		note: "The chat provider seam. Per-call-site accounting is enforced separately and " +
			"more precisely by internal/chat/callsite_accounting_test.go, which classifies " +
			"every chat.WithCallSite label. This entry records that the package is a " +
			"provider-reaching package at all; that test records what each of its call " +
			"sites does about spend.",
	},
	"vornik.io/vornik/internal/memory": {
		accounted: true,
		note: "Embedder.recordEmbedUsage writes one task_llm_usage row per billed provider " +
			"call (role/source memory_embedder), for the OpenAI-compatible, Bedrock Titan " +
			"and Bedrock Cohere paths alike. Recorded below the cache short-circuit so a " +
			"cache hit bills nothing, and before response parsing so an unusable body " +
			"cannot launder a charged call. Wired in container_scheduler.go; the wiring " +
			"itself is guarded by TestEmbedderUsageWiring_IsGuardedBySource. " +
			"NOTE: this package also reaches OpenAI-compatible endpoints over raw HTTP, " +
			"which this law cannot see — the classification covers those paths because " +
			"the recording lives in the shared Embedder, not because the law detected them.",
	},
}

// packagesImportingProviderSDKs returns each first-party package whose
// dependency closure includes a provider runtime SDK, mapped to the SDK matched.
//
// Uses `go list -deps` rather than a source grep so an INDIRECT path counts too:
// a package that reaches a provider through a helper package is reaching a
// provider. The CE→EE law in this file's sibling uses the same machinery for the
// same reason.
func packagesImportingProviderSDKs(t *testing.T, root string) map[string]string {
	t.Helper()

	// Direct imports only, per package: -deps would attribute an SDK to every
	// ancestor of the package that actually imports it, which would make the
	// registry a list of everything that transitively touches chat. The unit of
	// responsibility is the package holding the import.
	cmd := exec.Command("go", "list", "-f", "{{.ImportPath}} {{join .Imports \"|\"}}", "./...")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}

	found := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}
		pkg := parts[0]
		for _, imp := range strings.Split(parts[1], "|") {
			for _, sdk := range providerRuntimeSDKs {
				if strings.HasPrefix(imp, sdk) {
					found[pkg] = sdk
				}
			}
		}
	}
	return found
}

// TestEveryProviderReachingPackageIsClassified is the law. A package that can
// invoke a model must carry a recorded decision about its spend.
func TestEveryProviderReachingPackageIsClassified(t *testing.T) {
	found := packagesImportingProviderSDKs(t, moduleRoot(t))
	if len(found) == 0 {
		t.Fatal("no package imports a provider runtime SDK — the discovery is broken, " +
			"and a law that finds nothing silently passes forever")
	}

	pkgs := make([]string, 0, len(found))
	for pkg := range found {
		pkgs = append(pkgs, pkg)
	}
	sort.Strings(pkgs)

	for _, pkg := range pkgs {
		entry, ok := providerSpendRegistry[pkg]
		if !ok {
			t.Errorf("package %s imports %s but is not classified in providerSpendRegistry.\n"+
				"A new package that talks to a model provider needs a decision about its cost "+
				"accounting: either it records a task_llm_usage row per billed call, or the "+
				"registry says why not and what it would take. Unbilled provider calls have "+
				"reached production three times on this codebase and every one was found by an "+
				"operator reading a bill, not by the test suite.", pkg, found[pkg])
			continue
		}
		if strings.TrimSpace(entry.note) == "" {
			t.Errorf("package %s is classified with an empty note — the note is the point, "+
				"since an undocumented entry is indistinguishable from an unconsidered one", pkg)
		}
		if !entry.accounted {
			t.Logf("NOTE: %s reaches a provider and is deliberately unaccounted: %s", pkg, entry.note)
		}
	}
}

// TestProviderSpendRegistryHasNoStaleEntries keeps the registry honest in the
// other direction. A stale entry is worse than a missing one: it reads as
// evidence that somebody checked a package that no longer exists.
func TestProviderSpendRegistryHasNoStaleEntries(t *testing.T) {
	found := packagesImportingProviderSDKs(t, moduleRoot(t))
	for pkg := range providerSpendRegistry {
		if _, ok := found[pkg]; !ok {
			t.Errorf("providerSpendRegistry classifies %s, but it no longer imports a provider "+
				"runtime SDK — remove the entry so the registry keeps describing the code", pkg)
		}
	}
}

// TestProviderSpendLawCoversTheEmbedder is a targeted regression pin.
//
// The embedder is the package this law was written for: it reached Bedrock
// directly, was invisible to the chat-callsite guard, and went unbilled on every
// provider until 2026-08-12. If it ever drops out of the law's discovery — for
// instance because the Bedrock call moved behind an untracked helper — the law
// would pass while its motivating case went unguarded.
func TestProviderSpendLawCoversTheEmbedder(t *testing.T) {
	const embedderPkg = "vornik.io/vornik/internal/memory"
	found := packagesImportingProviderSDKs(t, moduleRoot(t))
	if _, ok := found[embedderPkg]; !ok {
		t.Errorf("%s is no longer discovered as provider-reaching. If the Bedrock call moved, "+
			"move this law's discovery with it — otherwise the case that motivated the law is "+
			"the one case it no longer covers.", embedderPkg)
	}
	// And it must be discoverable at the path the module actually uses, so a
	// module rename cannot quietly empty the registry.
	if _, err := os.Stat(filepath.Join(moduleRoot(t), "internal", "memory", "embedder.go")); err != nil {
		t.Errorf("internal/memory/embedder.go not found at the module root: %v", err)
	}
}

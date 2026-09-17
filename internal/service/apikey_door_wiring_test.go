package service

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/storage"
)

// TestAPIKeyAuthOptions_CarriesTheDoorWheneverAccountsExist — the §5.0
// disabled-owner door was implemented in internal/auth and in internal/authz
// and joined on NO auth chain: the primary router's, the /ui subtree's and
// the callback subtree's key lookups each authenticated a disabled owner's
// key. Three chains, one omission each.
func TestAPIKeyAuthOptions_CarriesTheDoorWheneverAccountsExist(t *testing.T) {
	// Built the way production builds it — through the accessor, from the
	// repositories — rather than by assigning the field a test controls.
	c := &Container{repos: &storage.Repositories{Identity: doorWiringIdentityStub{}}}
	cfg := api.BuildAuthConfig(nil, c.apiKeyAuthOptions(nil)...)
	if cfg.OwnerAccess == nil {
		t.Fatal("a container WITH an accounts service produced key-auth options with no disabled-owner door")
	}

	bare := &Container{}
	if api.BuildAuthConfig(nil, bare.apiKeyAuthOptions(nil)...).OwnerAccess != nil {
		t.Error("a container with no accounts service must not claim a door it cannot enforce")
	}
}

// TestAPIKeyLookup_IsOnlyReachableThroughTheBundle is a source contract, in
// the style of this repo's other reachability lints. The bundle only prevents
// the recurrence while it is the ONLY way to obtain a key lookup; a future
// hand-assembled chain would reopen all three holes silently, and no
// behavioural test can see a chain that does not exist yet.
//
// It walks ALL of internal/. Round 2 found the original scanned only
// internal/service while serverAuthOptions assembles the primary router's
// lookup in internal/api — the second-site shape §5.0 spent F4-2 closing.
// Widening to two packages made the claim true of two packages and unverified
// everywhere else, which round 3 flagged in turn: a third assembler somewhere
// unscanned is exactly what this exists to catch. Two assemblers are allowed,
// each only in its own package, and BOTH must append the door.
func TestAPIKeyLookup_IsOnlyReachableThroughTheBundle(t *testing.T) {
	// dir → the one function in it allowed to assemble a key lookup.
	// function name → the package directory it is allowed to live in.
	assemblers := map[string]string{
		"apiKeyAuthOptions": "service",
		"serverAuthOptions": "api",
	}
	call := regexp.MustCompile(`\bWithAPIKeyLookup\(`)
	door := regexp.MustCompile(`\bWithOwnerAccessCheck\(`)

	// Walk everything under internal/. Scoping to the two packages that
	// happen to assemble today would make the claim true of those two and
	// unverified everywhere else — and a third assembler somewhere else is
	// exactly what this exists to catch (review-20260915-b945, suggestion 3).
	err := filepath.Walk("..", func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, fn := range splitGoFuncs(string(src)) {
			if !call.MatchString(stripComments(fn.body)) {
				continue
			}
			wantPkg, allowed := assemblers[fn.name]
			if !allowed {
				t.Errorf("%s: func %s assembles an API-key auth chain by hand; only %v may, "+
					"so the §5.0 disabled-owner door cannot be omitted",
					path, fn.name, assemblerNames(assemblers))
				continue
			}
			if pkg := filepath.Base(filepath.Dir(path)); pkg != wantPkg {
				t.Errorf("%s: func %s is the named assembler for internal/%s and appeared in "+
					"internal/%s; two functions with one name defeats the attribution",
					path, fn.name, wantPkg, pkg)
			}
			// The permitted assembler must ALSO hang the door. Matched over
			// the function BODY rather than a line substring: an argument
			// rename must not silently turn the check off.
			if !door.MatchString(fn.body) {
				t.Errorf("%s: %s takes a key lookup without appending the disabled-owner door",
					path, fn.name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
}

// assemblerNames renders the allowlist for a failure message.
func assemblerNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// goFunc is one top-level function, split crudely on column-0 "func " —
// enough to attribute a call to its enclosing function without pulling in a
// parser, and it fails loudly (wrong function name) rather than silently if
// the split is wrong.
type goFunc struct{ name, body string }

func splitGoFuncs(src string) []goFunc {
	var out []goFunc
	decl := regexp.MustCompile(`^func (?:\([^)]*\) )?([A-Za-z0-9_]+)`)
	var cur *goFunc
	for _, line := range strings.Split(src, "\n") {
		if m := decl.FindStringSubmatch(line); m != nil {
			out = append(out, goFunc{name: m[1]})
			cur = &out[len(out)-1]
			continue
		}
		if cur != nil {
			cur.body += line + "\n"
		}
	}
	return out
}

// TestAccountsService_DoesNotDependOnInitOrder — initTelegram runs BEFORE
// initHTTPServer. While the accounts service was assigned to a field during
// HTTP init, anything wired earlier read nil: the Telegram bot's §5.2
// link-code redeemer was wired off that field and was therefore never wired
// at all, so a code issued in the UI could be typed into chat and answered
// "not valid" forever.
//
// That is the third appearance of one shape in this feature — built, and not
// hung. The accessor has no order to get wrong; this pins that it is used.
func TestAccountsService_DoesNotDependOnInitOrder(t *testing.T) {
	c := &Container{repos: &storage.Repositories{Identity: doorWiringIdentityStub{}}}

	// The EARLY caller (the bot's position) must get a service.
	first := c.accountsService()
	if first == nil {
		t.Fatal("the first caller got no accounts service: whatever is wired earliest in init is wired to nothing")
	}
	// And the later caller must get the SAME one — two services would mean
	// two audit sinks and two link-code stores for one deployment.
	if second := c.accountsService(); second != first {
		t.Error("accountsService returned a second instance; the deployment would have two account services")
	}

	if (&Container{}).accountsService() != nil {
		t.Error("a container with no identity repository must not claim an accounts service")
	}
}

// TestAccountsField_IsOnlyReadThroughTheAccessor is the source contract that
// keeps the fix above true. A future call site reading c.accounts directly
// reintroduces the order dependence silently, and no behavioural test can see
// a call site that does not exist yet.
func TestAccountsField_IsOnlyReadThroughTheAccessor(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	read := regexp.MustCompile(`c\.accounts\b`)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", name, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if !read.MatchString(line) || strings.Contains(line, "c.accountsOnce") {
				continue
			}
			if strings.Contains(line, "return c.accounts") {
				continue // the accessor itself
			}
			t.Errorf("%s:%d reads c.accounts directly; use (*Container).accountsService() "+
				"so the wiring does not depend on init order:\n\t%s", name, i+1, strings.TrimSpace(line))
		}
	}
}

// doorWiringIdentityStub is a SHAPE-ONLY stub: these tests assert what the
// option list and the accessor produce, never what a lookup returns, so no
// method on it is driven. The embedded interface is nil — calling anything
// not overridden here panics rather than answering wrongly, which is the
// behaviour a shape-only double should have (review-20260914-e581, minor).
type doorWiringIdentityStub struct{ persistence.IdentityRepository }

var _ persistence.IdentityRepository = doorWiringIdentityStub{}

func (doorWiringIdentityStub) ResolvePrincipal(context.Context, string, string) ([]persistence.PrincipalRow, error) {
	return nil, persistence.ErrNotFound
}

// stripComments removes whole-line // comments so a function that merely
// MENTIONS the call in prose is not read as making it. Two limits, stated
// rather than implied — the previous version of this contract was believed
// because its comment claimed more than its regex delivered:
//
//   - a trailing comment on a code line is not removed (the code on that line
//     is what we want to see anyway);
//   - a caller that binds the symbol to a variable first
//     (fn := api.WithAPIKeyLookup; fn(repo)) evades detection entirely.
//
// The second is a real hole. It is left open because closing it needs a type
// checker rather than a scanner, and the guard's purpose is to catch the
// ACCIDENTAL second assembler — which is what actually happened, three times.
func stripComments(body string) string {
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

package dispatcher

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestLinkRowsAreOnlyWrittenThroughTheMergePath — the account-always-wins rule
// lives in chooseWinner, and it protects nothing if another writer can repoint
// an `operator_identity_link` row without passing through it.
//
// That rule exists because a chat-to-chat /link could otherwise demote the
// account a speaker is bound to: content volume settles a tie between peers
// and must never settle authority. A direct Upsert elsewhere — an admin
// repoint, a backfill, a future "move this speaker" verb — would bypass it
// silently, and no behavioural test can see a writer that does not exist yet.
//
// This is the shape that caught the three built-but-not-hung defects in this
// feature: make the omission unavailable rather than merely discouraged.
func TestLinkRowsAreOnlyWrittenThroughTheMergePath(t *testing.T) {
	// The only functions permitted to write a link row, and where they live.
	allowed := map[string]string{
		"repointLoserLinks": "dispatcher", // moves a loser's rows onto the winner
		"pointAt":           "dispatcher", // writes one row, for the three merge call sites
	}
	// An Upsert on a LINK repository, however the argument is spelled.
	//
	// The first version matched only an inline &persistence.OperatorIdentityLink
	// literal or a variable literally named `row`, so a writer that said
	// `link := &persistence.OperatorIdentityLink{…}; repos.Links.Upsert(ctx, link)`
	// passed silently (review-20260915-1ea8). The contract claimed to make the
	// omission unavailable and made two SPELLINGS unavailable — the same gap,
	// one level up, as the guards it was written to imitate.
	//
	// It keys on the RECEIVER rather than the argument, so the argument may be
	// named anything, and the receiver may be named anything ENDING in
	// link/links — opLinks, operatorLinks, idLink. The first receiver-based
	// version required the name to be exactly `link` or `links`, so
	// `opLinks.Upsert(ctx, x)` evaded with no aliasing at all
	// (review-20260915-ba13 F4): the fix for one narrow match had simply
	// moved the narrowness to the other side of the dot.
	//
	// The residual, stated rather than implied: a writer that binds the
	// repository to a name containing no "link" at all — `r := repos.Links;
	// r.Upsert(…)` — still evades, because closing that needs a type checker
	// rather than a scanner. "Unavailable" would be the wrong word for what
	// this delivers, and using the wrong word is how the first version got
	// believed.
	write := regexp.MustCompile(`(?i)[A-Za-z]*links?\.Upsert\(ctx,`)

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
		// internal/persistence/repotest drives repositories directly against
		// their contract — that is its entire job, and a suite that could not
		// call Upsert could not pin what Upsert does.
		if strings.Contains(filepath.ToSlash(path), "/persistence/repotest/") {
			return nil
		}
		// The file must name the link type somewhere, which keeps the receiver
		// pattern from matching an unrelated `links` variable.
		if !strings.Contains(string(src), "OperatorIdentityLink") {
			return nil
		}
		for _, fn := range splitDispatcherFuncs(string(src)) {
			if !write.MatchString(fn.body) {
				continue
			}
			wantPkg, ok := allowed[fn.name]
			if !ok {
				t.Errorf("%s: func %s writes an operator_identity_link row directly; "+
					"route it through the merge path so chooseWinner's account-always-wins "+
					"rule cannot be bypassed", path, fn.name)
				continue
			}
			if pkg := filepath.Base(filepath.Dir(path)); pkg != wantPkg {
				t.Errorf("%s: func %s is the named writer for internal/%s and appeared in internal/%s",
					path, fn.name, wantPkg, pkg)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
}

// splitDispatcherFuncs mirrors the splitter used by the door contract in
// internal/service: crude column-0 "func " splitting, enough to attribute a
// call to its enclosing function without a parser, and loud rather than
// silent when the attribution is wrong.
type dispatcherFunc struct{ name, body string }

func splitDispatcherFuncs(src string) []dispatcherFunc {
	var out []dispatcherFunc
	decl := regexp.MustCompile(`^func (?:\([^)]*\) )?([A-Za-z0-9_]+)`)
	var cur *dispatcherFunc
	for _, line := range strings.Split(src, "\n") {
		if m := decl.FindStringSubmatch(line); m != nil {
			out = append(out, dispatcherFunc{name: m[1]})
			cur = &out[len(out)-1]
			continue
		}
		if cur != nil {
			cur.body += line + "\n"
		}
	}
	return out
}

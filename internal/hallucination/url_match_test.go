package hallucination

import "testing"

// Regression: task_20260823100022_bd4345a67e59cebe, project `assistant`,
// step `research` of exec_20260823100023_39a0fc9b3beb1e7d (2026-08-23).
//
// The agent fetched a Google Calendar iCal feed, then cited it in prose with
// the private-token path segment elided to /.../ and %40 percent-decoded to @.
// matchesFetchedURL compared byte-for-byte with a prefix-either-direction
// escape, so neither form matched, url_not_fetched fired at SeverityHigh, and
// a wholly correct daily briefing was thrown away.
//
// The cost was not the wasted step: the lead's recovery checkpoint restated the
// detector's verdict as established fact, the operator answered that checkpoint
// on a false premise, and the whole digest re-ran ~2.5h later for zero defect.
//
// See https://docs.vornik.io §4a.
const (
	icalFetched = "https://calendar.google.com/calendar/ical/vadim%40grinco.eu/private-0123456789abcdef0123456789abcdef/basic.ics"
	icalClaimed = "https://calendar.google.com/calendar/ical/vadim@grinco.eu/.../basic.ics"
)

func fetchedSet(urls ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(urls))
	for _, u := range urls {
		out[normalizeURLForMatch(u)] = struct{}{}
	}
	return out
}

func TestICalIncidentGrounds(t *testing.T) {
	if !matchesFetchedURL(normalizeURLForMatch(icalClaimed), fetchedSet(icalFetched)) {
		t.Fatal("the 2026-08-23 incident pair must ground: the agent DID fetch this feed, " +
			"it merely redacted the token and decoded %40 when citing it")
	}
}

// Each break in isolation, so a partial fix cannot pass the test above by
// accident.
func TestPercentDecodingAloneGrounds(t *testing.T) {
	fetched := fetchedSet("https://example.com/cal/vadim%40grinco.eu/basic.ics")
	if !matchesFetchedURL(normalizeURLForMatch("https://example.com/cal/vadim@grinco.eu/basic.ics"), fetched) {
		t.Fatal("a percent-decoded spelling of the same URL must ground")
	}
}

func TestMidPathElisionAloneGrounds(t *testing.T) {
	fetched := fetchedSet("https://example.com/a/secret-token-here/b/basic.ics")
	for _, marker := range []string{"...", "…", "<redacted>", "***", "*"} {
		claim := "https://example.com/a/" + marker + "/b/basic.ics"
		if !matchesFetchedURL(normalizeURLForMatch(claim), fetched) {
			t.Errorf("elision marker %q in mid-path must act as a wildcard", marker)
		}
	}
}

// An elision must be able to swallow MORE THAN ONE segment — the agent does
// not know or care how many segments it hid.
func TestElisionSpansMultipleSegments(t *testing.T) {
	fetched := fetchedSet("https://example.com/a/one/two/three/basic.ics")
	if !matchesFetchedURL(normalizeURLForMatch("https://example.com/a/.../basic.ics"), fetched) {
		t.Fatal("an elision marker must match one OR MORE consecutive segments")
	}
}

// --- The rule must keep working. These are the cases it exists for. ---

// A fabricated HOST must never ground. This is the rule's primary job.
func TestFabricatedHostDoesNotGround(t *testing.T) {
	fetched := fetchedSet("https://real.example.com/article/123")
	for _, claim := range []string{
		"https://invented.example.com/article/123",
		"https://real.example.com.evil.test/article/123",
		"https://realexample.com/article/123",
	} {
		if matchesFetchedURL(normalizeURLForMatch(claim), fetched) {
			t.Errorf("fabricated host %q must NOT ground", claim)
		}
	}
}

// The deviation from the filed fix direction, asserted. The backlog proposed
// matching on scheme+host+leading path segment; that would ground an invented
// deep path on a real host, which is exactly what the rule is for. Precision is
// retained wherever the model did not actually elide anything.
func TestFabricatedDeepPathOnARealHostDoesNotGround(t *testing.T) {
	fetched := fetchedSet("https://news.example.com/article/123")
	if matchesFetchedURL(normalizeURLForMatch("https://news.example.com/article/999-invented-slug"), fetched) {
		t.Fatal("an invented deep path on a fetched host must NOT ground — " +
			"blanket first-segment matching was declined for this reason (LLD §4a)")
	}
}

// A scheme change is a different resource.
func TestSchemeMustMatch(t *testing.T) {
	fetched := fetchedSet("https://example.com/a/b")
	if matchesFetchedURL(normalizeURLForMatch("http://example.com/a/b"), fetched) {
		t.Fatal("http:// must not ground against an https:// fetch")
	}
}

// An elision must not become a universal wildcard that grounds anything on a
// host the agent happened to touch.
func TestElisionDoesNotGroundADifferentPathShape(t *testing.T) {
	fetched := fetchedSet("https://example.com/calendar/ical/token/basic.ics")
	for _, claim := range []string{
		"https://example.com/admin/.../secrets.json",
		"https://example.com/calendar/.../evil.exe",
	} {
		if matchesFetchedURL(normalizeURLForMatch(claim), fetched) {
			t.Errorf("%q must not ground: the non-elided segments do not match", claim)
		}
	}
}

// A bare elision covering the WHOLE path would ground any URL on the host.
// That is too much power for a marker the model chooses to emit.
func TestWholePathElisionDoesNotGroundAnything(t *testing.T) {
	fetched := fetchedSet("https://example.com/calendar/ical/token/basic.ics")
	if matchesFetchedURL(normalizeURLForMatch("https://example.com/..."), fetched) {
		t.Fatal("an elision covering the entire path must not ground an arbitrary claim")
	}
}

// The pre-existing prefix escape must survive: "quoted the collection, fetched
// the item" is the case the original comment describes.
func TestPrefixEscapeStillWorks(t *testing.T) {
	fetched := fetchedSet("https://example.com/jobs/12345")
	if !matchesFetchedURL(normalizeURLForMatch("https://example.com/jobs"), fetched) {
		t.Fatal("the collection/item prefix case must still ground")
	}
	fetched2 := fetchedSet("https://example.com/jobs")
	if !matchesFetchedURL(normalizeURLForMatch("https://example.com/jobs/12345"), fetched2) {
		t.Fatal("the reverse prefix case must still ground")
	}
}

// Cosmetic normalisation the original code already promised in its doc comment.
func TestCosmeticNormalisation(t *testing.T) {
	fetched := fetchedSet("https://example.com/a/")
	for _, claim := range []string{
		"https://Example.com/a",
		"https://example.com/a#section",
		"https://www.example.com/a",
	} {
		if !matchesFetchedURL(normalizeURLForMatch(claim), fetched) {
			t.Errorf("%q should ground against https://example.com/a/", claim)
		}
	}
}

func TestEmptyAndMalformedAreSafe(t *testing.T) {
	fetched := fetchedSet("https://example.com/a")
	for _, claim := range []string{"", "not a url", "https://", "://x"} {
		// Must not panic, and must not ground.
		if matchesFetchedURL(normalizeURLForMatch(claim), fetched) {
			t.Errorf("%q must not ground", claim)
		}
	}
	// An empty fetched set grounds nothing.
	if matchesFetchedURL(normalizeURLForMatch("https://example.com/a"), map[string]struct{}{}) {
		t.Error("an empty fetched set must ground nothing")
	}
}

// A malformed percent-escape must not throw away the URL.
func TestInvalidPercentEscapeDegradesGracefully(t *testing.T) {
	fetched := fetchedSet("https://example.com/a%zz/b")
	if !matchesFetchedURL(normalizeURLForMatch("https://example.com/a%zz/b"), fetched) {
		t.Fatal("an undecodable escape must still match itself")
	}
}

// Two holes in the ORIGINAL raw-string prefix escape, reachable once the rest
// of the matching became more forgiving. Both are host-confusion bugs, which is
// the one thing this rule must never get wrong.
func TestLookalikeHostDoesNotGroundViaPrefix(t *testing.T) {
	// Raw string prefix: "https://real.example.com" IS a prefix of
	// "https://real.example.com.evil.test/x".
	fetched := fetchedSet("https://real.example.com")
	if matchesFetchedURL(normalizeURLForMatch("https://real.example.com.evil.test/x"), fetched) {
		t.Fatal("a lookalike host must not ground by string prefix")
	}
}

func TestDegenerateClaimDoesNotGroundEverything(t *testing.T) {
	fetched := fetchedSet("https://example.com/a/b", "https://other.test/c")
	for _, claim := range []string{"https://", "https://example.com"} {
		if claim == "https://example.com" {
			// This one SHOULD ground — it is the real host, and the
			// collection/item escape covers it.
			continue
		}
		if matchesFetchedURL(normalizeURLForMatch(claim), fetched) {
			t.Errorf("%q must not ground every fetched URL", claim)
		}
	}
}

// Segment-wise prefix, not byte-wise: /jobs must not ground /jobsearch.
func TestPrefixIsSegmentWise(t *testing.T) {
	fetched := fetchedSet("https://example.com/jobsearch/1")
	if matchesFetchedURL(normalizeURLForMatch("https://example.com/jobs"), fetched) {
		t.Fatal("/jobs must not ground /jobsearch — different page")
	}
}

// Boundary conditions for the segment wildcard (companion review
// review-20260826-0d3c, suggestion 1). Each pins a decision that is easy to
// get wrong and invisible until it grounds something it shouldn't.
func TestElisionBoundaryPositions(t *testing.T) {
	fetched := fetchedSet("https://example.com/a/b/c/d.ics")

	grounds := []struct{ claim, why string }{
		{"https://example.com/.../b/c/d.ics", "leading elision consumes the first segment"},
		{"https://example.com/a/.../d.ics", "middle elision consumes b and c"},
		{"https://example.com/a/...", "trailing elision consumes the rest of the path"},
		{"https://example.com/.../.../c/d.ics", "back-to-back elisions each consume >=1"},
		{"https://example.com/a/.../c/...", "two elisions in different positions"},
	}
	for _, g := range grounds {
		if !matchesFetchedURL(normalizeURLForMatch(g.claim), fetched) {
			t.Errorf("%q should ground (%s)", g.claim, g.why)
		}
	}

	rejects := []struct{ claim, why string }{
		// An elision means the model REMOVED something, so it must consume at
		// least one segment. Matching zero would ground a path that was never
		// fetched.
		{"https://example.com/a/b/.../c/d.ics", "elision cannot match zero segments"},
		// The literal segments still have to line up, in order.
		{"https://example.com/.../b/c/WRONG.ics", "terminal segment must match"},
		{"https://example.com/.../c/b/d.ics", "segment order must be preserved"},
		{"https://example.com/z/.../d.ics", "a wrong leading literal is still wrong"},
	}
	for _, r := range rejects {
		if matchesFetchedURL(normalizeURLForMatch(r.claim), fetched) {
			t.Errorf("%q must NOT ground (%s)", r.claim, r.why)
		}
	}
}

// An elision must not let a claim reach a DIFFERENT host, no matter where it
// sits. Host confusion is the one failure this rule cannot be allowed.
func TestElisionNeverCrossesHosts(t *testing.T) {
	fetched := fetchedSet("https://real.example.com/a/b")
	for _, claim := range []string{
		"https://evil.test/.../b",
		"https://.../a/b",
		"https://real.example.com.evil.test/.../b",
	} {
		if matchesFetchedURL(normalizeURLForMatch(claim), fetched) {
			t.Errorf("%q must not ground across hosts", claim)
		}
	}
}

// A query string CAN change which resource was served, so it is kept in the
// normalised form rather than discarded.
func TestQueryStringIsSignificant(t *testing.T) {
	fetched := fetchedSet("https://example.com/search?q=alpha")
	if !matchesFetchedURL(normalizeURLForMatch("https://example.com/search?q=alpha"), fetched) {
		t.Fatal("an identical query must ground")
	}
}

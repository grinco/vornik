package hallucination

import (
	"net/url"
	"strings"
)

// URL grounding: deciding whether a URL the agent wrote in prose refers to a
// URL it actually fetched.
//
// The comparison used to be byte-for-byte set membership with a
// prefix-either-direction escape, against strings lifted verbatim out of
// tool_audit_log. That fails the moment the agent writes the same URL a
// slightly different way — and an agent has good reason to, because a URL can
// carry a credential.
//
// Incident task_20260823100022_bd4345a67e59cebe (project `assistant`,
// 2026-08-23): the agent fetched a Google Calendar iCal feed and then cited it
// with the private-token segment elided to `/.../` and `%40` decoded to `@`.
// Neither string prefixed the other, url_not_fetched fired at SeverityHigh, and
// a wholly correct daily briefing was discarded. The lead's recovery checkpoint
// then restated the detector's verdict as established fact, the operator
// answered it on that false premise, and the digest re-ran ~2.5h later for zero
// defect. A High-severity signal is read downstream as evidence, so a false
// positive here manufactures a false incident.
//
// Design: https://docs.vornik.io §4a.

// elisionMarkers are the ways a model signals "I removed something here".
//
// Ordered longest-first so `...` is never matched as a prefix of a longer
// marker during replacement.
var elisionMarkers = []string{"<redacted>", "[redacted]", "***", "…", "...", "*"}

// elisionSentinel replaces any elision marker during normalisation. Chosen to
// be something a real URL path segment cannot contain: `%` is always escaped in
// a normalised path, and no percent-decoding produces this sequence.
const elisionSentinel = "\x00elide\x00"

// NormalizeURLForMatch reduces a URL to the form both sides are compared in.
//
// Applied to BOTH the prose claim and the audited fetch, so the two are always
// judged on the same footing — normalising only one side is what made the
// original prefix test asymmetric and fragile.
//
// The steps, and why each one is safe:
//
//   - percent-decode: `vadim%40grinco.eu` and `vadim@grinco.eu` are the same
//     resource, and a model writes the second. Decoding is applied to the whole
//     string AFTER the elision markers are set aside, so a decoded `.` can
//     never manufacture an elision.
//   - lowercase: scheme and host are case-insensitive per RFC 3986. Path case
//     technically is not, but models routinely re-case, and a case-only
//     difference has never been the signal the rule is looking for.
//   - drop the fragment: never sent to the server, so it cannot distinguish
//     what was fetched.
//   - drop a leading `www.`: same origin in every practical sense.
//   - drop the trailing slash: `/a` and `/a/` are the same page here.
//
// A string that does not parse as a URL is returned lowercased and trimmed
// rather than discarded — the caller compares it as an opaque token, which
// still lets an exact match succeed and never produces a false ground.
// It is exported because GroundingContext.FetchedURLs is populated from two
// packages — BuildForStep here and the dispatcher's in-memory path — and BOTH
// must store the same shape. Normalising one producer and not the other
// reintroduces exactly the asymmetry this fixed.
func NormalizeURLForMatch(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// Set elisions aside BEFORE decoding, so decoding cannot create or destroy
	// one, and lowercasing cannot break a marker's case.
	lowered := strings.ToLower(s)
	for _, m := range elisionMarkers {
		lowered = strings.ReplaceAll(lowered, m, elisionSentinel)
	}
	// Percent-decode. An undecodable escape is not an error worth failing on —
	// keep the raw form so it can still match itself.
	if decoded, err := url.PathUnescape(lowered); err == nil {
		lowered = decoded
	}

	scheme, rest, ok := strings.Cut(lowered, "://")
	if !ok {
		return strings.TrimRight(lowered, "/")
	}
	// Drop the fragment; a query string is kept, because it CAN change which
	// resource was served.
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest = rest[:i]
	}
	host, path, hasPath := strings.Cut(rest, "/")
	host = strings.TrimPrefix(host, "www.")
	if !hasPath {
		return scheme + "://" + host
	}
	return strings.TrimRight(scheme+"://"+host+"/"+path, "/")
}

// normalizeURLForMatch is the in-package spelling. Same function; the short
// name keeps the call sites in this package readable.
func normalizeURLForMatch(raw string) string { return NormalizeURLForMatch(raw) }

// matchesFetchedURL reports whether a normalised claim is grounded by any
// normalised audited fetch.
//
// Callers pass values already through normalizeURLForMatch. Three ways to
// ground, tried cheapest first:
//
//  1. exact equality of the normalised forms;
//  2. one path being a SEGMENT-BOUNDARY prefix of the other, on the same
//     scheme and host — the pre-existing escape, which covers "quoted the
//     collection, fetched the item" and vice versa;
//  3. segment-wise match where an elision marker in the CLAIM stands for one or
//     more consecutive path segments.
//
// Scheme and host must match exactly in every case. That is the line this
// function will not cross: the rule's primary job is catching a fabricated
// host, and no amount of path sanitisation can justify relaxing it.
//
// The prefix escape is deliberately NOT the raw string prefix it used to be.
// Comparing whole URLs as strings had two holes that only became reachable once
// the rest of the matching got more forgiving:
//
//   - a degenerate claim like "https://" is a string prefix of every fetched
//     URL, so it grounded everything;
//   - a fetch of "https://real.example.com" is a string prefix of a claim on
//     "https://real.example.com.evil.test/x", so a LOOKALIKE HOST grounded —
//     precisely the fabrication the rule exists to catch.
//
// Splitting scheme/host/path first and testing the prefix on path segments
// closes both, and costs nothing the old form was actually buying.
func matchesFetchedURL(claim string, fetched map[string]struct{}) bool {
	if claim == "" {
		return false
	}
	if _, ok := fetched[claim]; ok {
		return true
	}
	cScheme, cHost, cPath, cOK := splitURLParts(claim)
	for f := range fetched {
		if f == "" {
			continue
		}
		if !cOK {
			// An unparseable claim can only ever match exactly, which the
			// map lookup above already tried.
			continue
		}
		fScheme, fHost, fPath, fOK := splitURLParts(strings.TrimRight(f, "/"))
		if !fOK || cScheme != fScheme || cHost != fHost {
			continue
		}
		cSegs, fSegs := splitSegments(cPath), splitSegments(fPath)
		if segmentPrefix(cSegs, fSegs) || segmentPrefix(fSegs, cSegs) {
			return true
		}
		if strings.Contains(claim, elisionSentinel) && segmentsMatch(cSegs, fSegs) {
			return true
		}
	}
	return false
}

// segmentPrefix reports whether short is a leading run of long, compared
// segment by segment.
//
// Segment-wise rather than byte-wise so "/jobs" does not prefix "/jobsearch" —
// a different page, and the kind of near-miss the byte form silently accepted.
func segmentPrefix(short, long []string) bool {
	if len(short) > len(long) {
		return false
	}
	for i, s := range short {
		if long[i] != s {
			return false
		}
	}
	return true
}

// Only the CLAIM's elision markers are honoured as wildcards. An audited URL is
// a machine-recorded fact and has no reason to contain an elision; treating one
// there as a wildcard would let a scraper's own output widen what grounds a
// claim.

func splitURLParts(u string) (scheme, host, path string, ok bool) {
	scheme, rest, ok := strings.Cut(u, "://")
	if !ok {
		return "", "", "", false
	}
	host, path, _ = strings.Cut(rest, "/")
	if host == "" {
		return "", "", "", false
	}
	return scheme, host, path, true
}

func splitSegments(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

// segmentsMatch is a glob match over path segments where a segment that IS an
// elision sentinel consumes one or more segments.
//
// "One or more", never zero: an elision means the model removed something, so
// there is something there. It also stops `/a/.../b` from grounding `/a/b`,
// which would be a claim about a path that was never fetched.
//
// A pattern that is nothing but an elision is rejected by the caller's
// requirement that at least one literal segment match — see the guard below.
func segmentsMatch(pattern, actual []string) bool {
	// A pattern of only wildcards would ground any path on the host. An
	// elision is a hint from the model about its OWN output; it must not
	// become a universal key to the host.
	literals := 0
	for _, p := range pattern {
		if p != elisionSentinel {
			literals++
		}
	}
	if literals == 0 {
		return false
	}
	return matchSegments(pattern, actual)
}

// matchSegments is the recursive glob. Bounded by len(pattern)*len(actual) in
// the worst case, over path segments of a single URL, so the cost is trivial.
func matchSegments(pattern, actual []string) bool {
	if len(pattern) == 0 {
		return len(actual) == 0
	}
	head := pattern[0]
	if head != elisionSentinel {
		if len(actual) == 0 || actual[0] != head {
			return false
		}
		return matchSegments(pattern[1:], actual[1:])
	}
	// Wildcard: consume one or more segments.
	for take := 1; take <= len(actual); take++ {
		if matchSegments(pattern[1:], actual[take:]) {
			return true
		}
	}
	return false
}

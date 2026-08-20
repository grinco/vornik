package telemetryclient

import (
	"net/url"
	"strings"
	"testing"
)

// The endpoint constant is the one line in this package that decides where
// every default-configuration install sends its lifecycle events. It has no
// business naming a host the project does not control.
//
// grinco/vornik#8 proposed exactly that, replacing the constant with an AWS
// Lambda Function URL while leaving telemetry enabled by default and
// ProductionEmissionEnabled true. The Lambda turned out to be the project's own
// collector (grinco/vornik-infra `lambda/telemetry/`), so the intent was
// benign — but nothing in the build would have objected either way, and a
// third-party host reached by default is the one change in this file that
// cannot be walked back once a release ships.
//
// This test is the objection.

// endpointOnProjectDomain reports whether raw is an https URL on the project's
// own domain. Extracted so the rejected shapes can be asserted directly rather
// than only through the constant.
func endpointOnProjectDomain(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return false
	}
	h := u.Hostname()
	return h == "vornik.io" || strings.HasSuffix(h, ".vornik.io")
}

func TestDefaultEndpointIsOnTheProjectDomain(t *testing.T) {
	if !endpointOnProjectDomain(DefaultEndpoint) {
		t.Fatalf("DefaultEndpoint %q is not an https URL on vornik.io. Every default install "+
			"posts here; it must be a host the project controls.", DefaultEndpoint)
	}
}

func TestEndpointOnProjectDomain_rejectsWhatItMustReject(t *testing.T) {
	for name, raw := range map[string]string{
		"the Lambda Function URL from PR #8": "https://hpbps3m32bqwt6h6ht2flxkfv40msrtt.lambda-url.eu-central-1.on.aws/",
		"plain http on our own domain":       "http://telemetry.vornik.io",
		"a lookalike suffix":                 "https://telemetry.vornik.io.evil.example",
		"an unrelated host":                  "https://collector.example.com/v1/collect.json",
		"empty":                              "",
	} {
		if endpointOnProjectDomain(raw) {
			t.Errorf("%s (%q) was accepted as a project endpoint", name, raw)
		}
	}
	// And the shapes that must keep working: the path-less host the AWS
	// collector serves at `/`, and the legacy collect path it also accepts.
	for _, raw := range []string{
		"https://telemetry.vornik.io",
		"https://telemetry.vornik.io/v1/collect.json",
	} {
		if !endpointOnProjectDomain(raw) {
			t.Errorf("%q was rejected but is a valid project endpoint", raw)
		}
	}
}

// endpointPathIsServed reports whether raw's path is one the deployed collector
// actually routes. `lambda/telemetry/handler.mjs` matches an explicit set —
// PATHS = {"/", "/v1/collect.json"} — and answers anything else with a 404.
//
// The host check above deliberately ignores the path, so it cannot catch this.
// A wrong path is the quietest possible failure: Emit treats every transport
// error as diagnostic and never changes the result of the user operation, so a
// 404 on every event looks exactly like healthy telemetry from inside the
// product. Nothing would surface it until someone noticed the counters were
// flat.
func endpointPathIsServed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch u.Path {
	case "", "/", "/v1/collect.json":
		return true
	default:
		return false
	}
}

func TestDefaultEndpointPathIsOneTheCollectorServes(t *testing.T) {
	if !endpointPathIsServed(DefaultEndpoint) {
		t.Fatalf("DefaultEndpoint %q carries a path the collector does not route. "+
			"handler.mjs serves only \"/\" and \"/v1/collect.json\"; anything else 404s "+
			"and every event is lost without surfacing anywhere.", DefaultEndpoint)
	}
}

func TestEndpointPathIsServed_rejectsUnroutedPaths(t *testing.T) {
	for name, raw := range map[string]string{
		"a plausible but unrouted collect path": "https://telemetry.vornik.io/collect",
		"the v1 prefix without the file":        "https://telemetry.vornik.io/v1/",
		"a trailing slash on the legacy path":   "https://telemetry.vornik.io/v1/collect.json/",
		"an api-style path":                     "https://telemetry.vornik.io/api/v1/collect",
	} {
		if endpointPathIsServed(raw) {
			t.Errorf("%s (%q) was accepted, but the collector would 404 it", name, raw)
		}
	}
	// Query parameters are how the dimensions travel, so they must not affect
	// the path decision — BuildRequest appends them to whatever this constant is.
	for _, raw := range []string{
		"https://telemetry.vornik.io",
		"https://telemetry.vornik.io/",
		"https://telemetry.vornik.io?event=install_succeeded",
		"https://telemetry.vornik.io/v1/collect.json?event=install_succeeded",
	} {
		if !endpointPathIsServed(raw) {
			t.Errorf("%q was rejected but the collector routes it", raw)
		}
	}
}

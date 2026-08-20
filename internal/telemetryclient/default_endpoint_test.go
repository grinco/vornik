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

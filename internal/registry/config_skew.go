package registry

import (
	"bytes"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/config"
)

// UPGRADE SAFETY — diagnosing what the strict loader refuses.
//
// `LoadProjects` decodes with KnownFields(true) and SKIPS a file it cannot
// decode. Those two facts compose into a rule that is not obvious from either
// one alone, and cost a ~30-minute production outage on 2026-09-10:
//
//	A daemon that predates a config key rejects the ENTIRE FILE that uses it,
//	and drops the ENTIRE PROJECT — not the key, not the feature. The project,
//	and with it every surface that project owns.
//
// The loader is not the thing that changes. Fail-closed is correct for a
// permissions-bearing loader: a typo that removes a project fails loudly,
// while an over-permissive one fails nothing and says nothing. What was
// missing is that nothing carried the obligation fail-closed places on a
// DEPLOYER, and nothing diagnosed the resulting state — the failure surfaced
// as "my project is gone", never as "the upgrade rejected a key".
//
// Design: https://docs.vornik.io
// §10 (version skew) and §11 (this diagnosis).

// unknownFieldRE matches yaml.v3's strict-decode complaint, e.g.
//
//	line 6: field feeds not found in type registry.ProjectAutonomy
//
// Parsing an error STRING is not something to do lightly, and it is guarded:
// TestDiagnoseProjectDecodeError_* drive real decoder output rather than
// hand-written strings, so an upstream wording change fails those tests here
// instead of silently degrading the diagnosis in production. When it stops
// matching, the diagnosis reports what it could not determine — it never
// invents a reading.
var unknownFieldRE = regexp.MustCompile(`field (\S+) not found in type (\S+)`)

// SkewDiagnosis is a rejected project file, explained.
//
// Every field is best-effort and every one can be empty. That is deliberate:
// a diagnosis that guesses is worse than one that says it could not tell,
// because an operator acts on it. The distinction that matters — "you spelled
// a key I know wrongly" versus "I have never heard of this key, so your config
// is probably ahead of this binary" — is the difference between a fix the
// operator can make now and one that needs a deploy.
type SkewDiagnosis struct {
	// File is the project file that was skipped, relative to the projects dir.
	File string

	// ProjectID is the project that consequently does not exist, read
	// leniently from the rejected file. Empty when the file is too broken to
	// yield even that.
	ProjectID string

	// UnknownKeys are the keys the decoder refused, leaf names as yaml.v3
	// reports them.
	UnknownKeys []string

	// Suggestions maps an unknown key to a known key it matches apart from
	// spelling (case and separators). Present only for an EXACT normalised
	// match — see normaliseKey. No fuzzy matching: a wrong suggestion sends
	// the operator to change a line that was already right.
	Suggestions map[string]string

	// LikelyVersionSkew is true when at least one unknown key matches nothing
	// in this binary's schema under any spelling. That is what a key from a
	// newer release looks like, and it is what the 2026-09-10 incident was.
	LikelyVersionSkew bool

	// Err is the decoder's own error, kept verbatim.
	Err error
}

// Detail renders the diagnosis as the operator-facing paragraph the loader
// logs, the TreeIndex stores and `vornikctl doctor` prints.
func (d SkewDiagnosis) Detail() string {
	var b strings.Builder

	subject := "the project in " + d.File
	if d.ProjectID != "" {
		subject = fmt.Sprintf("project %q (%s)", d.ProjectID, d.File)
	}
	fmt.Fprintf(&b, "%s was NOT loaded: the whole file was skipped, so the project does not exist in the running registry and every surface it owns — autonomy, chat, email, webhooks — is dark.\n", subject)

	switch {
	case len(d.UnknownKeys) == 0:
		// Not a schema mismatch at all. Say nothing more than that.
		fmt.Fprintf(&b, "Cause: %s could not be parsed at all (%v). This is a syntax error, not a version mismatch — nothing here says the config is ahead of or behind the binary.\n", d.File, d.Err)
		return b.String()

	case d.LikelyVersionSkew:
		fmt.Fprintf(&b, "Cause: the key(s) %s are not known to THIS BINARY under any spelling. That is what a config written for a NEWER release looks like: the binary rejects the key, and the rejection takes the file with it.\n",
			strings.Join(quoteAll(d.unknownWithoutSuggestions()), ", "))
		b.WriteString("Remedy, and the deploy ordering it implies:\n")
		b.WriteString("  • Rolling a new key OUT: deploy the binary that knows the key FIRST, confirm it is the running one, and only then write the key into the deployed config. Hot-reload picks the config change up immediately, so the gap between the two IS the outage window.\n")
		b.WriteString("  • Rolling BACK: remove the key from the config BEFORE downgrading the binary. Removing a key is always safe; an older binary meeting a newer config is this same failure, arriving during a rollback when attention is elsewhere.\n")
		b.WriteString("  • Right now: delete or comment out the key to bring the project back (it returns on the next reload), or deploy the newer binary.\n")

	default:
		b.WriteString("Cause: misspelt key(s), each of which matches a key this binary knows apart from spelling:\n")
		for _, k := range d.UnknownKeys {
			if s := d.Suggestions[k]; s != "" {
				fmt.Fprintf(&b, "  • %q → did you mean %q?\n", k, s)
			}
		}
		b.WriteString("Fix the spelling and the project returns on the next reload. No deploy is needed.\n")
	}

	if len(d.UnknownKeys) > 0 {
		fmt.Fprintf(&b, "Decoder error, verbatim: %v\n", d.Err)
	}
	return b.String()
}

func (d SkewDiagnosis) unknownWithoutSuggestions() []string {
	out := make([]string, 0, len(d.UnknownKeys))
	for _, k := range d.UnknownKeys {
		if d.Suggestions[k] == "" {
			out = append(out, k)
		}
	}
	return out
}

// DiagnoseProjectDecodeError explains one rejected project file.
//
// Never returns an error of its own and never panics on a malformed input:
// this runs on the path where something has ALREADY gone wrong, and a
// diagnosis that can fail is a diagnosis that is missing when it is needed.
func DiagnoseProjectDecodeError(file string, data []byte, err error) SkewDiagnosis {
	d := SkewDiagnosis{File: file, Err: err, Suggestions: map[string]string{}}
	if err == nil {
		return d
	}

	// The project id is read LENIENTLY and on purpose: the file just failed a
	// strict decode, and the one thing the operator most needs from it is
	// which project vanished.
	var head struct {
		ProjectID string `yaml:"projectId"`
	}
	if yaml.Unmarshal(data, &head) == nil {
		d.ProjectID = strings.TrimSpace(head.ProjectID)
	}

	known := projectKeyIndex()
	seen := map[string]bool{}
	for _, m := range unknownFieldRE.FindAllStringSubmatch(err.Error(), -1) {
		key := m[1]
		if seen[key] {
			continue
		}
		seen[key] = true
		d.UnknownKeys = append(d.UnknownKeys, key)
		if match, ok := known[normaliseKey(key)]; ok {
			d.Suggestions[key] = match
		} else {
			// Unknown under every spelling this binary accepts. One such key
			// is enough to make the whole file a skew case: the remedy (deploy
			// ordering) is the one that applies.
			d.LikelyVersionSkew = true
		}
	}
	return d
}

// ProjectConfigKeys returns every dotted YAML key a project file may contain,
// derived from the struct tags of Project itself.
//
// DERIVED, NEVER HAND-MAINTAINED. A record of what a release expects that is
// written by hand is a document that drifts, and drift is the thing this whole
// area is about — this repo already runs a lint over prose-about-code for the
// same reason. Add a field to the Go type and it appears here with no second
// edit; delete one and it disappears.
//
// WHAT THIS DOES NOT COVER, stated because a list with no scope reads as a
// guarantee: it is the keys THIS BINARY accepts, not a history of which
// release introduced each one. It answers "would my config load?", which is
// the upgrade question; it does not answer "what changed in 2026.9.4?", which
// needs a per-key version the schema does not carry yet.
func ProjectConfigKeys() []string {
	keys := make([]string, 0, 256)
	config.WalkLeaves(reflect.ValueOf(Project{}), func(key string, _ reflect.StructField, _ reflect.Value) {
		keys = append(keys, key)
	})
	sort.Strings(keys)
	return keys
}

// projectKeyIndex maps a normalised LEAF name to the first full key that
// spells it. Leaf rather than full path because that is all yaml.v3 tells us
// about a rejection — it names the field and the Go type, not the yaml path.
func projectKeyIndex() map[string]string {
	idx := make(map[string]string, 256)
	for _, k := range ProjectConfigKeys() {
		leaf := k
		if i := strings.LastIndexByte(k, '.'); i >= 0 {
			leaf = k[i+1:]
		}
		n := normaliseKey(leaf)
		if _, exists := idx[n]; !exists {
			idx[n] = leaf
		}
	}
	return idx
}

// normaliseKey strips the difference between spellings of one concept:
// case, underscores and hyphens. `dailySoftUSD` and `daily_soft_usd`
// normalise to the same string.
//
// EXACT matching only, deliberately. The demonstrated hazard in this schema is
// the casing collision — snake_case and camelCase spellings of the same
// concept, which the 2026-08-27 design found twice inside this repo's own
// fixtures — and normalisation catches that class with no false positives. A
// fuzzy match would also catch a transposition like `mention_handel`, at the
// cost of sometimes telling an operator to change a line that was correct.
// Absent a suggestion the diagnosis still says the key is unknown, which is
// true; a wrong suggestion is not.
func normaliseKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '_' || r == '-':
			continue
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// DecodeProjectStrict is THE project decode. One function, so the loader, the
// doctor check and the tests cannot disagree about what the daemon accepts —
// a gate that drifts from the thing it gates is worse than no gate.
//
// Exported for the `project_config_skew` doctor check, which must answer
// "would the running daemon load this file?" and can only answer it honestly
// by asking the same decoder.
func DecodeProjectStrict(data []byte) error {
	_, err := decodeProject(data)
	return err
}

// decodeProject is the single configured decoder. The loader wants the value,
// the doctor check wants only the error, and both must be judging the same
// thing — so there is one function and two thin callers, not two decoders that
// agree today.
func decodeProject(data []byte) (Project, error) {
	var p Project
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	return p, dec.Decode(&p)
}

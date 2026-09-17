package chatauth

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/authz"
)

// The §5.3 compatibility shim. Its whole job is to make the cutover survivable:
// on a deployment where nobody has linked an identity yet — which is every
// deployment the moment this ships — switching to identity-only authorization
// would revoke chat access from every current user at the next restart.
//
// The OR-matrix §5.3 specifies, and which these tests pin cell by cell:
//
//   resolver | legacy | outcome
//   ---------|--------|----------------------------------------
//   grant    | grant  | allowed, NO warn, NO metric  (not legacy-dependent)
//   grant    | deny   | allowed, NO warn, NO metric  (already migrated)
//   deny     | grant  | allowed, WARN + METRIC       (the migration worklist)
//   deny     | deny   | denied
//
// The third row is the only one that counts, and that is deliberate: the
// metric must measure exactly the population that would LOSE access when the
// legacy config keys are removed, not the population the lists happen to name.

type stubResolver struct {
	principals map[string]*authz.Principal
	err        error
}

func (s stubResolver) Resolve(_ context.Context, channel, externalID string) (*authz.Principal, error) {
	if s.err != nil {
		return nil, s.err
	}
	p, ok := s.principals[channel+":"+externalID]
	if !ok {
		return nil, authz.ErrUnknownIdentity
	}
	return p, nil
}

func newShim(t *testing.T, r Resolver) (*Shim, *prometheus.Registry, *strings.Builder) {
	t.Helper()
	reg := prometheus.NewRegistry()
	logs := &strings.Builder{}
	return New(r, NewMetrics(reg), zerolog.New(logs)), reg, logs
}

func legacyGrantCount(t *testing.T, reg *prometheus.Registry) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total float64
	for _, mf := range mfs {
		if mf.GetName() != "vornik_auth_legacy_allowlist_grants_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			total += m.GetCounter().GetValue()
		}
	}
	return total
}

func TestShim_ORMatrix(t *testing.T) {
	known := &authz.Principal{UserID: "u1", Role: authz.RoleUser, Projects: []string{"alpha"}}
	cases := []struct {
		name         string
		linked       bool
		legacyAllow  bool
		wantAllowed  bool
		wantWarn     bool
		wantMetric   bool
		wantProjects []string
	}{
		{"resolver grants, legacy grants", true, true, true, false, false, []string{"alpha"}},
		{"resolver grants, legacy denies", true, false, true, false, false, []string{"alpha"}},
		{"resolver denies, legacy grants", false, true, true, true, true, nil},
		{"resolver denies, legacy denies", false, false, false, false, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := stubResolver{principals: map[string]*authz.Principal{}}
			if tc.linked {
				res.principals["telegram:42"] = known
			}
			s, reg, logs := newShim(t, res)

			got := s.Authorize(context.Background(), "telegram", "42", Legacy{Allowed: tc.legacyAllow, ConfigKey: "telegram.allowed_users"})
			if got.Allowed != tc.wantAllowed {
				t.Fatalf("Allowed = %v, want %v", got.Allowed, tc.wantAllowed)
			}
			if warned := strings.Contains(logs.String(), "legacy allowlist"); warned != tc.wantWarn {
				t.Errorf("warned = %v, want %v (log: %s)", warned, tc.wantWarn, logs.String())
			}
			if n := legacyGrantCount(t, reg); (n > 0) != tc.wantMetric {
				t.Errorf("legacy-grant metric = %v, want fired=%v", n, tc.wantMetric)
			}
			if tc.wantAllowed && tc.linked {
				if len(got.Projects) != len(tc.wantProjects) {
					t.Errorf("Projects = %v, want %v — a resolved principal carries its own scope", got.Projects, tc.wantProjects)
				}
			}
		})
	}
}

// TestShim_MetricCarriesTheConfigKeyThatGranted — the WARN and the metric are
// a migration worklist, so they must name the key an operator has to remove
// and the identity that still depends on it.
func TestShim_MetricCarriesTheConfigKeyThatGranted(t *testing.T) {
	s, reg, logs := newShim(t, stubResolver{principals: map[string]*authz.Principal{}})
	s.Authorize(context.Background(), "slack", "U123", Legacy{Allowed: true, ConfigKey: "slack.sender_allowlist"})

	mfs, _ := reg.Gather()
	var labels []*dto.LabelPair
	for _, mf := range mfs {
		if mf.GetName() == "vornik_auth_legacy_allowlist_grants_total" && len(mf.GetMetric()) > 0 {
			labels = mf.GetMetric()[0].GetLabel()
		}
	}
	found := map[string]string{}
	for _, l := range labels {
		found[l.GetName()] = l.GetValue()
	}
	if found["channel"] != "slack" || found["config_key"] != "slack.sender_allowlist" {
		t.Errorf("metric labels = %v, want channel=slack config_key=slack.sender_allowlist", found)
	}
	if !strings.Contains(logs.String(), "slack.sender_allowlist") || !strings.Contains(logs.String(), "U123") {
		t.Errorf("the WARN must name the config key AND the identity to migrate: %s", logs.String())
	}
}

// TestShim_WarnIsDedupedPerIdentity — the log doubles as the awaiting-migration
// list, which is only readable if one identity produces one line rather than
// one per message.
func TestShim_WarnIsDedupedPerIdentity(t *testing.T) {
	s, _, logs := newShim(t, stubResolver{principals: map[string]*authz.Principal{}})
	for i := 0; i < 5; i++ {
		s.Authorize(context.Background(), "telegram", "42", Legacy{Allowed: true, ConfigKey: "telegram.allowed_users"})
	}
	if n := strings.Count(logs.String(), "legacy allowlist"); n != 1 {
		t.Errorf("warned %d times for one identity, want 1 — the log is a worklist, not a firehose", n)
	}
	// A DIFFERENT identity is a different line: the point is to enumerate who
	// still needs migrating.
	s.Authorize(context.Background(), "telegram", "43", Legacy{Allowed: true, ConfigKey: "telegram.allowed_users"})
	if n := strings.Count(logs.String(), "legacy allowlist"); n != 2 {
		t.Errorf("a second identity produced %d lines, want 2", n)
	}
}

// TestShim_MetricCountsEveryLegacyGrant — unlike the WARN, the counter is NOT
// deduped: the removal gate is "14 consecutive days of zero increments", so a
// deduped counter would read zero while an identity was still depending on the
// legacy list every minute.
func TestShim_MetricCountsEveryLegacyGrant(t *testing.T) {
	s, reg, _ := newShim(t, stubResolver{principals: map[string]*authz.Principal{}})
	for i := 0; i < 5; i++ {
		s.Authorize(context.Background(), "telegram", "42", Legacy{Allowed: true, ConfigKey: "telegram.allowed_users"})
	}
	if n := legacyGrantCount(t, reg); n != 5 {
		t.Errorf("metric = %v, want 5 — the removal gate reads this, and a deduped counter would read zero while access was still legacy-dependent", n)
	}
}

// TestShim_RemovingTheLegacyEntryIsWhatReleasesTheGate replaces the severed-
// identity test. Round-3 finding F15 asked for a Sever() method on the premise
// that legacy removal is wholesale per config key; it is not — the allowlists
// are keyed per identity, so deleting one entry is the durable, restart-proof
// way to stop a straggler holding the 14-day gate open. See the note in
// shim.go.
func TestShim_RemovingTheLegacyEntryIsWhatReleasesTheGate(t *testing.T) {
	s, reg, _ := newShim(t, stubResolver{principals: map[string]*authz.Principal{}})
	granting := Legacy{Allowed: true, ConfigKey: "telegram.allowed_users"}

	if got := s.Authorize(context.Background(), "telegram", "99", granting); !got.Allowed {
		t.Fatal("precondition: the legacy list should grant while the entry exists")
	}
	before := legacyGrantCount(t, reg)

	// The operator deletes that one entry; the caller now reports no legacy
	// grant for this identity.
	removed := Legacy{Allowed: false, ConfigKey: "telegram.allowed_users"}
	if got := s.Authorize(context.Background(), "telegram", "99", removed); got.Allowed {
		t.Error("with its allowlist entry gone the identity must be denied")
	}
	if legacyGrantCount(t, reg) != before {
		t.Error("a denied identity must not increment the gate's metric, or the gate never closes")
	}
}

// TestShim_ResolverErrorDoesNotSilentlyFallBack: a resolver that is DOWN is
// not a resolver that says no. Falling through to the legacy list on an error
// would turn a database blip into a silent authorization downgrade, so the
// error path is reported and counted separately.
func TestShim_ResolverErrorIsDistinguishedFromDenial(t *testing.T) {
	s, _, logs := newShim(t, stubResolver{err: errors.New("db down")})
	got := s.Authorize(context.Background(), "telegram", "42", Legacy{Allowed: true, ConfigKey: "telegram.allowed_users"})
	if !got.Allowed {
		t.Error("during the shim window a resolver outage must not lock out a legacy-listed operator")
	}
	if !strings.Contains(logs.String(), "resolver unavailable") {
		t.Errorf("a resolver outage must be visible as such, not as a routine legacy grant: %s", logs.String())
	}
}

// TestShim_NoResolverWiredIsPureLegacy — the pre-Phase-4 behaviour, unchanged.
func TestShim_NoResolverWiredIsPureLegacy(t *testing.T) {
	s, reg, logs := newShim(t, nil)
	if got := s.Authorize(context.Background(), "telegram", "42", Legacy{Allowed: true}); !got.Allowed {
		t.Error("with no resolver the legacy list is the only authority and must still grant")
	}
	if n := legacyGrantCount(t, reg); n != 0 {
		t.Errorf("metric fired (%v) with no resolver wired; there is no migration to measure yet", n)
	}
	if strings.Contains(logs.String(), "legacy allowlist") {
		t.Error("no resolver means no migration worklist, so no WARN")
	}
}

// TestShim_WarnDedupIsBounded — the dedup set is a worklist keyed by identity.
// On a channel whose external ids churn it never repeats a key, so without a
// cap it grows for the process lifetime (review-20260914-3c36 F3).
//
// The property that must survive the cap is the metric, not the log line: the
// §5.3 removal gate reads the counter, and the counter is not deduped.
func TestShim_WarnDedupIsBounded(t *testing.T) {
	reg := prometheus.NewRegistry()
	s := New(stubResolver{}, NewMetrics(reg), zerolog.New(io.Discard))

	const n = warnedCap + 100
	for i := 0; i < n; i++ {
		d := s.Authorize(context.Background(), "telegram", "churn-"+strconv.Itoa(i),
			Legacy{Allowed: true, ConfigKey: "telegram.allowed_users"})
		if !d.Allowed || !d.ViaLegacyOnly {
			t.Fatalf("identity %d: legacy-only grant expected, got %+v", i, d)
		}
	}

	s.mu.Lock()
	size := len(s.warned)
	s.mu.Unlock()
	if size > warnedCap {
		t.Errorf("dedup set holds %d entries, above the %d cap", size, warnedCap)
	}

	// Every one of them counted, cap or no cap.
	if got := legacyGrantCount(t, reg); got != n {
		t.Errorf("legacy grant counter = %v, want %d — the removal gate reads this, and it must not be deduped", got, n)
	}
}

// TestMetrics_CountsBeforeAndAfterAttach — the daemon builds this shim during
// subsystem init, before the served registry exists. NewMetrics(nil) used to
// fall back to prometheus.DefaultRegisterer, which /metrics does not serve
// (the 2026-06-06 invisible-metric incident): the counter incremented where
// nobody could read it.
//
// For THIS counter that is not a missing metric, it is a wrong one. The §5.3
// removal gate reads "14 consecutive days at zero" off it, so an unserved
// counter reads as a satisfied gate — a control that cannot tell "nobody used
// the legacy allowlist" from "nothing was ever counted" reports the first and
// means the second.
func TestMetrics_CountsBeforeAndAfterAttach(t *testing.T) {
	m := NewMetrics(nil) // detached, as the daemon builds it
	s := New(stubResolver{}, m, zerolog.New(io.Discard))
	legacy := Legacy{Allowed: true, ConfigKey: "telegram.allowed_users"}

	// Two grants while no registry exists. These must not vanish.
	s.Authorize(context.Background(), "telegram", "1", legacy)
	s.Authorize(context.Background(), "telegram", "2", legacy)

	reg := prometheus.NewRegistry()
	m.Attach(reg)
	if got := legacyGrantCount(t, reg); got != 2 {
		t.Errorf("after Attach the counter reads %v, want 2 — grants before the registry existed were dropped, and the removal gate would read them as a clean window", got)
	}

	// And it keeps counting afterwards.
	s.Authorize(context.Background(), "telegram", "3", legacy)
	if got := legacyGrantCount(t, reg); got != 3 {
		t.Errorf("after Attach the counter reads %v, want 3", got)
	}

	// Idempotent: initHTTPServer runs twice, and a second Attach must not
	// register a duplicate collector (which panics) or reset the count.
	m.Attach(reg)
	if got := legacyGrantCount(t, reg); got != 3 {
		t.Errorf("a second Attach changed the count to %v, want 3", got)
	}
}

// TestMetrics_RecordsDuringAttachAreNotLost — one round-2 reviewer read the
// drain as lossy: a recorder blocked on the mutex would, on acquiring it,
// find vec still nil and write into a pending map Attach had just cleared.
//
// That reading assumes Attach releases the lock between the drain and the
// store. It does not — the unlock is deferred, so it happens after
// vec.Store, and a blocked recorder always finds vec set. The other reviewer
// read it correctly. Neither reading should be settled by argument, so this
// test hammers the exact window instead: N recorders racing one Attach, and
// every single grant must appear in the counter.
func TestMetrics_RecordsDuringAttachAreNotLost(t *testing.T) {
	const n = 200
	m := NewMetrics(nil)
	s := New(stubResolver{}, m, zerolog.New(io.Discard))
	legacy := Legacy{Allowed: true, ConfigKey: "telegram.allowed_users"}
	reg := prometheus.NewRegistry()

	var start, done sync.WaitGroup
	start.Add(1)
	done.Add(n + 1)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer done.Done()
			start.Wait()
			s.Authorize(context.Background(), "telegram", "racer-"+strconv.Itoa(i), legacy)
		}(i)
	}
	go func() {
		defer done.Done()
		start.Wait()
		m.Attach(reg)
	}()
	start.Done()
	done.Wait()

	if got := legacyGrantCount(t, reg); got != n {
		t.Errorf("counter = %v after %d grants racing Attach, want %d — a grant landed in a "+
			"drained tally, and the removal gate reads this counter", got, n, n)
	}
}

// TestMetrics_NilHolderIsSafe — the container attaches this holder
// unconditionally in its pass-2 metrics block, and on every deployment
// without an identity core the holder is nil, because it is created inside
// chatAuthShim() which returns early in that case.
//
// So the nil case is not an edge, it is the majority configuration, and a
// panic here would be a startup crash-loop on the deployments least equipped
// to diagnose it.
func TestMetrics_NilHolderIsSafe(t *testing.T) {
	var m *Metrics
	m.Attach(prometheus.NewRegistry()) // must not panic
	m.recordLegacyGrant("telegram", "telegram.allowed_users")

	// And a shim holding a nil holder still authorizes.
	s := New(stubResolver{}, nil, zerolog.New(io.Discard))
	d := s.Authorize(context.Background(), "telegram", "1",
		Legacy{Allowed: true, ConfigKey: "telegram.allowed_users"})
	if !d.Allowed {
		t.Error("a shim with no metrics refused a grant; metrics are observability, not the decision")
	}
}

// TestShim_DistinguishesAnOutageFromAnOrdinaryNo — a caller that must refuse
// on anything short of a resolved identity still needs to know WHY, because
// the two reasons have different remedies: "you have not linked yet" is fixed
// by /link, and "the resolver is down" is fixed by waiting or by an operator.
// Telling a person to link when the database is unreachable sends them to do
// something that cannot work.
//
// Added 2026-09-15 for the configuration assistant's chat entrypoint, whose
// design (config-assistant §6.3.3, test 23) requires it to refuse while
// identity resolution is unavailable AND say so. resolve() already drew this
// line — ErrUnknownIdentity and ErrUserDisabled are ordinary noes, anything
// else is an outage — and Decision did not carry it out.
func TestShim_DistinguishesAnOutageFromAnOrdinaryNo(t *testing.T) {
	linked := map[string]*authz.Principal{"slack:U-linked": {UserID: "user_1"}}

	// An outage, with NO legacy list: denied, and marked unavailable.
	s, _, _ := newShim(t, stubResolver{err: errors.New("database is down")})
	d := s.Authorize(context.Background(), "slack", "U-x", Legacy{})
	if d.Allowed {
		t.Error("a resolver outage granted access")
	}
	if !d.ResolverUnavailable {
		t.Error("a resolver outage is indistinguishable from an unlinked sender; " +
			"a caller cannot name the right remedy")
	}

	// An ordinary "no" — the identity is simply not linked.
	s, _, _ = newShim(t, stubResolver{principals: linked})
	d = s.Authorize(context.Background(), "slack", "U-stranger", Legacy{})
	if d.Allowed || d.ResolverUnavailable {
		t.Errorf("an unlinked sender was reported as an outage: %+v", d)
	}

	// A success must never be marked unavailable.
	d = s.Authorize(context.Background(), "slack", "U-linked", Legacy{})
	if !d.Allowed || d.Principal == nil || d.ResolverUnavailable {
		t.Errorf("a resolved identity was mis-reported: %+v", d)
	}

	// THE CASE THAT MATTERS MOST: an outage WITH a legacy list that would
	// admit. The shim still grants for ordinary chat (that is its whole
	// purpose), but it must mark the grant legacy-only AND flag the outage,
	// so a caller requiring a real identity refuses and explains.
	s, _, _ = newShim(t, stubResolver{err: errors.New("database is down")})
	d = s.Authorize(context.Background(), "slack", "U-listed", Legacy{Allowed: true, ConfigKey: "sender_allowlist"})
	if !d.Allowed || !d.ViaLegacyOnly {
		t.Errorf("the shim stopped granting ordinary chat during an outage: %+v", d)
	}
	if d.Principal != nil {
		t.Error("a legacy-only grant carried a Principal; callers use that to mean 'identified'")
	}
	if !d.ResolverUnavailable {
		t.Error("an outage masked by a legacy grant is unreported; the config entrypoint " +
			"would tell the person to /link while the resolver is down")
	}
}

// TestShim_EveryOrdinaryNoIsPinned closes the gap review-20260915-8717 F1
// named: ResolverUnavailable's honesty is INHERITED from resolve()'s filter,
// and only one of the two sentinels that filter names was tested.
//
// The table is over the sentinels authz declares as ordinary answers. If a
// third is ever added — ErrUserPending, ErrTenantSuspended — and resolve()'s
// whitelist is not extended with it, this test fails instead of the
// entrypoint silently telling a merely-unlinked person to "wait and tell an
// operator" while their actual remedy is /link. That is the INVERSE of the
// wrong-remedy bug the field was added to fix, and it would otherwise ship
// past the suite.
func TestShim_EveryOrdinaryNoIsPinned(t *testing.T) {
	ordinary := map[string]error{
		"unknown identity": authz.ErrUnknownIdentity,
		"user disabled":    authz.ErrUserDisabled,
	}
	for name, sentinel := range ordinary {
		s, _, _ := newShim(t, stubResolver{err: sentinel})
		d := s.Authorize(context.Background(), "slack", "U-x", Legacy{})
		if d.Allowed {
			t.Errorf("%s: granted access", name)
		}
		if d.ResolverUnavailable {
			t.Errorf("%s: reported as a RESOLVER OUTAGE. It is an ordinary answer, and a caller "+
				"that believes it is an outage tells the person to wait when their remedy is to link", name)
		}
	}

	// And the counterpart: the sentinel authz declares FOR unavailability must
	// be reported as one. ErrResolverUnavailable exists precisely so a door
	// that cannot resolve is distinguishable from one that resolved to "no".
	s, _, _ := newShim(t, stubResolver{err: authz.ErrResolverUnavailable})
	if d := s.Authorize(context.Background(), "slack", "U-x", Legacy{}); !d.ResolverUnavailable {
		t.Error("authz.ErrResolverUnavailable was not reported as an outage; the one sentinel " +
			"whose entire purpose is to say so")
	}
}

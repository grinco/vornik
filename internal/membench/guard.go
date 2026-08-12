package membench

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// Destructive-run guard (design §5.8).
//
// A benchmark run bulk-writes memory and clears it between runs. That makes it
// exactly the kind of tool that eventually gets pointed at the wrong DSN — so
// the protection is a code-level block, not a documented convention.

// DenyDatabasesEnv names additional databases that can never be a benchmark
// target, comma-separated.
//
// This exists because a database name can LIE about its role — the reference
// deployment's production database reads like a scratch name, a historical artifact
// of a rename — and a lying name cannot be caught by any generic pattern. It has to
// be written down to be denied at all, which left two options: name it in shipped
// source, or name it in the deployment's own configuration.
//
// Shipped source was the original choice and it was wrong: the CE export's
// operator-token scan failed the build because the name is a piece of one operator's
// infrastructure, published to every reader of the public repository, while being
// useless to them — their production database has a different name.
//
// **This variable may only ADD denials.** §5.8 rejects an environment override on
// the grounds that an exported variable persists for a whole shell and is inherited
// by children, so it is a weaker authorisation than a flag retyped per invocation.
// That argument is entirely about LOOSENING the guard. Tightening carries none of
// it: a variable that can only ever refuse more is safe to inherit, and the failure
// mode of forgetting to set it is a denial that does not happen rather than a
// protection silently removed.
const DenyDatabasesEnv = "VORNIK_BENCH_DENY_DATABASES"

// deniedDatabases can never be a benchmark target, whatever the operator types.
//
// Deliberately GENERIC. Deployment-specific names belong in DenyDatabasesEnv, and
// TestGuard_ShipsNoDeploymentSpecificName fails the build if one reappears here.
var deniedDatabases = []string{
	"prod",
	"production",
	"live",
}

// CheckDestructiveTarget authorises a destructive run against database.
//
// Two conditions, both required:
//
//  1. confirmation must equal database — the operator names the target
//     explicitly, so a run cannot proceed against a DSN they have not read.
//  2. database must be on neither the shipped denylist nor the configured one.
//
// There is deliberately NO override that loosens either list — no flag, and no
// environment variable. Round-1 review proposed an env-var escape hatch "for local
// dev, but not by CLI flag"; that is strictly weaker, because an exported variable
// persists for a whole shell session and is inherited by every child process,
// whereas a flag must be retyped per invocation and lands in shell history beside
// the command it authorised. Local development points at a differently-named
// database, which costs one createdb.
func CheckDestructiveTarget(database, confirmation string) error {
	db := strings.TrimSpace(database)
	conf := strings.TrimSpace(confirmation)

	if db == "" {
		return fmt.Errorf("refusing to run: no target database named. An empty " +
			"database name usually means a default was substituted downstream")
	}
	if conf == "" {
		return fmt.Errorf("refusing to run: pass --i-know-this-wipes %s to confirm "+
			"the target, which this run will bulk-write and clear", db)
	}
	if conf != db {
		return fmt.Errorf("refusing to run: confirmation names %q but the configured "+
			"database is %q — you may not be looking at the DSN you think you are",
			conf, db)
	}
	for _, denied := range deniedDatabases {
		if !strings.EqualFold(db, denied) {
			continue
		}
		return fmt.Errorf("refusing to run against %q: it is on the hardcoded "+
			"denylist because it is a production database. No flag and no "+
			"environment variable overrides this. Point the harness at a "+
			"differently-named database instead", db)
	}
	for _, denied := range configuredDenials() {
		if !strings.EqualFold(db, denied) {
			continue
		}
		// Naming the source matters: an operator hitting a denial they cannot find
		// in the source would otherwise read the guard as a bug and go looking for
		// a bypass.
		return fmt.Errorf("refusing to run against %q: this deployment lists it in "+
			"%s, which denies databases whose names do not advertise their role. "+
			"Nothing overrides this. Point the harness at a differently-named "+
			"database instead", db, DenyDatabasesEnv)
	}
	return nil
}

// configuredDenials reads the deployment's additional denials.
//
// Empty entries are skipped so a trailing comma — normal in a hand-edited unit file
// — cannot produce an empty pattern that would match an empty database name.
func configuredDenials() []string {
	raw := strings.TrimSpace(os.Getenv(DenyDatabasesEnv))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// WriteTargetReporter is an optional MemorySystem capability: the database the
// system actually bulk-writes, as the system itself reports it.
//
// It exists because the destructive-run guard could not previously tell the
// difference between the database an operator NAMED and the one that would be
// written. Those turned out to be different things, and the gap was not
// theoretical — see VerifyWriteTarget.
type WriteTargetReporter interface {
	// WriteTargetDatabase returns the database this system writes to. An error
	// means the system could not establish it, which the guard treats as a refusal
	// rather than as permission.
	WriteTargetDatabase(ctx context.Context) (string, error)
}

// VerifyWriteTarget refuses a destructive run unless the system under test
// confirms it writes the database the operator named.
//
// WHY THIS EXISTS, precisely. CheckDestructiveTarget validates --database and
// --i-know-this-wipes as strings: it proves the operator typed a name twice and
// that the name is not denylisted. It cannot prove the run will write THAT
// database. On 2026-08-12 it did not: a run naming a freshly-created throwaway put
// twelve fixture documents into the production corpus, because the vornik adapter
// reaches a running daemon over companion MCP and the daemon writes to whatever
// database it is configured with. The named database was left with zero tables.
//
// A guard that reads as containment and provides none is worse than no guard,
// because the flag name is what stops an operator looking further.
//
// IT FAILS CLOSED. A system that cannot report its write target — an older daemon,
// a broken token, an adapter with no such capability — is refused, not waved
// through. Passing on "could not verify" is precisely the behaviour that caused the
// incident, and an unverified guard is indistinguishable from an absent one while
// still looking like protection.
func VerifyWriteTarget(ctx context.Context, sys MemorySystem, database string) error {
	want := strings.TrimSpace(database)

	reporter, ok := sys.(WriteTargetReporter)
	if !ok {
		return fmt.Errorf("refusing to run: the %q system cannot report which database it "+
			"writes, so naming %q proves nothing about where this run's writes will land. "+
			"That gap put fixture documents into a production corpus on 2026-08-12. Use an "+
			"adapter that reports its write target",
			sys.Name(), want)
	}

	got, err := reporter.WriteTargetDatabase(ctx)
	if err != nil {
		return fmt.Errorf("refusing to run: could not establish which database the %q system "+
			"writes (%w). The guard fails closed — 'unverified' is not 'safe', and treating it "+
			"as safe is what let a benchmark write production",
			sys.Name(), err)
	}

	got = strings.TrimSpace(got)
	if got == "" {
		return fmt.Errorf("refusing to run: the %q system reported an EMPTY write target, "+
			"which names no database. Treating empty as agreement would re-open the hole for "+
			"any deployment whose reporting is misconfigured", sys.Name())
	}
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("refusing to run: you named %q but the %q system actually writes "+
			"%q. This is the check that was missing on 2026-08-12, when a run naming a "+
			"throwaway database wrote twelve documents into the production corpus and left "+
			"the named database empty. Point the system at %q, or name %q if that is really "+
			"the target",
			want, sys.Name(), got, want, got)
	}
	return nil
}

#!/usr/bin/env bash
# Unit tests for scripts/bench-write-target-guard.sh.
#
# The guard authorises a run that will bulk-write and CLEAR a database, so its
# failure mode is not "the benchmark did not run" — it is a production database
# cleared by a run pointed at the wrong daemon. Each case below is a way that
# has nearly happened or could.
#
# Run: bash scripts/bench-write-target-guard_test.sh
set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/bench-write-target-guard.sh
. "$HERE/bench-write-target-guard.sh"

pass=0; fail=0
ok()  { pass=$((pass+1)); echo "PASS: $1"; }
bad() { fail=$((fail+1)); echo "FAIL: $1" >&2; }
want_refused() { if "$@" >/dev/null 2>&1; then return 1; else return 0; fi; }

tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT

benchcfg="$tmp/bench.yaml"
cat > "$benchcfg" <<'YAML'
server:
  name: not-the-database
database:
  host: localhost
  port: 5432
  name: agentbench_81151576
  user: swarmd
YAML

prodcfg="$tmp/prod.yaml"
cat > "$prodcfg" <<'YAML'
database:
  name: legacy_prod_test
YAML

placeholdercfg="$tmp/placeholder.yaml"
cat > "$placeholdercfg" <<'YAML'
database:
  name: "${POSTGRES_DB}"
YAML

# --- database.name is read from the database BLOCK, not the first name: ---
got="$(bench_config_database "$benchcfg")"
if [ "$got" = "agentbench_81151576" ]; then
    ok "database.name is read from the database block, not the first name: in the file"
else
    bad "database.name = '$got', want agentbench_81151576 (an earlier name: was picked up)"
fi

commentcfg="$tmp/comment.yaml"
cat > "$commentcfg" <<'YAML'
database:
  name: agentbench_81151576  # the isolated harness target
YAML
got="$(bench_config_database "$commentcfg")"
if [ "$got" = "agentbench_81151576" ]; then
    ok "a trailing YAML comment is not part of the database name"
else
    bad "trailing comment leaked into the name: '$got'"
fi

# --- the happy path ---
if bench_assert_write_target "agentbench_81151576" "$benchcfg" >/dev/null 2>&1; then
    ok "a daemon writing the bench database is authorised"
else
    bad "the bench database was refused"
fi

# --- THE CASE THIS EXISTS FOR ---
# VORNIK_URL points at production; the daemon truthfully reports a production
# database whose name reads like a scratch one (legacy_prod_test stands in for
# the reference deployment's real name, which the public tree must not carry),
# and which matches none of the prod/production/live names the old guard listed.
if want_refused bench_assert_write_target "legacy_prod_test" "$benchcfg"; then
    ok "a daemon writing the PRODUCTION database is refused (the old guard allowed it)"
else
    bad "legacy_prod_test was authorised for a run that clears it"
fi

# The message must name both databases: an operator at 2am needs to see that the
# daemon and the config disagree, not just that something was refused.
msg="$(bench_assert_write_target "legacy_prod_test" "$benchcfg" 2>&1)"
case "$msg" in
    *legacy_prod_test*agentbench_81151576*) ok "the refusal names both the reported and the expected database" ;;
    *) bad "the refusal does not name both databases: $msg" ;;
esac

# --- pointing --daemon-config at the PRODUCTION config does not launder it ---
# Both sides then say legacy_prod_test and agree, which is exactly why the config
# must be the bench one; this asserts the failure is at least loud in the
# expected way rather than silently "matching".
if bench_assert_write_target "legacy_prod_test" "$prodcfg" >/dev/null 2>&1; then
    ok "a config that names the production database matches it (documented: the check identifies the DEPLOYMENT, so --daemon-config must be the bench one)"
else
    bad "unexpected refusal when both sides agree"
fi

# --- fail closed on anything unverifiable ---
if want_refused bench_assert_write_target "" "$benchcfg"; then
    ok "a daemon that reports no write target is refused"
else
    bad "an empty write target was authorised"
fi

if want_refused bench_assert_write_target "agentbench_81151576" "$tmp/missing.yaml"; then
    ok "a missing bench config is refused rather than skipped"
else
    bad "a missing config authorised the run"
fi

if want_refused bench_assert_write_target "agentbench_81151576" "$placeholdercfg"; then
    ok "an unexpanded \${POSTGRES_DB} placeholder is refused"
else
    bad "a placeholder was treated as a literal name"
fi

# --- an absent denial list WARNS and proceeds ---
# It refused until the 2026-09-04 review pointed out that the check can only see
# the variable is non-empty, not that it names this deployment's database — so
# as a gate it was a checkbox, while the write-target check above already covers
# this script's hazard. What it can do honestly is not be silent.
warn_out="$(
    unset VORNIK_BENCH_DENY_DATABASES
    bench_assert_deny_list_loaded 2>&1 >/dev/null
)"
warn_rc=$(
    unset VORNIK_BENCH_DENY_DATABASES
    bench_assert_deny_list_loaded >/dev/null 2>&1
    echo $?
)
if [ "$warn_rc" -eq 0 ]; then
    ok "an unset VORNIK_BENCH_DENY_DATABASES does not block the run"
else
    bad "the denial-list check still refuses (it is advisory now)"
fi
case "$warn_out" in
    *warning:*VORNIK_BENCH_DENY_DATABASES*) ok "...but it says so, naming the variable" ;;
    *) bad "an absent denial list was silent: '$warn_out'" ;;
esac

if (
    VORNIK_BENCH_DENY_DATABASES=legacy_prod_test
    export VORNIK_BENCH_DENY_DATABASES
    bench_assert_deny_list_loaded >/dev/null 2>&1
); then
    ok "a loaded denial list passes"
else
    bad "a loaded denial list was refused"
fi

echo "================================"; echo "PASSED: $pass"; echo "FAILED: $fail"
[ "$fail" -eq 0 ]

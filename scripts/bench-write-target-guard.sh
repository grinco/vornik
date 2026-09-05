#!/usr/bin/env bash
# bench-write-target-guard.sh — assert a benchmark run's writes land in the
# benchmark database, by POSITIVE identification.
#
# Sourced by scripts/agentbench-reproduce.sh; exercised directly by
# scripts/bench-write-target-guard_test.sh.
#
# WHY THIS EXISTS, AND WHY IT IS NOT A LIST OF NAMES.
#
# A benchmark run passes --database and --i-know-this-wipes to a command that
# bulk-writes and CLEARS the named database. agentbench-reproduce.sh resolves
# that name by asking the daemon at $VORNIK_URL where it writes — correct, and
# the reason it asks is that naming a database proves nothing when the daemon
# writes wherever it was configured (the 2026-08-12 incident that put twelve
# fixture documents into a production corpus).
#
# It then guarded the answer with:
#
#     case "$DB" in prod|production|live) fail ... ;; esac
#
# Three guessed names. The reference deployment's PRODUCTION database carries a
# name that reads like a scratch database and is not one — a historical artifact
# of a rename, ending in `_test`. It matches none of the three, so
# a run started with VORNIK_URL pointing at the production daemon instead of the
# bench daemon would have been authorised to clear the live database. The
# --i-know-this-wipes confirmation could not catch it either, because the script
# fills both sides of that comparison from the same variable: a confirmation a
# script can compute is not a confirmation.
#
# internal/membench/guard.go carries the real denylist AND a deployment-supplied
# one (VORNIK_BENCH_DENY_DATABASES, which a deployment sets to its production
# database's name through shell-env.sh). The case statement above was a second, weaker
# implementation of that check — and the agent-benchmark design is explicit that
# these primitives are "imported, never re-implemented: a safety check with two
# implementations has one that is wrong". This was the wrong one.
#
# THE RULE HERE IS THE INVERSE: name the one database that IS allowed, and refuse
# everything else. An allowlist cannot be wrong about a name nobody guessed,
# and the allowed name is not typed — it is read from the bench daemon's own
# config, so the check compares what the daemon reports against what the bench
# deployment is configured to be.

# bench_config_database FILE — print the `database.name` value from a daemon
# config. Prints nothing when the file, the block, or the key is absent.
#
# Parses the database BLOCK specifically. The sibling generator
# (scripts/gen-shell-env.sh) takes the first `name:` in the file, which is a
# different key on any config whose earlier sections have one — a latent bug
# filed separately. Not repeated here.
bench_config_database() {
    [ -r "${1:-}" ] || return 0
    python3 - "$1" <<'PY' 2>/dev/null || true
import re, sys
try:
    text = open(sys.argv[1], encoding="utf-8").read()
except OSError:
    raise SystemExit(0)
# The `database:` block, then its `name:` — indentation-scoped, so a `name:`
# belonging to any other section cannot be mistaken for it.
block = re.search(r"(?m)^database:[ \t]*$\n((?:[ \t]+.*\n|\s*\n)*)", text)
if not block:
    # Say why. A silent exit here becomes a refusal with no diagnosis, and an
    # operator cannot tell a mis-indented config from a missing one.
    print("bench guard: no `database:` block in %s" % sys.argv[1], file=sys.stderr)
    raise SystemExit(0)
name = re.search(r"(?m)^[ \t]+name:[ \t]*(.+?)[ \t]*$", block.group(1))
if not name:
    print("bench guard: `database:` block in %s has no name: key" % sys.argv[1], file=sys.stderr)
    raise SystemExit(0)
value = name.group(1).strip()
# A trailing YAML comment is part of the line, not the value. Only outside
# quotes: `name: "db # 1"` is a (perverse) literal name, `name: db # prod` is
# not. Without this the comparison fails closed on a spurious mismatch.
if not value.startswith(('"', "'")):
    value = re.split(r"\s+#", value, 1)[0].strip()
print(value.strip('"').strip("'"))
PY
}

# bench_assert_write_target RESOLVED_DB BENCH_CONFIG
#
# Refuses unless RESOLVED_DB — what the daemon says it writes — is exactly the
# database the bench config names. Prints the reason to stderr and returns 1.
bench_assert_write_target() {
    _resolved="${1:-}"
    _config="${2:-}"

    if [ -z "$_resolved" ]; then
        echo "refusing: the daemon reported no write target. 'unverified' is not 'safe'." >&2
        return 1
    fi

    _want="$(bench_config_database "$_config")"
    if [ -z "$_want" ]; then
        echo "refusing: cannot read database.name from the bench config '${_config}'," >&2
        echo "  so there is nothing to check the daemon's answer ('${_resolved}') against." >&2
        echo "  Point --daemon-config at the bench daemon's config." >&2
        return 1
    fi

    # An unexpanded ${VAR} cannot be compared. Failing closed here is the point:
    # a placeholder is exactly how gen-shell-env silently protected nothing on
    # every placeholder-based install.
    # shellcheck disable=SC2016 # intentional: matching the LITERAL text "${...}"
    case "$_want" in
        '${'*'}'|'$'*)
            echo "refusing: the bench config's database.name is the unexpanded placeholder '${_want}'." >&2
            echo "  A name that is not a literal cannot identify the write target." >&2
            return 1
            ;;
    esac

    if [ "$_resolved" != "$_want" ]; then
        echo "refusing: the daemon writes '${_resolved}', but the bench deployment's database" >&2
        echo "  is '${_want}'. This run would bulk-write and CLEAR '${_resolved}'." >&2
        echo "  The usual cause is VORNIK_URL pointing at the production daemon instead of" >&2
        echo "  the bench one. Nothing about the name is checked — only that the daemon you" >&2
        echo "  are talking to is the deployment you configured." >&2
        return 1
    fi

    return 0
}

# bench_assert_deny_list_loaded — require this deployment's own denial list to be
# present in the environment.
#
# VORNIK_BENCH_DENY_DATABASES is what denies a production database whose NAME
# does not advertise its role; shell-env.sh exports it, and ~/.bashrc.d sources
# that for interactive shells. A non-interactive shell (cron, a unit, `bash -c`,
# CI) does not, and internal/membench/guard.go cannot tell "no denials
# configured" from "this deployment has nothing to deny" — it treats an unset
# variable as an empty list, silently.
#
# That asymmetry is deliberate there (the variable may only ADD denials, so
# forgetting it can never remove a protection).
#
# WARNS RATHER THAN REFUSES, changed on review (2026-09-04, finding c). The
# reviewer's argument is correct on the merits: this check can only see that the
# variable is NON-EMPTY, not that it names this deployment's database, so as a
# gate it is a checkbox — and the positive write-target check above already
# covers this script's actual hazard, which makes a refusal here pure friction
# with a false-negative failure mode of its own. What it can still do honestly
# is SAY that a protection other paths rely on is not loaded, which is the part
# that was silent everywhere.
bench_assert_deny_list_loaded() {
    if [ -z "${VORNIK_BENCH_DENY_DATABASES:-}" ]; then
        echo "warning: VORNIK_BENCH_DENY_DATABASES is not set, so this deployment's own" >&2
        echo "  database is not on the harness denylist. The write-target check below still" >&2
        echo "  applies; other entry points to \`vornikctl bench\` do not have it. To load it:" >&2
        echo "    . \"\${VORNIK_CONFIG_DIR:-\$HOME/.config/vornik}/shell-env.sh\"" >&2
        echo "  (an interactive shell gets it from ~/.bashrc.d; a script or cron job does not)." >&2
    fi
    return 0
}

#!/usr/bin/env bash
# Reproduce an agent-quality benchmark run.
#
# Pins every axis that changes a result, and REFUSES rather than guessing when
# one is missing. Nothing here is specific to the maintainers' machine: it needs
# a Vornik daemon dedicated to benchmarking, a database that daemon writes, and a
# companion token scoped to the benchmark project.
#
# WHY A SECOND DAEMON IS NOT OPTIONAL. The harness asks the daemon which database
# it writes (companion `whoami`) and refuses on mismatch. Pointing a production
# daemon at a differently-named project is therefore refused — correctly. That
# check exists because on 2026-08-12 a run naming a throwaway database wrote
# twelve documents into a production corpus and left the named one empty.
#
# Usage:
#   export VORNIK_URL=http://127.0.0.1:8090            # the BENCH daemon
#   export VORNIK_COMPANION_TOKEN=<vornikctl companion grant -p agentbench ...>
#   export VORNIK_BENCH_DSN="host=... dbname=... user=... password=... sslmode=disable"
#   scripts/agentbench-reproduce.sh --arm baseline [--repeats 3] [--tasks <file>]
#
# See https://docs.vornik.io for what
# each figure means and why it is shaped the way it is.
set -euo pipefail

TASKS="internal/agentbench/tasksets/dev-swarm-tasks-v1.json"
PROJECT="agentbench"
SWARM="dev-swarm"
ARM=""
REPEATS=1
PREREG=""
OUTDIR="./agentbench-runs"

while [ $# -gt 0 ]; do
    case "$1" in
        --arm)             ARM="$2"; shift 2 ;;
        --tasks)           TASKS="$2"; shift 2 ;;
        --repeats)         REPEATS="$2"; shift 2 ;;
        --preregistration) PREREG="$2"; shift 2 ;;
        --project)         PROJECT="$2"; shift 2 ;;
        --swarm)           SWARM="$2"; shift 2 ;;
        --out)             OUTDIR="$2"; shift 2 ;;
        *) echo "unknown flag: $1" >&2; exit 2 ;;
    esac
done

fail() { echo "error: $*" >&2; exit 1; }

[ -n "$ARM" ]                          || fail "--arm is required: a run that does not name its arm cannot be compared with anything"
[ -n "${VORNIK_URL:-}" ]               || fail "VORNIK_URL must point at the BENCH daemon, not production"
[ -n "${VORNIK_COMPANION_TOKEN:-}" ]   || fail "VORNIK_COMPANION_TOKEN must be a token scoped to the benchmark project"
[ -n "${VORNIK_BENCH_DSN:-}" ]         || fail "VORNIK_BENCH_DSN must name the database the bench daemon writes"
[ -f "$TASKS" ]                        || fail "task set not found: $TASKS"

# The database the daemon SAYS it writes. The harness re-checks this itself and
# refuses on mismatch; asking here too turns a late refusal into an early, clearer
# one.
DB=$(curl -sS -X POST "${VORNIK_URL%/}/api/v1/mcp/companion" \
        -H "Authorization: Bearer $VORNIK_COMPANION_TOKEN" \
        -H "Content-Type: application/json" \
        -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami","arguments":{}}}' \
     | python3 -c 'import sys,json; print(json.loads(json.load(sys.stdin)["result"]["content"][0]["text"]).get("database",""))')
[ -n "$DB" ] || fail "the daemon at $VORNIK_URL did not report a write target; the harness fails closed on this and so does this script"

case "$DB" in
    prod|production|live) fail "the daemon at $VORNIK_URL writes '$DB'. Refusing." ;;
esac
echo "daemon at $VORNIK_URL writes: $DB"

# A pre-registration is REQUIRED by the harness. Generating a placeholder here
# would defeat it, so this script only reminds you what it must contain.
if [ -z "$PREREG" ]; then
    fail "--preregistration is required. Commit a file BEFORE the run stating the
  arms compared, the metric, the intended delta, the measured sigma_d and the n it
  came from, the computed pair count, and why the comparison is worth spending on.
  Choosing what to compare after seeing results is the line between a benchmark
  and a press release, which is why the harness will not run without one."
fi
[ -f "$PREREG" ] || fail "pre-registration not found: $PREREG"

HASH=$(vornikctl bench agent taskset-hash "$TASKS" | awk '{print $1}')
echo "task set: $TASKS"
echo "digest:   $HASH"
echo "arm:      $ARM   repeats: $REPEATS"

mkdir -p "$OUTDIR"
JOURNAL="$OUTDIR/${ARM}-$(date -u +%Y%m%dT%H%M%SZ).json"

vornikctl bench agent run \
    --project "$PROJECT" --benchmark-project "$PROJECT" --swarm "$SWARM" \
    --database "$DB" --i-know-this-wipes "$DB" \
    --tasks "$TASKS" --preregistration "$PREREG" \
    --arm "$ARM" --run-id "$ARM-$(date -u +%s)" --repeats "$REPEATS" \
    --journal "$JOURNAL"

echo
vornikctl bench agent rollup "$JOURNAL"
echo
echo "journal: $JOURNAL"
echo
echo "A SINGLE RUN IS NOT A RESULT. Collect at least 10 before quoting a sigma —"
echo "an sigma from a handful of runs is a direction, not a verdict, and"
echo "underestimating spread manufactures significance because it is the"
echo "denominator. The harness refuses to gate on fewer."

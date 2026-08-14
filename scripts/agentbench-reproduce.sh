#!/usr/bin/env bash
# Run or reproduce an agent-quality benchmark pass.
#
# Every check below exists because it failed in practice on 2026-08-14. Nothing
# here is defensive programming for its own sake; each refusal maps to a run that
# produced a wrong number, spent money nobody authorised, or silently measured
# nothing. See https://docs.vornik.io
# §12 for the incident record.
#
# MODES
#   --smoke     ONE task, ONE run — prove the pipeline before spending on a full pass
#   --gold      record ground truth from an unrestricted-ceiling arm
#   (default)   score an arm against pinned gold and journal the verdicts
#
# ALWAYS SMOKE FIRST. The preflight below checks configuration; only a real run
# proves the pipeline. Three full gold passes were abandoned mid-flight on
# 2026-08-14 — a dirty workspace, a Bedrock router fallback, a Bedrock ROLE
# fallback — each of which a single task would have exposed for ~1/54th of the
# tokens. The preflight now covers all three, but the next fault will be one it
# does not know about, which is exactly what smoke is for.
#
# USAGE
#   export VORNIK_URL=http://127.0.0.1:8090            # the BENCH daemon, not production
#   export VORNIK_COMPANION_TOKEN=<vornikctl companion grant -p agentbench --client claude-code ...>
#   export VORNIK_BENCH_DSN="host=... dbname=... user=... password=... sslmode=disable"
#
#   scripts/agentbench-reproduce.sh --gold --runs 3
#   scripts/agentbench-reproduce.sh --arm baseline --preregistration prereg.json \
#       --context-policy "suppression=none;advert=gated" --gold gold.json
set -euo pipefail

TASKS="internal/agentbench/tasksets/dev-swarm-tasks-v1.json"
PROJECT="agentbench"; SWARM="dev-swarm"
ARM=""; PREREG=""; POLICY=""; GOLD=""; RUNS=3; REPEATS=1; BATCH=3
OUTDIR="./agentbench-runs"; MODE="run"
DAEMON_BINARY="${VORNIK_BENCH_BINARY:-$HOME/.local/bin/vornik-enterprise-bench}"
DAEMON_CONFIG="${VORNIK_BENCH_CONFIG:-$HOME/.config/vornik-bench/config.yaml}"
WORKSPACE="${VORNIK_BENCH_WORKSPACE:-$HOME/.local/share/vornik-bench/workspaces/agentbench}"

while [ $# -gt 0 ]; do
    case "$1" in
        --smoke)           MODE="smoke"; shift ;;
        --gold)            MODE="gold"; shift ;;
        --arm)             ARM="$2"; shift 2 ;;
        --preregistration) PREREG="$2"; shift 2 ;;
        --context-policy)  POLICY="$2"; shift 2 ;;
        --gold-manifest)   GOLD="$2"; shift 2 ;;
        --tasks)           TASKS="$2"; shift 2 ;;
        --runs)            RUNS="$2"; shift 2 ;;
        --batch-size)      BATCH="$2"; shift 2 ;;
        --repeats)         REPEATS="$2"; shift 2 ;;
        --project)         PROJECT="$2"; shift 2 ;;
        --swarm)           SWARM="$2"; shift 2 ;;
        --daemon-binary)   DAEMON_BINARY="$2"; shift 2 ;;
        --daemon-config)   DAEMON_CONFIG="$2"; shift 2 ;;
        --workspace)       WORKSPACE="$2"; shift 2 ;;
        --out)             OUTDIR="$2"; shift 2 ;;
        *) echo "unknown flag: $1" >&2; exit 2 ;;
    esac
done

fail() { echo "error: $*" >&2; exit 1; }
note() { echo "  ok  $*"; }

[ -n "${VORNIK_URL:-}" ]             || fail "VORNIK_URL must point at the BENCH daemon, not production"
[ -n "${VORNIK_COMPANION_TOKEN:-}" ] || fail "VORNIK_COMPANION_TOKEN must be scoped to the benchmark project"
[ -n "${VORNIK_BENCH_DSN:-}" ]       || fail "VORNIK_BENCH_DSN must name the database the bench daemon writes"
[ -f "$TASKS" ]                      || fail "task set not found: $TASKS"

echo ">> preflight"

# 1. WHERE THE WRITES LAND. Naming a database proves nothing: the harness reaches a
#    running daemon and the daemon writes wherever it was configured. On 2026-08-12
#    that gap put twelve fixture documents into a production corpus.
DB=$(curl -sS -X POST "${VORNIK_URL%/}/api/v1/mcp/companion" \
        -H "Authorization: Bearer $VORNIK_COMPANION_TOKEN" -H "Content-Type: application/json" \
        -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami","arguments":{}}}' \
     | python3 -c 'import sys,json; print(json.loads(json.load(sys.stdin)["result"]["content"][0]["text"]).get("database",""))')
[ -n "$DB" ] || fail "the daemon at $VORNIK_URL did not report a write target; the harness fails closed on this and so does this script"
case "$DB" in prod|production|live) fail "the daemon at $VORNIK_URL writes '$DB'. Refusing." ;; esac
note "daemon writes: $DB"

# 2. COST RECORDING. The bench config tree was created without pricing.yaml, so every
#    cost_usd read 0.0000 and ~\$10 of real spend went unrecorded — and was reported
#    as "free". A zero you cannot distinguish from "unpriced" is not a measurement.
CFGDIR=$(dirname "$DAEMON_CONFIG")/configs
if [ -f "$CFGDIR/pricing.yaml" ]; then note "pricing.yaml present — cost will be recorded"
else fail "no pricing.yaml in $CFGDIR: every cost_usd will record 0.0000 and the run will
  report real spend as free. Copy it from the production config tree."; fi

# 3. PROVIDER CONTROL. A model_fallbacks entry silently served a Bedrock model in place
#    of an Ollama one for 473k tokens. That is unbudgeted spend AND a comparability
#    break: the arm's model set changes mid-run with nothing declaring it.
if python3 -c "
import yaml,sys
r=yaml.safe_load(open('$DAEMON_CONFIG'))['chat']['router']
bad = bool(r.get('model_fallbacks')) or r.get('bedrock',{}).get('enabled')
sys.exit(1 if bad else 0)" 2>/dev/null; then
    note "no router fallbacks, bedrock disabled — provider is pinned"
else
    fail "$DAEMON_CONFIG still has model_fallbacks set or bedrock enabled. A fallback
  crosses providers silently: it spends on a per-request provider without asking, and
  changes the arm's model set mid-run so two runs key alike having measured different
  systems. Set chat.router.model_fallbacks: {} and chat.router.bedrock.enabled: false."
fi

# 3b. ROLE-LEVEL FALLBACKS. The router's model_fallbacks are not the only ones: each
#     swarm role carries its own `modelFallback`. Every one on this deployment pointed
#     at Bedrock. With bedrock disabled the fallback path dies and takes the step with
#     it ("MODEL_UNHEALTHY ... circuit open"); with it enabled they spend per-request
#     and silently change the arm's model set. Point them at a same-provider model.
SWARMFILE="$(dirname "$DAEMON_CONFIG")/configs/swarms/${SWARM}.md"
if [ -f "$SWARMFILE" ]; then
    BADFB=$(grep -oE 'modelFallback:[[:space:]]*"[^"]+"' "$SWARMFILE" \
            | grep -cE '"(zai|openai|moonshotai|minimax|deepseek|anthropic|meta|mistral|amazon|google|qwen|cohere|nvidia|us|global)\.' || true)
    [ "${BADFB:-0}" = "0" ] || fail "$SWARMFILE has $BADFB role modelFallback entr(y|ies) pointing at a
  per-request provider. Either that path spends without asking, or — with the provider
  disabled — the fallback fails and takes the step with it. Point them at a model on the
  same provider as the role's primary."
    note "role fallbacks stay on-provider"
fi

# 4. WORKSPACE STATE. Untracked workflow machinery accumulated across runs and blocked
#    the merge-to-master step from task 3 onward. The agent had succeeded; the
#    benchmark's own workspace was dirty, and gold recorded the task as one the arm
#    could never pass.
if [ -d "$WORKSPACE/.git" ]; then
    DIRTY=$(git -C "$WORKSPACE" status --porcelain | wc -l)
    [ "$DIRTY" = "0" ] || fail "benchmark workspace $WORKSPACE has $DIRTY uncommitted entr(y|ies).
  Runs will fail with 'changes could not be merged to master' and gold will record those
  tasks as ones the unrestricted arm never passed. Commit or ignore them first."
    note "workspace clean"
else
    echo "  --  no git workspace at $WORKSPACE (skipping cleanliness check)"
fi

# 5. IDLE. A benchmark that shares the daemon with live work measures the contention.
INFLIGHT=$(psql "$VORNIK_BENCH_DSN" -tAc "SELECT count(*) FROM tasks WHERE status IN ('RUNNING','LEASED','QUEUED');" 2>/dev/null || echo "?")
[ "$INFLIGHT" = "0" ] || [ "$INFLIGHT" = "?" ] || fail "$INFLIGHT task(s) already in flight on the bench daemon; wait for idle"
note "bench idle"

HASH=$(vornikctl bench agent taskset-hash "$TASKS" | awk '{print $1}')
note "task set: $(basename "$TASKS") @ ${HASH:0:12}  ($(python3 -c "import json;print(len(json.load(open('$TASKS'))))") tasks)"
mkdir -p "$OUTDIR"

if [ "$MODE" = "smoke" ]; then
    SMOKE_TASKS="$OUTDIR/smoke-tasks.json"
    python3 -c "
import json,sys
t=json.load(open('$TASKS'))
json.dump(t[:1], open('$SMOKE_TASKS','w'), indent=2)
print('  ok  smoke task:', t[0]['id'], '(' + t[0]['workflow'] + ')')
"
    SMOKE_GOLD="$OUTDIR/smoke-gold.json"; rm -f "$SMOKE_GOLD"
    # Marker so the spend below is THIS run's, not whatever else touched the
    # ledger recently. A window query would happily credit another run's tokens
    # and report a wired pricing table that is not wired.
    SMOKE_T0=$(psql "$VORNIK_BENCH_DSN" -tAc "SELECT now()" 2>/dev/null)
    echo ">> smoke: 1 task, 1 unrestricted run on swarm '$SWARM'"
    vornikctl bench agent gold \
        --project "$PROJECT" --benchmark-project "$PROJECT" --swarm "$SWARM" \
        --database "$DB" --i-know-this-wipes "$DB" \
        --tasks "$SMOKE_TASKS" --task-set-hash "smoke-$HASH" --runs 1 \
        --gold "$SMOKE_GOLD" >/dev/null || fail "smoke run failed — fix before spending on a full pass"

    # The pipeline is proven only if the run produced EVIDENCE, not merely exited 0.
    # A pass that records nothing looks identical to a clean one in the exit code.
    python3 - "$SMOKE_GOLD" <<'SMOKE'
import json, sys
m = json.load(open(sys.argv[1]))
entries = m.get("entries") or []
if not entries:
    sys.exit("smoke produced an EMPTY gold manifest: the run exited 0 and measured nothing")
e = entries[0]
if e.get("excluded"):
    sys.exit(f"smoke task was excluded ({e.get('excludedReason')}). "
             "A full pass would exclude it 54 times over.")
paths = e.get("paths") or []
if not paths or not paths[0]:
    sys.exit("smoke recorded no tool path: gold would be empty for every task")
print(f"  ok  gold recorded {len(paths[0])} tool(s) for {e['taskId']}")
SMOKE

    SPEND=$(psql "$VORNIK_BENCH_DSN" -tAc "SELECT coalesce(round(sum(cost_usd)::numeric,4),0) FROM task_llm_usage WHERE recorded_at > '$SMOKE_T0';" 2>/dev/null || echo "?")
    [ "$SPEND" = "0.0000" ] && fail "smoke recorded \$0.0000 spend: pricing is not wired, so a full
  pass would report real spend as free"
    echo "  ok  cost recorded for this smoke: \$$SPEND"
    echo "      a full pass is ~54x one task-run; budget accordingly"
    echo
    echo "SMOKE CLEAN on '$SWARM'. Re-run with --gold for the full pass."
    exit 0
fi

if [ "$MODE" = "gold" ]; then
    # Refuse a full pass that has not been smoked on this swarm. 54 runs is not the
    # place to discover a config fault.
    [ -f "$OUTDIR/smoke-gold.json" ] || fail "no smoke result in $OUTDIR for swarm '$SWARM'.
  Run with --smoke first: one task, one run, ~1/54th of the tokens. Three full passes
  were abandoned mid-flight for faults a single run would have surfaced."

    NTASKS=$(python3 -c "import json;print(len(json.load(open('$TASKS'))))")
    NBATCH=$(( (NTASKS + BATCH - 1) / BATCH ))
    echo ">> gold pass: $NTASKS tasks x $RUNS run(s), in $NBATCH batch(es) of $BATCH"
    echo "   a dropped session costs ONE batch; re-running skips batches already on disk"

    BATCHFILES=()
    for ((b=0; b<NBATCH; b++)); do
        BFILE="$OUTDIR/gold-batch-$b.json"
        BTASKS="$OUTDIR/batch-$b-tasks.json"
        BATCHFILES+=("$BFILE")
        if [ -f "$BFILE" ]; then
            echo "   batch $b: already recorded, skipping"
            continue
        fi
        python3 -c "
import json
t=json.load(open('$TASKS'))
json.dump(t[$b*$BATCH:($b+1)*$BATCH], open('$BTASKS','w'), indent=2)
print('   batch $b:', ', '.join(x['id'] for x in t[$b*$BATCH:($b+1)*$BATCH]))
"
        vornikctl bench agent gold \
            --project "$PROJECT" --benchmark-project "$PROJECT" --swarm "$SWARM" \
            --database "$DB" --i-know-this-wipes "$DB" \
            --tasks "$BTASKS" --task-set-hash "batch$b-$HASH" --runs "$RUNS" \
            --gold "$BFILE" >/dev/null || fail "batch $b failed. Fix, then re-run: completed
  batches are skipped, so only batch $b is repeated."
        echo "   batch $b: recorded"
    done

    # Batches were hashed per-batch so each is independently pinned; the merged
    # manifest carries the WHOLE task set's hash, which is what the fence compares.
    python3 - "$OUTDIR" "$HASH" "${BATCHFILES[@]}" <<'REHASH'
import json, sys
outdir, full_hash, files = sys.argv[1], sys.argv[2], sys.argv[3:]
for f in files:
    m = json.load(open(f))
    m["taskSetSha256"] = full_hash
    json.dump(m, open(f, "w"), indent=2)
REHASH

    vornikctl bench agent gold-merge "${BATCHFILES[@]}" --out "${GOLD:-$OUTDIR/gold.json}"
    echo
    echo "Gold is OPERATOR-REVIEWED before it gates anything: it defines what 'correct'"
    echo "means, so the harness that produced it cannot certify it."
    exit 0
fi

# --- scoring run ---------------------------------------------------------------
[ -n "$ARM" ]    || fail "--arm is required: a run that does not name its arm cannot be compared"
[ -n "$POLICY" ] || fail "--context-policy is required: it names the independent variable"
[ -n "$PREREG" ] || fail "--preregistration is required. Commit a file BEFORE the run stating the
  arms compared, the metric, the intended delta, the measured sigma_d and the n it came
  from, the computed pair count, and why the comparison is worth spending on. Choosing
  what to compare after seeing results is the line between a benchmark and a press release."
[ -f "$PREREG" ] || fail "pre-registration not found: $PREREG"

JOURNAL="$OUTDIR/${ARM}-$(date -u +%Y%m%dT%H%M%SZ).json"
vornikctl bench agent run \
    --project "$PROJECT" --benchmark-project "$PROJECT" --swarm "$SWARM" \
    --database "$DB" --i-know-this-wipes "$DB" \
    --tasks "$TASKS" --preregistration "$PREREG" \
    --context-policy "$POLICY" \
    --daemon-binary "$DAEMON_BINARY" --daemon-config "$DAEMON_CONFIG" \
    ${GOLD:+--gold "$GOLD"} \
    --arm "$ARM" --run-id "$ARM-$(date -u +%s)" --repeats "$REPEATS" \
    --journal "$JOURNAL"

echo
vornikctl bench agent rollup "$JOURNAL"
echo
echo "journal: $JOURNAL"
echo
echo "A SINGLE RUN IS NOT A RESULT, and neither are four. Collect at least 10 before"
echo "quoting a sigma: the n=4 figure for schema conformance was 4.3x too small and"
echo "implied a gate twelve times cheaper than the real one."

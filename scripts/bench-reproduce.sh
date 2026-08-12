#!/usr/bin/env bash
# Reproduce a published Vornik memory benchmark result.
#
# Pins every axis that changes a result, so a run either lands on the published
# comparability key or tells you which axis moved. Nothing here is specific to the
# maintainers' machine: it needs a Vornik daemon, a companion key, and a database
# that is NOT your production one.
#
# Usage:
#   export VORNIK_URL=http://127.0.0.1:8080
#   export VORNIK_COMPANION_TOKEN=<vornikctl companion grant ... --memory-all>
#   scripts/bench-reproduce.sh --dataset-path ./longmemeval_oracle.json \
#       [--items 6] [--category temporal-reasoning] [--runs 3] [--database NAME]
#
# --database must name the database YOUR daemon actually writes. The harness asks the
# daemon and refuses when the two disagree, so a wrong value fails loudly rather than
# writing somewhere you did not intend. Set VORNIK_BENCH_DATABASE to avoid repeating it.
#
# The dataset is NOT vendored — it is 15 MB (oracle) to 265 MB (_s) and belongs to
# its authors. Fetch it first:
#   curl -sLO https://huggingface.co/datasets/xiaowu0162/longmemeval-cleaned/resolve/main/longmemeval_oracle.json
#
# Note the repo name: `longmemeval-cleaned` is the September 2025 revision. The
# older `longmemeval` repo is DEPRECATED and its numbers are not comparable.
set -euo pipefail

DATASET_PATH=""
ITEMS=6
CATEGORY="temporal-reasoning"
DATABASE="${VORNIK_BENCH_DATABASE:-vornik_bench}"
RUNS=3
RUN_ROOT="${VORNIK_BENCH_RUN_DIR:-./bench-runs}"

while [ $# -gt 0 ]; do
  case "$1" in
    --dataset-path) DATASET_PATH="$2"; shift 2 ;;
    --items)        ITEMS="$2"; shift 2 ;;
    --category)     CATEGORY="$2"; shift 2 ;;
    --database)     DATABASE="$2"; shift 2 ;;
    --runs)         RUNS="$2"; shift 2 ;;
    -h|--help)      sed -n '2,20p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[ -n "$DATASET_PATH" ] || { echo "error: --dataset-path is required (see --help)" >&2; exit 2; }
[ -f "$DATASET_PATH" ] || { echo "error: no such dataset file: $DATASET_PATH" >&2; exit 2; }
[ -n "${VORNIK_URL:-}" ] || { echo "error: set VORNIK_URL to your daemon" >&2; exit 2; }
[ -n "${VORNIK_COMPANION_TOKEN:-}" ] || {
  echo "error: set VORNIK_COMPANION_TOKEN — mint one with:" >&2
  echo "  vornikctl companion grant --project <p> --client claude-code --memory-all" >&2
  exit 2; }

VORNIKCTL="${VORNIKCTL:-vornikctl}"
command -v "$VORNIKCTL" >/dev/null || { echo "error: $VORNIKCTL not on PATH" >&2; exit 2; }

# The digest pins the dataset REVISION. Results are not comparable across revisions,
# and the harness verifies this rather than trusting the filename.
if command -v sha256sum >/dev/null; then
  HASH="$(sha256sum "$DATASET_PATH" | cut -d' ' -f1)"
else
  HASH="$(shasum -a 256 "$DATASET_PATH" | cut -d' ' -f1)"
fi

echo "dataset : $DATASET_PATH"
echo "sha256  : $HASH"
echo "items   : $ITEMS (first $ITEMS of category '"'"'$CATEGORY'"'"', in dataset file order)"
echo "database: $DATABASE"
echo "runs    : $RUNS"
echo

# Repeated runs, because a mean without a spread is not a result. Anything whose
# ingest involves a model varies run to run, and one run cannot show you that.
DIRS=()
for i in $(seq 1 "$RUNS"); do
  DIR="$RUN_ROOT/reproduce-$i"
  rm -rf "$DIR"
  echo "--- run $i/$RUNS ---"
  # --tier2-only        judge-free: no answer model, no judge, no cloud credentials
  # --accept-unverified-path  measure the system AS SHIPPED (reranker in the path).
  #                     Omit it to require a provably deterministic retrieval path,
  #                     which is what a CI gate wants and what a product comparison
  #                     cannot have.
  "$VORNIKCTL" bench memory run \
    --system vornik \
    --dataset longmemeval \
    --dataset-path "$DATASET_PATH" \
    --dataset-sha256 "$HASH" \
    --database "$DATABASE" \
    --i-know-this-wipes "$DATABASE" \
    --tier2-only \
    --accept-unverified-path \
    --category "$CATEGORY" \
    --max-items "$ITEMS" \
    --max-tokens 4096 \
    --run-dir "$DIR"
  DIRS+=("$DIR")
done

echo
echo "=== aggregate (mean, spread, and the gate tolerance each metric needs) ==="
"$VORNIKCTL" bench memory aggregate "${DIRS[@]}"

echo
echo "Compare the comparability key above with the published one. If they differ, the"
echo "runs measured different things — the key names which axis moved."

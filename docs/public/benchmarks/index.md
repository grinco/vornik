# Benchmarks

Vornik is measured, not asserted. This section states what we measure, how to
reproduce it, and where Vornik loses.

Two benchmarks, answering different questions:

- **Memory / retrieval** — how well the memory subsystem finds what matters,
  against a public dataset. Most of this page. Independently reproducible.
- **Agent quality** — the decisions the control logic makes: what the lead
  granted, whether roles followed their output schemas, whether agents called
  tools correctly. Reported in
  [Results by release](results.md#agent-quality--202690-first-scored-arm).
  **Not independently reproducible** — its task set and answer key are not
  published — so it is reported with that limitation stated rather than implied.

Every number here is produced by `vornikctl bench memory` or `vornikctl bench
agent`, which ship with the product. Nothing is hand-transcribed from a spreadsheet, and every result carries a
**comparability key** — a digest of the dataset revision, the models involved, the
retrieval budget and the prompts — so two numbers that are not comparable refuse to
be compared.

## What we lead with, and why

Retrieval quality is reported in two tiers, never blended.

**Tier 2 — judge-free.** Context recall, context precision and MRR, computed by
comparing retrieved document ids against the dataset's gold labels. No model is in
the loop, so these are reproducible indefinitely and independent of any model's
lifecycle. **This is the primary result.**

**Tier 1 — judged accuracy.** An LLM answers from the retrieved context and a second
LLM grades the answer. Useful, and supporting only: measured judge variance is
~4.5% standard deviation at n=30, which is enough to manufacture or erase a "win".

We lead with tier 2 because a benchmark whose headline number depends on a hosted
proprietary judge stops being reproducible the day that judge is retired. Any judged
figure we publish names an **open-weight, version-pinned** judge so an external
auditor can re-run it.

## Current results

See [Results by release](results.md) for the tracked history — the LongMemEval
retrieval rows, and the agent-quality arm.

**The agent figures name their model on purpose.** They are measured on a
self-hosted 27B open-weight model on a single machine, which is what a
self-hosting user actually runs, rather than on a large hosted model that would
report a smoother number. The gap between the two is stated alongside the
result.

## Reproducing a result

The harness needs a running Vornik daemon, a companion key with memory access, and a
database that is **not** your production one.

```bash
# 1. The dataset is not vendored — it belongs to its authors. Note the repo name:
#    `longmemeval-cleaned` is the Sept 2025 revision. The older `longmemeval` repo is
#    DEPRECATED and its numbers are not comparable to these.
curl -sLO https://huggingface.co/datasets/xiaowu0162/longmemeval-cleaned/resolve/main/longmemeval_oracle.json

# 2. A key scoped to a throwaway project.
vornikctl companion grant --project bench --client claude-code --memory-all

export VORNIK_URL=http://127.0.0.1:8080
export VORNIK_COMPANION_TOKEN=<the secret printed above>

# 3. Name the database YOUR daemon writes. The harness asks the daemon and refuses
#    when the two disagree, so a wrong value fails loudly instead of writing
#    somewhere you did not intend.
scripts/bench-reproduce.sh --dataset-path ./longmemeval_oracle.json \
    --database my_bench_db --category temporal-reasoning --items 6 --runs 3
```

**Run it against a bench deployment, not production.** The harness bulk-writes a
corpus. A bench daemon wants its own database, its own listener, and a config tree
containing only autonomy-off projects — a daemon that loads a production project tree
against a throwaway database will happily start doing that project's real work.

**Item selection is deterministic:** the first N items of the named category, in
dataset file order — or, for a run spanning every ability, the first N of *each*
category. The six-item rows list the question ids they scored, so a reproduction can
confirm it measured the same questions rather than inferring it from a count. The
120-item row does not list 120 ids; its selection rule is exact instead
(`--max-items-per-category 20`, no `--category` filter), and the ids it actually
scored are in that run's journal.

**Warm and cold are different experiments.** Until 2026-08-21 the harness did not
perform the store clear its own flag promised, so repeated runs accumulated a corpus.
It now clears before every run, unconditionally. Rows taken before that date are warm
and are marked closed; the tracked axis is cold. `bench memory aggregate` and the
comparability key both refuse to mix the two.

`scripts/bench-reproduce.sh` pins every axis that changes a result and prints the
comparability key it produced. If your key differs from a published one, the runs are
measuring different things and the script says so rather than letting you compare
them.

### Two guards you will meet

Both exist because they caught real incidents, and neither can be skipped silently.

**It refuses to write the wrong database.** The harness bulk-writes a corpus, so it
asks the daemon which database it *actually* writes to and compares that with the one
you named. It fails closed when it cannot get an answer:

```
error: refusing to run: you named "vornik_bench" but the "vornik" system actually
writes "production_db"
```

**It waits for ingest to finish.** Embedding is asynchronous. A run that recalls
while the queue is still draining measures keyword-only retrieval and reports it as
though it were semantic — which is exactly how an early internal comparison produced
a recall figure 8 points too low. The harness now waits for the ingest queue to
empty, and refuses if it never does.

## What these numbers do not tell you

- **Not a marketing claim.** Absolute scores are on *our* scale, under *our* judge
  and *our* retrieval budget. Do not compare them against a figure published
  elsewhere under different conditions.
- **Tier 2 cannot measure abstention.** For questions whose answer is deliberately
  absent from the corpus there is no gold document to retrieve, so those items are
  excluded from the mean rather than scored zero. Abstention is judged-only.
- **Small n is small.** Every result states its item count. A six-item screen is a
  smoke test, not a benchmark.
- **Chunking asymmetry is real.** Long sessions are split on turn boundaries at the
  per-deposit size cap. Where a comparison system does not split, the two systems saw
  differently-shaped inputs; the harness records this as a method note.

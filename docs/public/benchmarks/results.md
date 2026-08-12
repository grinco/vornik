# Results by release

One row per release, per benchmark. Rows are never edited after a release ships — a
regression is shown by the next row being worse, not by the previous row changing.

Each row states its item count and standard deviation across repeated runs. A mean
without a spread is not a result: two systems whose intervals overlap have not been
separated by the measurement.

## LongMemEval — retrieval (tier 2, judge-free)

**Reproduce:**

```bash
curl -sLO https://huggingface.co/datasets/xiaowu0162/longmemeval-cleaned/resolve/main/longmemeval_oracle.json
scripts/bench-reproduce.sh --dataset-path ./longmemeval_oracle.json \
    --category temporal-reasoning --items 6 --runs 3 --database <your-bench-db>
```

Item selection is deterministic: the first N items of the named category, in dataset
file order. The rows below scored `gpt4_2655b836`, `gpt4_2487a7cb`, `gpt4_76048e76`,
`gpt4_2312f94c`, `0bb5a684`, `08f4fc43` — listed so a reproduction can confirm it
measured the same questions rather than inferring it from a count.

**Dataset:** LongMemEval v1-cleaned (`xiaowu0162/longmemeval-cleaned`, Sept 2025),
`longmemeval_oracle.json`, `sha256 821a2034d219ab45846873dd14c14f12cfe7776e73527a483f9dac095d38620c`.
**Budget:** `max_tokens=4096` both arms. **Judge:** none — tier 2 is judge-free.

| Release | Items | Ability | Context recall | Context precision | MRR | Runs |
|---|---|---|---|---|---|---|
| `2026.8.3-13-g13476b8f` (baseline, pre-tracking) | 6 | temporal-reasoning | 1.0000 ±0.0000 | 0.9444 ±0.0000 | 1.0000 ±0.0000 | n=3 |

Comparability key `d0fa4f0f8389`. Produced by the `scripts/bench-reproduce.sh`
invocation above, on the binary named in the Release column. All three metrics are
**deterministic** across repeated runs — sd exactly 0.0000 — so this row can be gated
on exact equality rather than a tolerance.

Tracking starts with the next tagged release. This first row is a **baseline taken
before tracking began**, recorded so the first tracked release has something to be
compared against.

#### Re-ingesting the corpus moves the numbers slightly

The row above repeats the run against an **already-ingested** corpus, which is what
the reproduction script does. Clearing the database and re-ingesting before every run
instead gives MRR 0.9444 ±0.0481 (n=3) on the same six items, with recall and
precision unchanged.

The cause is worth knowing before you compare two numbers: ingest runs an LLM titler
over each chunk, and it does not produce identical titles every time. Retrieval is
deterministic *given a corpus*; the corpus is not identical across re-ingests. So
compare warm-to-warm or cold-to-cold, never one against the other.

## LongMemEval — judged accuracy (tier 1) and the judge's noise floor

**A judge with an unmeasured noise floor is not a standard.** Before any judged number
can be gated or quoted as a win, the variance of the judge itself has to be known —
otherwise a "win" is indistinguishable from the judge disagreeing with itself.

Ten identical runs, same items, same models, warm corpus:

| Metric | Mean | sd | Min | Max | Narrowest defensible gate (3σ) |
|---|---|---|---|---|---|
| **judged accuracy** | **0.8241** | **0.0340** | 0.7500 | 0.8750 | **±0.1020** |
| context recall | 0.9764 | 0.0059 | 0.9722 | 0.9861 | ±0.0176 |
| context precision | 0.9410 | 0.0000 | 0.9410 | 0.9410 | exact |
| MRR | 0.9917 | 0.0108 | 0.9792 | 1.0000 | ±0.0323 |

n=10 runs × 24 items, `temporal-reasoning`, comparability key `a2fd4d245b21`.
**Judge:** `gemma4:31b`. **Answerer:** `gpt-oss:120b` — a different model family, so the
judge is not grading its own output. Both open-weight and version-pinned, served from
a single provider (Ollama Cloud). A multi-provider router was deliberately avoided:
switching backends between calls would inject variance into the very number being
measured.

**What this means in practice.** Judged accuracy moved between 0.750 and 0.875 with
nothing changing but the models' own sampling — 3 of 24 items flipping verdict. A
tier-1 gate would therefore need a **±10.2 point** threshold to avoid firing on noise,
which is wider than most real improvements. That is the entire argument for leading
with tier 2: on the same runs, context precision was exactly constant.

Note that context recall is **not** deterministic here (sd 0.0059) although it is on
the tier-2 rows above. These judged runs measure the system **as shipped**, with the
LLM reranker in the retrieval path; the tier-2 rows measure the RRF path, which has no
model in it. Same product, two paths, and only one of them can carry an
exact-equality gate.

### Reproducing the judge-variance figure

```bash
export VORNIK_BENCH_LLM_URL=https://ollama.com/v1
export VORNIK_BENCH_LLM_KEY=<your key>
for i in $(seq 1 10); do
  vornikctl bench memory run --system vornik --dataset longmemeval \
    --dataset-path ./longmemeval_oracle.json \
    --database <your-bench-db> --i-know-this-wipes <your-bench-db> \
    --answer-model gpt-oss:120b --judge-model gemma4:31b \
    --category temporal-reasoning --max-items 24 --max-tokens 4096 \
    --run-dir ./jv-$i
done
vornikctl bench memory aggregate ./jv-*
```

Roughly 4.5 minutes per run on the reference deployment; the first run is much slower
because it ingests the corpus.

### Comparison: hindsight 0.9.0, same items, same budget

Measured under identical conditions on the same machine, each system **as it ships**
— both with their own rerankers active. Recorded because a comparison that only
flatters us is worth less than one that is obviously honest.

| System | Context recall | Context precision | MRR | Runs |
|---|---|---|---|---|
| Vornik | 1.0000 ±0.0000 | **0.9444 ±0.0000** | **0.9444 ±0.0481** | n=3 |
| hindsight 0.9.0 | 1.0000 ±0.0000 | 0.6806 ±0.0833 | 0.7292 ±0.1250 | n=4 |

Both arms are **cold** here — database truncated / bank deleted before every run — so
the two are measured the same way. That is why Vornik's MRR reads 0.9444 ±0.0481 in
this table and 1.0000 ±0.0000 in the release row above: the release row repeats
against a warm corpus, as the reproduction script does. Comparing a warm number with
a cold one would flatter whichever arm was warm.

**Recall is tied at 1.000.** Both systems retrieved every gold document on all six
items, so this subset does not separate them on recall — it is the *oracle* haystack,
which carries about three sessions per item where the full `longmemeval_s` carries
38–62. Expect recall to separate the systems on the larger haystack, and treat a tie
on an easy subset as a statement about the subset.

Vornik's retrieved set is tighter (precision 0.944 vs 0.681) and better ordered
(MRR 0.944 vs 0.729). hindsight reaches the same recall by returning more.

**One difference matters more than the scores.** Vornik's tier-2 figures are
*deterministic*: recall and precision have standard deviation exactly 0.0000 across
repeated runs, because its ingest path is chunk-and-embed with no model deciding what
gets stored. hindsight's precision and MRR vary run to run (sd 0.083 and 0.125, with
MRR ranging 0.667–0.917 across four runs) because its ingest consolidates memories
through an LLM, so the corpus itself differs each time.

That has a practical consequence: **a single hindsight run cannot be quoted**, and any
comparison against it needs repeated runs and a spread. Vornik can be gated on exact
equality; hindsight would need a ±0.375 MRR tolerance to avoid firing on its own noise.

### Method and honest caveats

- **n=6, one of six abilities.** A smoke-scale result. It exists to prove the pipeline
  end to end and to establish a baseline, not to rank products.
- Both arms are marked `retrieval_path_unverified` in their comparability keys: each
  system was measured with its own reranker in the path, so neither run establishes a
  deterministic retrieval path. This is the correct setting for a product comparison
  and the wrong one for a CI gate.
- hindsight's internal extraction model was pinned to an open-weight hosted model
  (`google/gemma-4-26b-a4b-it`). Its embedder and reranker were its own bundled local
  defaults. Vornik used its configured local embedder.
- Hindsight banks are deleted between runs. Without that its corpus accumulates and
  precision falls run over run — the first measurement taken before this was wired
  read 0.806 precision against 0.639 on an identical repeat.

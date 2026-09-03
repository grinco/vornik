# Results by release

One row per release, per benchmark. Rows are never edited after a release ships — a
regression is shown by the next row being worse, not by the previous row changing.

Each row states its item count and standard deviation across repeated runs. A mean
without a spread is not a result: two systems whose intervals overlap have not been
separated by the measurement.

## LongMemEval — retrieval (tier 2, judge-free)

**How these rows were produced** — kept as the record of what was run, **not** as a
reproduction recipe. The harness now clears the store before every run, so this command
yields a *cold* result against a table of *warm* ones. What that costs you depends on
your deployment: with the reranker **off** it reproduces these figures exactly
(measured 2026-08-27, sd 0.0000 on all three metrics); with the reranker **on** the
MRR varies run to run. The script cannot control that — it is
`memory.reranker.enabled` on the daemon. See the axis note below.

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

**This axis is CLOSED.** It measures a *warm* corpus — the run repeats against an
already-ingested store. Since `98037e34` (2026-08-21) the harness clears the store
before every run, unconditionally, so a warm repeat can no longer be produced. The
table is kept because a shipped row is history; it is not extended. The open axis is
[all six abilities, cold](#longmemeval-retrieval-all-six-abilities-tier-2-cold-corpus).

| Release | Items | Ability | Context recall | Context precision | MRR | Runs | Key |
|---|---|---|---|---|---|---|---|
| `2026.8.3-13-g13476b8f` (baseline, pre-tracking) | 6 | temporal-reasoning | 1.0000 ±0.0000 | 0.9444 ±0.0000 | 1.0000 ±0.0000 | n=3 | `d0fa4f0f8389` |
| `2026.8.7-48-g0e450f9c` (shipped as **2026.8.8**) | 6 | temporal-reasoning | 1.0000 ±0.0000 | 0.9444 ±0.0000 | 1.0000 ±0.0000 | n=3 | `e865104e9959` |

Both rows were produced by the `scripts/bench-reproduce.sh` invocation above, on the
binary named in the Release column. All three metrics are **deterministic** across
repeated runs — sd exactly 0.0000 — so these rows can be gated on exact equality
rather than a tolerance.

**The two rows carry different comparability keys, and that is stated rather than
smoothed over.** The 2026.8.8 run left `our_extraction_model`, `answer_model` and
`judge_model` empty, which marks its key PARTIAL; the baseline run named them. Every
field that determines *what was measured* matches — harness version, dataset name and
digest, item selection, `max_tokens=4096`, `observed_recall_method=context-assembly`.
So the relationship is "not provably comparable" rather than "not comparable", and the
exact metric equality is offered as the evidence. A reader who wants the strict
reading should treat them as two tables of one row each.

**Why 2026.8.8's row is dated a day before its tag.** The run was taken at
`0e450f9c`, four commits before `2026.8.8`; all four are release-notes and installer
plumbing, touching no code the benchmark exercises. This is recorded rather than
rounded off, because the alternative — writing the tag and hoping — is how a track
record stops being believable.

#### Re-ingesting the corpus moves the numbers slightly

The rows above repeat the run against an **already-ingested** corpus, which is what
the reproduction script did at the time. Clearing the database and re-ingesting before
every run instead gives MRR 0.9444 ±0.0481 (n=3) on the same six items, with recall and
precision unchanged.

The cause was recorded at the time as ingest non-determinism: an LLM titler runs over
each chunk and does not produce identical titles every time, so retrieval is
deterministic *given a corpus* while the corpus is not identical across re-ingests.

**That attribution turned out to be wrong, and is corrected below.** Re-measured
2026-08-27 with the reranker disabled, the same six items are deterministic *cold* —
MRR 1.0000, sd exactly 0.0000 across three runs. The ±0.0481 came from the LLM
reranker reordering results between runs, not from the corpus changing. The original
figure was taken with each system as it ships, reranker active, and the reranker was
never named as the source. The advice to compare warm-to-warm or cold-to-cold still
holds on general grounds; it is just not what produced this particular spread.

**This distinction stopped being a caveat and became the axis break.** Until
2026-08-21 the harness never actually performed the clear that its own flag
(`--i-know-this-wipes`), its guard text and its design all promised — the
authorisation was built, the action was not (`98037e34`). Every run before that date
is warm; every run after it is cold, and there is no flag to opt out. The fix also
established that the missing clear *moved the score*: on three runs of an identical
120 items, admitted deposits fell 426 → 426 → 209 as the store filled, and judged
accuracy moved 0.692 → 0.750 on a manual wipe with nothing else changed — in the
direction that **understates** the system, because a deduped item loses the haystack
it is scored against. Warm numbers are therefore not merely incomparable to cold ones;
the older ones are pessimistic by an amount nobody has bounded.

#### The same six items, measured cold

Taken 2026-08-27 on `2026.8.9-50-g4b343821`, reranker disabled, store cleared before
every run. It is **not** appended to the table above, because that table is warm and
this is cold — appending it is precisely the silent axis change rule 3 forbids.

| Release | Items | Ability | Context recall | Context precision | MRR | Runs | Key |
|---|---|---|---|---|---|---|---|
| `2026.8.9-50-g4b343821` | 6 | temporal-reasoning | 1.0000 ±0.0000 | 0.9444 ±0.0000 | 1.0000 ±0.0000 | n=3 | `e865104e9959` |

**On this subset, warm and cold are identical** — the same three figures, all with sd
exactly 0.0000, as the warm `2026.8.7-48-g0e450f9c` row. The corpus-warmth effect the
section above warns about does not appear here at all once the reranker is out of the
retrieval path.

That reframes the earlier caveat rather than contradicting it. The published cold
figure of **MRR 0.9444 ±0.0481** was measured with each system *as it ships* — LLM
reranker active — so its variance was a model reordering results between runs, not the
corpus differing. Take the model out and the RRF path is deterministic cold, exactly as
it is warm.

> **A gap this exposed, recorded rather than smoothed over.** The warm row above and
> the cold row here carry the **same comparability key** `e865104e9959`.
> `ComparabilityFields` does not encode whether the corpus was warm or cold, so two runs
> on opposite sides of the 2026-08-21 clear fix compare clean and the tooling would
> merge them without complaint. It went unnoticed because on these six items the two
> regimes give identical numbers — but a key that cannot express an axis the
> documentation calls decisive is a guard that looks protective and is not. Tracked as
> a defect; until it is fixed, warm-versus-cold has to be checked by hand against the
> run date.

## LongMemEval — retrieval, all six abilities (tier 2, cold corpus)

**This is the open axis.** It replaces the six-item warm axis above, which the
2026-08-21 clear fix closed. It is a better measurement on both counts that matter:
120 items rather than 6, and all six abilities rather than one — a single ability is a
statement about that ability, not about retrieval.

**Reproduce:**

```bash
curl -sLO https://huggingface.co/datasets/xiaowu0162/longmemeval-cleaned/resolve/main/longmemeval_oracle.json
vornikctl bench memory run --system vornik --dataset longmemeval \
    --dataset-path ./longmemeval_oracle.json \
    --dataset-sha256 821a2034d219ab45846873dd14c14f12cfe7776e73527a483f9dac095d38620c \
    --database <your-bench-db> --i-know-this-wipes <your-bench-db> \
    --tier2-only --accept-unverified-path \
    --max-items-per-category 20 --max-tokens 4096 \
    --run-dir ./run-$i          # repeat for i in 1 2 3
vornikctl bench memory aggregate ./run-*
```

**Dataset:** as above. **Budget:** `max_tokens=4096`. **Judge:** none — tier 2 is
judge-free. **Corpus:** cold; the harness clears the store before each run.

| Release | Items | Abilities | Context recall | Context precision | MRR | Runs | Key |
|---|---|---|---|---|---|---|---|
| `2026.8.8-40-g2698eb67` | 120 | all 6 | 0.9900 ±0.0036 | 0.9854 ±0.0000 | 0.9958 ±0.0000 | n=3 | `93a6e7a0729b` |
| `2026.8.9-50-g4b343821` | 120 | all 6 | 0.9891 ±0.0036 | 0.9854 ±0.0000 | 0.9958 ±0.0000 | n=3 | `93a6e7a0729b` |

**The second row is the first release-over-release comparison this table can
actually support**, and it shows no regression. Precision and MRR are identical to
four decimal places; recall moved −0.0009 against a per-release sd of 0.0036 and a
narrowest-defensible (3σ) threshold of ±0.0107 — about a twelfth of the smallest
move that could fire without being noise. The sd itself reproduced exactly
(0.0036 both times), which is the more reassuring number: it says the *measurement*
is stable, not merely that two point estimates happened to agree.

Figures from `bench memory aggregate` over the three run directories, not
hand-computed. Standard deviations are sample (n−1), which is what the tool reports.

Per ability, same three runs:

| Ability | Context recall | Context precision | MRR | Items |
|---|---|---|---|---|
| `knowledge-update` | 1.0000 ±0.0000 | 1.0000 ±0.0000 | 1.0000 ±0.0000 | 20 |
| `multi-session` | 0.9564 ±0.0250 | 0.9667 ±0.0000 | 0.9750 ±0.0000 | 20 |
| `single-session-assistant` | 1.0000 ±0.0000 | 1.0000 ±0.0000 | 1.0000 ±0.0000 | 20 |
| `single-session-preference` | 1.0000 ±0.0000 | 1.0000 ±0.0000 | 1.0000 ±0.0000 | 20 |
| `single-session-user` | 1.0000 ±0.0000 | 1.0000 ±0.0000 | 1.0000 ±0.0000 | 20 |
| `temporal-reasoning` | 0.9833 ±0.0144 | 0.9458 ±0.0000 | 1.0000 ±0.0000 | 20 |

**Where it loses.** `multi-session` is the only ability that misses recall, and it is
also the only one whose recall varies run to run (sd 0.0250 against 0.0000 on four of
six). Questions needing evidence spread across sessions are where this system is
weakest, and the variance says the weakness is not a fixed set of items — the boundary
moves. `temporal-reasoning` carries the lowest precision (0.9458): it retrieves the
right documents and pads the set.

**Caveats specific to this row.**

- The key is **PARTIAL**. `our_extraction_model`, `answer_model` and `judge_model`
  were left empty, so the run cannot be *proven* comparable to another — only observed
  to match on every field it does record.
- `retrieval_path_unverified` is set. The reranker was enabled on the deployment, so
  the observed path is `context-assembly|context-assembly+rerank` — a model is in the
  retrieval path and the run is therefore not deterministic by construction. This is
  the correct setting for reporting the system **as shipped** and the wrong one for a
  CI gate, which wants the reranker off and the RRF path proven.
- n=3 on 120 items. Enough for a spread, not enough to resolve a sub-point move.

## Agent quality — 2026.9.0 (first scored arm)

A different benchmark from the LongMemEval rows above, measuring a different
thing: not retrieval, but **the decisions the control logic makes** — what the
lead granted, whether roles followed their output schemas, and whether agents
called tools correctly. It runs 30 software tasks through a multi-agent
`dev-pipeline` swarm against an operator-reviewed answer key.

| Release | Tasks | Task success | Schema conformance | Tool-call validity | Steps with no output | Cost/task |
|---|---|---|---|---|---|---|
| `2026.8.9-70-g5d247f72` (shipped as **2026.9.0**) | 30 | 100.0% | 0.985 | 1.000 | 9.7% | $0.74 |

Efficiency, same arm: 667,801 tokens and 62.4 tool calls per task, 0 escalations,
0 schema retries. Total spend $25.23.

**Read both layers, because the first one alone flatters us.** Task success is
100%, and underneath it **14 of 144 terminal steps (9.7%) produced no output at
all** — 8 hit the iteration cap, 4 entered a degenerate tool loop, 2 failed
outright. Recovery absorbs those, which is the system working as designed, but a
headline "100%" without the step figure would describe a smoother product than
exists.

### The model is part of the result, not a footnote

**Model: `Qwen/Qwen3.8-27B-FP8`, self-hosted, one box.** Every figure above is
that model's behaviour as much as the control logic's, and the difference is
large enough to change how the numbers read.

Measured over ~244,000 tool calls across two deployments: this model enters
identical-repeat tool loops **26x more often** than the mix of larger hosted
models (0.52% of calls against 0.02%), and once nudged out of one it changes
approach **36%** of the time against **82%**.

That is deliberate and it is the point. **A self-hosted 27B on a single machine
is what someone running this at home actually has.** An organisation putting a
large hosted model behind the same control logic sees the lower rate; these
figures describe the harder case rather than the flattering one. The comparability
key pins the model identity, so a future arm on a different model refuses to
compare against this row rather than quietly superseding it.

### What this row does NOT establish

- **No pass or fail.** `bench agent gate` **refused a verdict**, correctly:
  resolving a 5-point effect at the inherited σ=0.0604 needs 12 paired tasks and
  this arm has 5, which can only resolve 7.6 points. A smaller movement must be
  reported as inconclusive with that floor, never as "no change".
- **No trend.** It is the first scored arm; there is nothing to compare it
  against. The releases before it have no agent row at all.
- **No noise floor for this task set.** The σ above is inherited from an earlier
  3-task measurement and describes a different set. The honest σ for these 30
  tasks does not exist yet, which is why the gate refuses.
- **Not independently reproducible.** Unlike the LongMemEval rows, which run
  against a public dataset anyone can fetch, the agent task set and its answer
  key are not published. An external reader can see the method and the figures
  and cannot re-run them. That is a real limitation of this row and is stated
  rather than left to be discovered.

### Provenance

Both the daemon binary and the agent image are recorded by content digest in the
run's arm key, and the run refuses to merge batches that disagree on any axis.
**Arms recorded before 2026-08-29 carry no agent-image identity at all** — the
executor discarded every image ID it observed, so those runs are marked
untrustworthy and cannot serve as a baseline. This is the first arm whose image
provenance is real.

## Releases with no row, and why

The policy in `RELEASE.md` requires a row per release from `2026.8.4`. It was not
followed. Rather than leave the gaps silent — which reads as "nothing regressed" —
each is stated:

| Release | Row | Why |
|---|---|---|
| `2026.8.4` | none | No benchmark run was taken. Not backfillable: it would need the tagged binary rebuilt *and* the bench deployment's models restored to what they were, and those were changed during 2026.8.7. |
| `2026.8.5` | none | As above. |
| `2026.8.6` | none | As above. |
| `2026.8.7` | none | No run. The reason was recorded at the time in the 2026.8.7 release notes: every outward-facing provider was disabled and all roles moved to a locally served model during that cycle, so a run could not be pinned to the baseline row's axes. |
| `2026.8.8` | **yes** | Both axes above. The warm row was measured at `0e450f9c`; the cold row at `2698eb67`. |
| `2026.8.9` | none *at the tag* | No run was taken at `2026.8.9` itself. The tree past it is measured on both memory axes above. |
| `2026.9.0` | **agent row only** | The agent arm above was taken on the release candidate (`2026.8.9-70-g5d247f72`). No LongMemEval row: the memory axes were measured 50 commits earlier in the same cycle and nothing in the intervening work touches the retrieval path. Stated rather than left as a silent gap. |
| `2026.9.1` | none | No run was taken. The cycle is bug fixes and the forge re-review feature; nothing in it touches ingestion, embedding, retrieval or the agent harness, so the axes stand where `2026.8.9-50-g4b343821` (memory, both axes) and `2026.8.9-70-g5d247f72` (agent arm) left them. A row measured on an unchanged path would add a data point without adding evidence, and the cost of a 120-item n=3 run is not free. Stated rather than left as a silent gap. |

A missing row stated as missing is honest. An absent row is not — which is the whole
reason this section exists.

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

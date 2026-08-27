# Run evidence for the published rows

One `*.manifest.json` per run behind a row in [results.md](../results.md). A published
row that cannot be audited is an assertion, so the provenance of every run is committed
here rather than left on one host.

Each manifest is the harness's own output, copied unmodified. It carries:

- `comparability_key` and the full `comparability_fields` it was computed from — this
  is what makes a row's axis checkable rather than described. The key is a sha256 over
  every field in the order listed, each contributed as its NUL-terminated name followed
  by its NUL-terminated value, so it can be recomputed from the manifest alone and must
  match the value recorded beside it.
- `trust` — the harness's own verdict. A run stamped `trustworthy: false` is not
  published; those verdicts are why several runs from the same week are absent.
- `dataset_sha256` — pins the dataset revision.
- `finished_at` — the only thing tying a run to a release, because the manifest
  records no daemon or harness revision. Release attribution for these rows was
  therefore reconstructed by matching this timestamp against the commit history, which
  is why one row names a commit rather than a tag.

## What is deliberately NOT here

`results.json` and `journal.jsonl` from the same runs stay out of version control.
They embed LongMemEval question text and gold document ids, and this project does not
vendor the dataset — it belongs to its authors, and `index.md` tells you to fetch it
from Hugging Face. Committing the scored output would vendor it by the back door, into
the public CE export as well.

The manifests carry no dataset content: hashes, model names, retrieval parameters and
verdicts only.

## Which run backs which row

| Row in `results.md` | Manifests | Key |
|---|---|---|
| `2026.8.7-48-g0e450f9c` (shipped as 2026.8.8), warm 6-item axis | `reproduce-{1,2,3}` | `e865104e9959` |
| `2026.8.8-40-g2698eb67`, cold 120-item axis | `postfix-20260821-{qwen38-27b,r2,r3}` | `93a6e7a0729b` |
| `2026.8.9-50-g4b343821`, cold 120-item axis | `head-20260827-6ab-r{1,2,3}` | `93a6e7a0729b` |
| `2026.8.9-50-g4b343821`, cold 6-item axis | `head-20260827-tr6-r{1,2,3}` | `e865104e9959` |

Note that the 6-item cold manifests share key `e865104e9959` with the WARM
`reproduce-*` manifests: the key does not encode corpus warmth. Check the run date
against 2026-08-21 to tell the two regimes apart until that is fixed.

The `2026.8.3-13-g13476b8f` baseline row predates this directory; its run artifacts
were not retained, which is the omission this directory exists to stop repeating.

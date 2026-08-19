---
workflowId: "companion-data-validation"
displayName: "Companion: Data validation"
description: "Validates a dataset against a stated schema. Returns anomaly list, distribution summary, and an integrity verdict the host LLM can use to decide whether to act on the data."
version: "1.0.0"
author: "Vadim Grinco <vadim@grinco.eu>"
license: "Proprietary"
entrypoint: "validate"
maxStepVisits: 1
maxIterations: 10
maxWallClock: "30m"
cleanup_artifacts:
  - artifacts/out/findings.md
# Scores this workflow's DECLARED output obligations — the
# require_output_glob entries above — as met/declared (2026-08-18).
# Chosen over pinned_case_validation because that kind needs the verifier to
# emit testing.cases[], which the local benchmark model managed 15% of the
# time against 100% for a 397B; "write the file you promised" is a contract a
# small model can actually keep. It measures delivery, NOT correctness: a
# workflow writing a valid but empty file scores 1.0. See
# https://docs.vornik.io
qualityScoring:
  kind: "contract_satisfaction"
steps:
  validate:
    type: "agent"
    # Output contract (customer report 2026-08-03): the role schema permits a
    # declared refusal, so without this a step that writes nothing still
    # counted as success. At least one file matching this glob must be written
    # DURING this step or the step fails into on_fail. Filename-specific on
    # purpose — a wildcard would be satisfied by an upstream artifact re-staged
    # into artifacts/out/ while this step runs.
    require_output_glob: "artifacts/out/findings.md"
    role: "analyst"
    on_success: "done"
    on_fail: "failed"
    timeout: "30m"
terminals:
  done:
    status: "COMPLETED"
  failed:
    status: "FAILED"
    message: "Data validation failed"
---

# Companion: Data validation

One-shot validator. The host LLM passes `dataset:` (a path or
inline rows) and `schema:` (field names, types, constraints). The
analyst checks each row, summarises distribution, and flags
anomalies.

## Prompts

### validate

Read the dataset from the path in the task payload. Apply the
schema's constraints (type, nullability, range, regex) row by row.

Produce `artifacts/out/findings.md` per the analyst role's
contract, with these specific sections:

  - "Verdict" — pass / pass-with-warnings / fail.
  - "Row count" — total, valid, invalid.
  - "Distribution" — for each field, a one-line summary (min /
    max / mean for numerics; top 3 values for categoricals;
    null count for everything).
  - "Anomalies" — severity-sorted (HIGH first); each entry
    includes the offending row, the violated constraint, and a
    short note.

Never modify the dataset. Validation is read-only.

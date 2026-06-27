---
workflowId: "companion-data-validation"
displayName: "Companion: Data validation"
description: "Validates a dataset against a stated schema. Returns anomaly list, distribution summary, and an integrity verdict the host LLM can use to decide whether to act on the data."
version: "1.0.0"
entrypoint: "validate"
maxStepVisits: 1
maxIterations: 10
maxWallClock: "30m"
cleanup_artifacts:
  - artifacts/out/findings.md
steps:
  validate:
    type: "agent"
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

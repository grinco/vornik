# tool_definitions golden fixtures

Recorded 2026-09-03 against `images/vornik-agent/entrypoint.sh` at commit
`912a175b` — the last revision in which the tool registries and the tool
definitions were hand-written in the entrypoint — by

    test/agent/tool_definitions_golden_test.sh --record

Each file is `tool_definitions()`'s output for one cell of the environment ×
allowlist matrix the test describes, canonicalised with `jq -S`. They are the
behaviour-preservation spec for the refactor in
`https://docs.vornik.io` §6.

Re-record ONLY for a deliberate change to what a model is offered, in the same
commit as that change, and say so in the commit message. A re-record that
accompanies a refactor is the refactor changing behaviour.


2026-09-05 deliberate schema change: the six `*-full.json` cells now declare
grep.head_limit minimum 1 and describe that constraint. This accompanies the
runtime fix for non-positive limits in tool-dispatch design §11. All other
cells and all other schema fields remain unchanged.

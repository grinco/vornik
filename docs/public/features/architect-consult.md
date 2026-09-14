# Architect consultation over A2A

!!! note "Community Edition — use requires a subscription"

    The consultation mechanism ships in the **Community Edition** and is
    **off by default**. Using it requires a vornik subscription: the gate is
    your service contract, not a licence check in the software. Enabling it
    creates an auditable record, and so does every consultation.

The [configuration assistant](config-assistant.md) can ask the **vornik
architect** — a remote expert agent reachable over the A2A protocol — one
question per request when it needs domain knowledge it does not hold: what a
configuration key means in practice, how a workflow topology is usually shaped
for a purpose, why a measurement looks the way it does.

## What is sent

Only a **minimized, sanitized question** — never the configuration tree, never
a diff, never a secret. The question passes the same secret screen as the
assistant's own model call and the judge call; a question that trips it is
not sent and the request continues without the consultation.

The peer's answer is **advice**, never an approval: it is folded into the
assistant's reasoning as untrusted text, cannot grant tools, and cannot make
a proposal auto-apply.

## Bounds

- at most one consultation per assistant request;
- a per-consult deadline (`config_assistant.consult.timeout`, default two
  minutes) and answer-size cap;
- the destination is the operator-configured `a2a.peers` entry only — the
  model cannot choose a host, and the destination and your consent are
  re-checked immediately before each send;
- an unreachable or timed-out peer degrades the answer (the proposal says the
  consultation did not happen); it never fails the request and is never
  retried automatically.

## Audit

Before any network call, the attempt is committed to the admin audit log.
If the audit log is not wired, or the write fails, **no call is made**. The
log carries enable / disable / destination-change events, per-request
attempt and outcome, and the first-attempted and first-successful use per
project — which is the evidence a contractual gate rests on. Counters
supplement the log; they do not replace it.

## Turning it on

```yaml
a2a:
  peers:
    vornik_architect:
      url: https://architect.vornik.cloud/a2a/v1/agents/vornik/architect
      api_key: ${VORNIK_ARCHITECT_API_KEY}
config_assistant:
  consult:
    enabled: true
    peer: vornik_architect
```

```bash
vornikctl doctor feature enable architect-consult
```

The doctor refuses unless the configuration assistant is enabled, the peer
entry exists and the admin audit log is wired.

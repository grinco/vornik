# Compliance status

**Last updated:** 28 July 2026

This page states where Vornik actually stands on EU AI Act and GDPR
conformity — including what is **not** done. We publish the gaps because a
vendor's compliance page is the first thing a buyer's legal team tests, and an
unverifiable claim fails that test immediately.

If you need a document listed below as "in preparation", ask — the timeline is
real and we would rather tell you than have you discover it in procurement.

---

## The short version

| | Status |
|---|---|
| **EU AI Act Art 50** (AI-interaction transparency) | 🟡 Implemented, shipping for the 2 Aug 2026 deadline |
| **EU AI Act Art 50(2)** (machine-readable marking) | 🔴 Not started — due 2 Dec 2026 under transitional relief |
| **EU AI Act high-risk (Annex III)** | ⚪ Not applicable today — obligations begin 2 Dec 2027 |
| **GDPR — technical measures** | 🟡 Substantially in place, one significant gap |
| **GDPR — documentation** | 🔴 In preparation |

**We do not currently claim to be "GDPR compliant."** Two mandatory documents
are still being written. See below.

---

## The self-hosting position, which matters more than any badge

Vornik is self-hosted. In a self-hosted deployment:

- **You are the data controller.** Your data lives in your Postgres, on your
  infrastructure.
- The only data leaving your network is what you send to the model providers
  **you** configure — and if you run local models, nothing leaves at all.
- There is no Vornik-operated cloud holding your data, so there is no
  vendor-side breach surface to trust us about.

For a privacy-motivated buyer this is a stronger guarantee than a certification
on a page, because it is structural rather than contractual.

---

## EU AI Act

### Article 50(1) — you must be told you are talking to an AI

**Status: implemented, shipping for 2 August 2026.**

Every conversational surface — Telegram, Slack, email, GitHub comments, web
chat — discloses that replies are AI-generated. Continuous chat surfaces
disclose once at the start of a conversation; email replies and GitHub comments
carry the notice on every message, because those get forwarded and quoted away
from their original thread.

Each disclosure is recorded with a timestamp and a hash of the exact wording
served, so conformity can be demonstrated rather than asserted.

Details: [AI transparency](ai-transparency.md).

### Article 50(2) — machine-readable marking of AI-generated content

**Status: not started. Due 2 December 2026.**

Vornik generates synthetic text — pull request bodies, commit messages, issue
comments, emails. Article 50(2) requires that content be marked in a
machine-readable way.

Vornik was placed on the market before 2 August 2026 and therefore qualifies
for the transitional relief that moves this obligation to **2 December 2026**.
We are relying on that relief deliberately and disclosing that we are.

### Article 4 — AI literacy

**Status: documentation published for operators.**

### High-risk obligations (Annex III)

**Status: not applicable today.**

Vornik is not currently placed on the market as a high-risk AI system. If you
deploy it into an Annex III use case — recruitment, credit scoring, education,
essential services, critical infrastructure — **you** become the deployer of a
high-risk system from **2 December 2027** and inherit Article 26 duties.

We are building the vendor-side evidence pack you would need (logging
guarantees, instructions for use, human-oversight documentation, data
governance) ahead of that date. Ask if you need it sooner; tell us if you are
planning such a deployment.

---

## GDPR

### What is in place

These are properties of the shipped code, not intentions:

- **Erasure.** Project-scoped hard deletion across the full table set, plus
  chunk-level eviction of memory with a tombstone audit trail, so a deletion
  can itself be evidenced.
- **Retention.** A configurable sweeper with per-table and per-project
  windows. **Off by default** so an upgrade never silently deletes your data —
  which means you must turn it on and choose your windows. See below.
- **Purpose limitation.** Memory chunks carry sensitivity and purpose policy,
  evaluated on every retrieval, with the block decisions retained for a year.
- **Accountability.** Full tool-call, chat, retrieval and ingest audit trails.
- **Access control.** OIDC with group-based permissions, per-project scoping,
  and API-key authentication.

### What is not in place

We would rather you read this from us than find it in a questionnaire.

| Gap | Consequence | Status |
|---|---|---|
| **Records of processing (Art 30)** | Mandatory document; does not yet exist | In preparation |
| **Data processing agreement (Art 28)** | No standard processor terms to sign | In preparation |
| **No data-subject identifier** | Access / erasure / portability requests cannot be scoped to one person — deletion works at project or record level, not per data subject | Engineering work scheduled |
| **DPIA (Art 35)** | Not yet performed | In preparation |
| **Retention off by default** | Personal data is kept indefinitely unless you enable the sweeper | Recommended profile + a startup warning being added |
| **Breach notification (Art 33)** | No formalised controller-notification pipeline | Planned |

The third row is the one to weigh if you are evaluating Vornik for personal
data at scale. Deleting *a project* or *a record* works well today; deleting
*everything about one person* requires manual work.

### Sub-processors

Where configured, conversation content is transmitted to the model providers
listed on the [AI transparency](ai-transparency.md) page. In a self-hosted
deployment you choose these, and a local-model configuration transmits nothing
externally.

---

## Reporting a problem

Security issues: see [security](security.md).
Compliance and data-protection questions: **enterprise@vornik.io**.

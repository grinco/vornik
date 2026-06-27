---
sources:
    - path: internal/forge/forge.go
      sha256: 76698e93cdd44c1cce5bce8535bd141536501a938af874d660b2925addd2cd9e
    - path: internal/forge/github/github.go
      sha256: 929ff0572327d1f0bfbe07704a0c0f7a6929a21de6dcbb8575b2d4d6840851b8
---
# Forge — GitHub automation

!!! note "Community Edition"

    Included in the free, open-source **Community Edition**. See [Editions](../editions.md).


Forge connects a vornik project to a GitHub repository so that **labelling an
issue opens a pull request, and opening a pull request gets a review** — done by
your swarm, on your infrastructure. The defining property is the credential
model: **the daemon owns the push, and the agent never holds a credential.**

## What it does

Forge classifies inbound GitHub events deterministically (no LLM router decides
what to do) and drives two main flows:

- **Issue labelled → pull request.** Label an issue `bug` and the swarm fixes it
  on a `fix/issue-<n>` branch; label it `enhancement` / `feature` /
  `feature-request` and it takes a first cut on a `feat/issue-<n>` branch. Either
  way the daemon opens the PR (titled `Fix #<n>: …` or `Implement #<n>: …`,
  closing the issue). **Every automated PR is opened as a draft** — a human marks
  it ready.
- **Pull request opened → review.** When a PR is opened, reopened, or marked
  ready for review, the daemon fetches the diff, a reviewer agent reads it, and
  the daemon posts the review back through GitHub's review API.

A bare issue with no label is ignored — only a *labelled* issue is actionable.

## The credential model

This is the part that matters for security review:

- The GitHub App private key lives **only on the daemon's filesystem**. It is
  read once at startup and never leaves the daemon process.
- Outbound calls use a **short-lived installation token** the daemon mints on
  demand and caches in memory for a few minutes.
- The **push** hands that token to `git` through an in-process credential header
  — never on the command line (so it can't be seen in the process list), never
  in the remote URL, and never written to on-disk git config. Pushes are
  non-forced and idempotent: an up-to-date branch is a no-op, a diverged branch
  is rejected.
- The agent only edits files in its workspace. The project's `.git` directory is
  mounted **read-only** into the agent container, so the agent *cannot* commit,
  push, or run `gh` — the entire mutation path is deterministic and daemon-side.

The result: a compromised or confused agent has no path to your repository's
write credentials, because it never has them.

## Setting it up

1. **Create a GitHub App** and download its private key (PEM). Note the App ID.
2. **Grant permissions:** Contents (read & write — verified at daemon startup),
   Issues (read & write), Pull requests (read & write), and Metadata (read).
3. **Subscribe to webhook events:** `issues`, `pull_request`, and (for `@vornik`
   mention replies) `issue_comment`.
4. **Point the webhook** at your daemon's signed webhook endpoint,
   `POST /api/v1/webhooks/{projectId}/{source}`, and set a webhook secret. Forge
   verifies every delivery's `X-Hub-Signature-256` HMAC; the secret is supplied
   to the daemon by environment-variable *name*, never written in plain YAML.
5. **Install the App** on your repositories and record the installation ID.
6. **Configure the project.** Under the project's `forge` block:

   ```yaml
   forge:
     provider: github
     github:
       app_id: "123456"
       installation_id: "78901234"
       private_key_path: /etc/vornik/secrets/forge-app.pem
       # api_base_url: https://github.example.com/api/v3   # GitHub Enterprise only
   ```

   Webhook sources route by the classified job: change-request (PR) events go to
   the review workflow, everything else to the issue workflow. Set
   `require_forge_event: true` on the source to drop deliveries that aren't
   actionable.

At startup the daemon verifies it can actually push for each Forge-configured
project and warns you if it can't — so a misconfigured key surfaces immediately,
not on the first issue.

## Review behaviour

By default a posted review is a non-gating comment, even when the reviewer
approves. Set `gating_reviews: true` on the review step to turn the reviewer's
verdict into a real GitHub APPROVE / REQUEST_CHANGES. (A review that would
approve the App's *own* PR is automatically downgraded to a comment, since GitHub
won't let an author approve their own pull request.)

## Conversational replies

A separate GitHub App *channel* handles `@vornik` mentions in issue comments:
an allowlisted user mentioning `@vornik` routes through vornik and gets a reply
posted as an issue comment. This is the chat surface; the deterministic flows
above are the automation surface.

## Notes and limits

- Automated PRs are always **drafts** — promotion to ready is a human action.
- Re-running a flow is idempotent: it matches the deterministic branch name and
  only short-circuits on an *open* PR (a closed or merged one yields a fresh PR).
- Provider support is GitHub today; the provider interface is built to be
  provider-neutral. Per-installation rate-limit buckets and PR inline
  (per-line) review comments are not yet implemented — reviews are posted at the
  PR level.

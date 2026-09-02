---
sources:
    - path: internal/forge/forge.go
      sha256: ea94efec75e13324c1377118a9f67a924532f41f4ad93bcdd876518678842eba
    - path: internal/forge/github/github.go
      sha256: e63da4e64d7de29cac06193d40704348143823a11830aec7e61c2ee64cfd0646
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
- **Pull request opened → review.** When a PR is opened, reopened, marked ready
  for review, or **updated with new commits**, the daemon fetches the diff, a
  reviewer agent reads it, and
  the daemon posts the review back through GitHub's review API.

A bare issue with no label is ignored — only a *labelled* issue is actionable.

**Draft pull requests are not reviewed automatically.** A draft is work in
progress; marking it ready for review is what starts the review. An explicit
request (below) still reviews a draft — asking is consent.

### Re-review: new commits, and asking for one

A pull request is reviewed again when you push to it. Two things keep that from
becoming noise:

- **A burst of pushes produces one review, not one per push.** While a review is
  running, further pushes update what it will look at rather than queueing more
  reviews.
- **A re-review looks at what changed** since the last review it posted, not the
  whole pull request again — so it does not repeat findings you have already
  read. If the baseline is unavailable for any reason, including a force-push
  that rewrote it, the review falls back to the complete diff rather than
  reviewing less than it should.

You can also ask directly, by mentioning the bot in a comment on the pull
request:

| Comment | Effect |
|---|---|
| `@vornik review` | review now, covering what changed since the last one |
| `@vornik full review` | review the whole pull request, ignoring the baseline |
| `@vornik pause` | stop reviewing this PR automatically; commands still work |
| `@vornik resume` | resume automatic review of this PR |

**Only people with standing in the repository can run these** — its owner,
organisation members, and invited collaborators. A review is model spend, so on
a public repository an ungated command would let any passer-by spend your
budget. Note that a *contributor* — someone who has had a pull request merged —
does not qualify: that describes the past, not permission to trigger spend at
will. Anything the forge does not vouch for is refused, so an unfamiliar payload
fails closed.

A review posted in answer to a comment **quotes the request it is answering**, so
the review says what it was asked. Without that a reader sees a verdict with no
idea which question produced it — and comments can be edited or deleted, taking
the context with them, while the review itself stays.

A mention that isn't one of these commands gets a conversational reply, exactly
as before.

To turn automatic review-on-push off for a whole project rather than one PR, set
`github_app.auto_review_on_push: false`. Automatic review of drafts can be turned
on with `github_app.review_draft_prs: true`.

## Backlog-origin pull requests

The two flows above are inbound-event-driven: an issue label or an opened PR
tells Forge what to do and which repo to do it in. A project running
[backlog autonomy](../guides/autonomy.md#backlog-autonomy-and-agent-deposits)
opens draft PRs a third way, with no inbound event at all: an item consumed
from the project's backlog file is dispatched into the `backlog-item`
workflow (analyze → implement, TDD → test → review → publish), and its
`publish` step is the same deterministic `forge.open_change_request` handler
the issue flow uses — pushing the branch and opening the PR daemon-side, no
agent involved.

A few things differ from the issue-driven flow because there's no issue to
key off of:

- **Branch naming.** No `fix/issue-<n>` / `feat/issue-<n>` — the branch is
  named from the backlog item itself (`backlog/<slug>`).
- **No issue to close.** The PR title and body describe the change; there's
  no `Fix #<n>` linkage because no issue triggered it.
- **Always draft, same as the issue flow.** Nothing about backlog origin
  changes that — every automated PR is opened as a draft regardless of how it
  was triggered.
- **The outbound repo comes from config, not the event.** With no webhook
  payload naming a repository, backlog-origin publishing needs `github.repo`
  (`owner/repo`) set on the project so the daemon knows where to push.

Once opened, a backlog-origin PR goes through the exact same **pull request
opened → review** path as any other PR — it isn't special-cased for review,
only for how it came to exist.

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

The reviewer works through the diff in a fixed order — intent, a design
challenge (is this the right approach, and what's the alternative?), a
correctness pass that cites `file:line` for every finding, test adequacy,
security and robustness, and an explicit "what would break this" attempt —
before reaching a severity-ranked verdict. A problem the reviewer notices
that the diff didn't introduce doesn't block the PR: it gets recorded with
the `backlog_deposit` tool (see
[Backlog autonomy and agent deposits](../guides/autonomy.md#backlog-autonomy-and-agent-deposits))
instead, so the verdict stays scoped to what's actually in the diff.

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

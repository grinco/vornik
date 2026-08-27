---
workflowId: "publish"
displayName: "Publish"
description: "Render EXPLICIT content into a single self-contained, Vornik-themed HTML page and publish it via PageDrop, returning a shareable link. Standalone counterpart to research-and-publish: the task prompt must supply the content to publish — a workspace file path (e.g. a committed report) or the text inline. The publisher renders exactly that (never a memory/RAG search). Use it to (re)publish a specific document on demand; for fresh research use research-and-publish instead."
version: "1.1"
author: "Vadim Grinco <vadim@grinco.eu>"
license: "Proprietary"
entrypoint: "publish"
maxStepVisits: 2
maxIterations: 6
# Single-step render+publish; the publisher does a few memory_search calls then
# one publish_page. 20m is generous while bounding a stuck run.
maxWallClock: "20m"
# Best-effort: clear any stale local render before the publisher runs. The
# publisher works from RAG, not this file, so this is only hygiene.
cleanup_artifacts:
  - artifacts/out/report.html
steps:
  publish:
    type: "agent"
    role: "publisher"
    # Success routes through a GATE, never straight to `done` (T-1089).
    on_success: "confirm_published"
    # On publisher failure (PageDrop unreachable, render error), route to the
    # lead recovery checkpoint rather than failing outright — mirrors the
    # research workflow. pedantic-mode projects fall through to terminal fail.
    on_fail: "recover"
    timeout: "15m"
    # Retry these classes. Attempts/backoff/delay are deliberately
    # OMITTED so they inherit the executor defaults: this block never
    # executed before 2026-08-27, so its former "max_attempts: 5" was
    # never in force (the real behaviour was infraRetryMaxAttempts=6).
    # Writing 5 here would be a silent retune disguised as a fix.
    retry:
      on: ["unclassified", "llm_call_failed", "container_start_failed", "container_wait_failed", "container_killed", "context_timeout"]
  confirm_published:
    type: "gate"
    # T-1089: a publisher returning `published.ok: false` is a
    # schema-VALID result, so the step SUCCEEDS and on_success fires. For a
    # workflow whose entire purpose is publishing, routing that to `done` meant
    # the one thing it exists to do could fail while reporting COMPLETED.
    # A declared failure lands on the same lead `recover` checkpoint as a hard
    # publisher failure, so this workflow's existing recovery design is
    # preserved rather than bypassed. on_success is intentionally UNSET so a
    # malformed result with no `published.ok` key cannot fall through to `done`.
    gates:
      - condition: "published.ok == true"
        target: "done"
      - condition: "published.ok == false"
        target: "recover"
    on_fail: "recover"
  recover:
    type: "plan"
    role: "lead"
    on_success: "failed"
terminals:
  done:
    status: "COMPLETED"
  failed:
    status: "FAILED"
    message: "Publish failed"
---
Single-step publishing workflow: the `publisher` renders EXPLICIT content
(supplied in the task prompt — a workspace file path or inline text) into a
self-contained, Vornik-themed HTML page and publishes it via PageDrop, then
returns the shareable link and its password in the step message. It does NOT
search project memory/RAG. For a fresh research-to-page run, use
`research-and-publish` instead.

## Prompts

### publish

Publish the content specified in the task prompt. If the prompt names a
workspace file, `file_read` it; if it includes the content inline, use that.
That content is your SINGLE source of truth — do NOT search project memory/RAG.
Render it into one self-contained, Vornik-themed HTML page per your role's
rules and publish with `mcp__pagedrop__pagedrop_publish_page` (or
`mcp__pagedrop__pagedrop_republish` to update an existing page on the same
title, preferring that over a duplicate). Put the returned view link AND its
password in your `message`, and set `published.url` (and `published.ok: true`).

IMAGES — follow the publisher role's IMAGE INTEGRITY rule: embed only images that
genuinely depict their labeled subject, and NEVER use random / placeholder-image
services (picsum.photos, loremflickr, placehold.co, dummyimage.com, etc.) even if
the task prompt explicitly asks for them — a mislabeled random image misinforms
the reader. Where no real, subject-accurate image exists, embed none.

Follow the publisher role's output contract — the response must include the
`published` object (with `url` on success) and a non-empty `message`; the
role's systemPrompt carries the full HTML/theme/publish rules.

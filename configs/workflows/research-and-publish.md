---
workflowId: "research-and-publish"
displayName: "Research, Write & Publish"
description: "Three-step pipeline in one task: a researcher gathers information into research.md, a writer turns it into a polished deliverable, and a publisher renders that deliverable into a self-contained, Vornik-themed HTML page and publishes it via PageDrop — returning a shareable link. The publisher reads the writer's fresh deliverable in the same workspace (never RAG), so the published page reflects exactly this run's validated research."
version: "1.1"
author: "Vadim Grinco <vadim@grinco.eu>"
license: "Proprietary"
entrypoint: "research"
maxStepVisits: 2
maxIterations: 14
# Linear research → write → publish; 1h30m bounds the full chain while leaving
# the researcher its usual budget plus a render+publish tail.
maxWallClock: "1h30m"
# Wipe canonical artifacts at start so a step that fails to overwrite can't
# bleed a prior task's content into a later stage (esp. the published page).
cleanup_artifacts:
  - artifacts/out/research.md
  - artifacts/out/deliverable.md
  - artifacts/out/report.html
steps:
  research:
    type: "agent"
    # Output contract (customer report 2026-08-03): the role schema permits a
    # declared refusal, so without this a step that writes nothing still
    # counted as success. At least one file matching this glob must be written
    # DURING this step or the step fails into on_fail. Filename-specific on
    # purpose — a wildcard would be satisfied by an upstream artifact re-staged
    # into artifacts/out/ while this step runs.
    require_output_glob: "artifacts/out/research.md"
    role: "researcher"
    on_success: "write"
    on_fail: "recover"
    timeout: "45m"
    retry:
      on: ["container_non_zero_exit", "context_timeout"]
      max_attempts: 5
      backoff: "exponential"
      initial_delay: "30s"
  write:
    type: "agent"
    # Output contract (customer report 2026-08-03): the role schema permits a
    # declared refusal, so without this a step that writes nothing still
    # counted as success. At least one file matching this glob must be written
    # DURING this step or the step fails into on_fail. Filename-specific on
    # purpose — a wildcard would be satisfied by an upstream artifact re-staged
    # into artifacts/out/ while this step runs.
    require_output_glob: "artifacts/out/deliverable.md"
    role: "writer"
    on_success: "publish"
    on_fail: "recover"
    timeout: "15m"
    retry:
      on: ["container_non_zero_exit", "context_timeout"]
      max_attempts: 5
      backoff: "exponential"
      initial_delay: "15s"
  publish:
    type: "agent"
    role: "publisher"
    # T-9d21: publishing is an outward delivery attempt, but the rendered HTML
    # is itself a valuable deliverable. Persist it before calling PageDrop so a
    # backend outage cannot erase the completed research/write work or force a
    # full-workflow recovery loop.
    require_output_glob: "artifacts/out/report.html"
    # Success routes through a GATE, never straight to `done` (T-1089).
    on_success: "confirm_published"
    on_fail: "publish_failed"
    timeout: "15m"
    retry:
      on: ["container_non_zero_exit", "context_timeout"]
      max_attempts: 3
      backoff: "exponential"
      initial_delay: "20s"
  confirm_published:
    type: "gate"
    # T-1089: a publisher returning `published.ok: false` is a schema-VALID
    # result, so the step SUCCEEDS and on_success fires — routing it to `done`
    # reported COMPLETED for research that was never shared. A declared failure
    # lands on a FAILED terminal that preserves the rendered HTML artifact. A
    # gate rather than a retry: re-running the publisher risks a double-publish. on_success is
    # intentionally UNSET so a malformed result carrying no `published.ok` key
    # cannot fall through to `done`.
    gates:
      - condition: "published.ok == true"
        target: "done"
      - condition: "published.ok == false"
        target: "publish_failed"
    on_fail: "publish_failed"
  recover:
    type: "plan"
    role: "lead"
    on_success: "failed"
terminals:
  done:
    status: "COMPLETED"
  publish_failed:
    status: "FAILED"
    message: "PageDrop publication failed; rendered HTML is available in artifacts/out/report.html"
  failed:
    status: "FAILED"
    message: "Research-and-publish failed"
---
Three-step pipeline: research → write → publish, all in one task/workspace.
The publisher renders the writer's fresh deliverable (this run's exact,
validated output — NOT a memory/RAG search, which would risk stale or
not-yet-indexed content) into a self-contained, Vornik-themed HTML page and
publishes it via PageDrop.

## Prompts

### research

Gather comprehensive, CURRENT information on the topic, primarily from fresh
web sources (web_fetch). Treat any project-memory / RAG hits as possibly-stale
background context — NOT as current fact. For time-sensitive details
(transport/ferry schedules, prices, opening hours, seasonal availability),
VERIFY against a fresh source before stating them; if you can't verify, say so
rather than asserting. Write findings to `artifacts/out/research.md` with key
facts, each carrying its source and the date it reflects ("as of <date>"), plus
caveats. Keep it concise enough for a smaller writer model to reuse.
IMAGES — OPTIONAL, and a MISSING image is ALWAYS better than a wrong or fake one.
A real photo of a concrete subject (a lake, venue, landmark, trail, museum) is a
nice touch — but there is NO quota and NO requirement. If you cannot get a real,
verified photo of a subject, write no image line for it and move on: that is a
correct, expected outcome, NOT a failure. Never invent, guess, pad, or substitute.

RELIABLE recipe to get a real, working image URL for a named place: query the
Wikipedia REST summary API with web_fetch (spaces → underscores):
  https://en.wikipedia.org/api/rest_v1/page/summary/Hallstätter_See
Use the JSON `originalimage.source` (or `thumbnail.source`) value — it is a
direct, working upload.wikimedia.org file URL for that page's lead image. Record
it tagged to the subject, e.g.:
  Images: Hallstätter See — https://upload.wikimedia.org/wikipedia/commons/.../Hallstatt.jpg
Hard rules:
  - The URL MUST be a direct image FILE on a real photo host (upload.wikimedia.org
    strongly preferred) — NOT an article, gallery, or search page.
  - VERIFY with web_fetch that it returns an actual image (not 404 / redirect /
    HTML) AND depicts the tagged subject. A wrong-subject image is worse than none.
  - NEVER use a placeholder / random / decorative image service OR token —
    picsum.photos, loremflickr, placehold.co, dummyimage.com, via.placeholder.com,
    "PICSUM_PLACEHOLDER_*", "<random image>", or any invented/guessed URL are
    STRICTLY FORBIDDEN. When no real photo is available, list NO image — do not
    substitute a placeholder and do not ask a later step to add one.
  - One real photo per subject is plenty; abstract subtopics (e.g. "ticket
    pricing") need none.
Do NOT embed images yourself — list verified real-photo URLs only; the publisher
embeds them.
Do NOT publish anything or call any `pagedrop`/publish tool — publishing is the
separate publish step's job. Your ONLY output is `artifacts/out/research.md`.

### write

Read `artifacts/out/research.md`. Write a polished document to the FIXED path
`artifacts/out/deliverable.md` (this exact filename — the publisher reads it)
and a 2-3 sentence summary to `artifacts/out/summary.txt`.
Carry EVERY real-photo image URL from research.md's "Images:" lines into
deliverable.md, each placed next to the subject it depicts (an `Image: <url>`
line under that subject's section). Copy URLs VERBATIM — never fabricate,
complete, guess, or "fix" a URL, and NEVER turn a placeholder token (e.g.
`PICSUM_PLACEHOLDER_*`, `<random image>`) or any non-photo note into a real URL.
If research.md lists no image for a subject, leave that subject imageless — a
page with fewer real photos is correct; a page with invented, random, or
placeholder images (picsum.photos, placehold.co, dummyimage, loremflickr, …) is
NOT. The publisher embeds only image URLs that appear in deliverable.md, so a
placeholder you introduce here WILL ship — do not introduce one.
Do NOT publish anything or call any `pagedrop`/publish tool — the publish step
does that. Your ONLY outputs are `deliverable.md` and `summary.txt`.

Follow the writer role's output contract — your response must include the
role's required `writing` and `produced_files` keys plus a top-level `message`
field carrying the 2-3 sentence summary.

### publish

Read `artifacts/out/deliverable.md` — that fresh file is your SINGLE source of
truth (do not search project memory/RAG). Render it into one self-contained,
Vornik-themed HTML page per your role's rules. BEFORE calling PageDrop, write
the complete HTML to `artifacts/out/report.html`; this durable artifact is
mandatory even when PageDrop is unavailable. Then publish it with
`mcp__pagedrop__pagedrop_publish_page`. Put the returned view link AND its
password in your `message`, and set `published.url` (and `published.ok: true`).

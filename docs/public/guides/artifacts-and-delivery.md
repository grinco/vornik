---
sources:
    - path: internal/dispatcher/render_document.go
      sha256: 978d37b59d3584d28b5260865673db9fbb40bbf123ecff1d76a3eab1c49a27fe
    - path: internal/dispatcher/tools.go
      sha256: a17794ec0d0d46edfe37981be4de0633fb3dd54a0f30dbacab2431c4c2952e34
    - path: internal/dispatcher/agent.go
      sha256: fc3d9d3007a4c94630de6375a295a634ab054e7c2234ebaadaa15dbe121e043b
    - path: internal/email/channel.go
      sha256: 38058785781c8f5dad5197c282de88501a07e81a192d3cec3c6715bd57fb8a6a
---
# Artifacts & Outbound File Delivery

When a vornik agent produces something a person actually wants to *keep* — a
report, a CV, a patch, a generated PDF — you want the **file**, not a wall of
text pasted into the chat. vornik has two built-in tools for that:

- **`render_document`** — turn markdown into a real `.md`, `.html`, or `.pdf`
  file and deliver it.
- **`send_artifact`** — deliver a file a completed task already produced.

Both deliver to **wherever the conversation is happening** — the file lands in
the same Telegram chat or email thread the request came in on. You never name a
recipient; the destination is bound to the conversation.

## Rendering a document on the fly

`render_document` converts markdown you (or the agent) supply into one or more
formats and sends each file back to the chat. It runs deterministically on the
daemon host — no agent container, no extra model call.

Parameters:

| Parameter | Required | Description |
|---|---|---|
| `content` | yes | The markdown source, rendered verbatim. |
| `name` | yes | Base filename, no extension (e.g. `quarterly-report`). |
| `formats` | no | Any of `md`, `html`, `pdf`. Defaults to all three. |

- **`md`** is always available — it's the source written to a file.
- **`html`** is produced with `pandoc`. If pandoc isn't on the host it falls
  back to a bundled renderer, and as a last resort to a plain unstyled wrapper,
  so an HTML file is always delivered.
- **`pdf`** is produced with `pandoc` + `weasyprint`. Both must be available
  (directly on the host, or via the bundled agent image). If neither path can
  render a PDF, the failure is reported plainly rather than silently dropped.

Rendered files are **transient**: they're streamed straight to the chat and not
kept in long-term storage. Use `render_document` for "make me this document
now"; use `send_artifact` (below) for "give me the file that task produced."

## Delivering a task's artifact

`send_artifact` retrieves a file a completed task wrote to its output and
delivers it as a download.

| Parameter | Required | Description |
|---|---|---|
| `task_id` | yes | The task whose output you want delivered. |
| `artifact_name` | no | A specific artifact; omit to send the task's first output artifact. |

Two safeguards are worth knowing:

- **Project scope is enforced.** A conversation pinned to one project can only
  retrieve that project's artifacts — you can't pull another project's files by
  guessing a task id.
- **Only operator-facing outputs are eligible.** Selection is bounded to a
  task's published output artifacts; internal scratch and debug files can never
  be sent.

## Where files go

File delivery is bound to the channel the conversation started on:

- **Telegram** — delivered as a document upload (up to **50 MiB** per file).
- **Email** — delivered as an attachment on a reply in the same thread
  (`multipart/mixed`, with the body always listing the files produced).

> **Slack is not a file-delivery target** for these tools today. A request on a
> channel with no file surface simply doesn't expose `render_document` /
> `send_artifact`.

Email also enforces a per-message attachment size cap (configurable, below):
over-cap attachments are **skipped and logged, but the reply is still sent** —
with the body listing the files — so the recipient is never left with nothing.

## Files going IN: two different mechanisms

Files reach an agent by one of two paths, and they are **not** interchangeable. A
customer built a workflow and a gate against the wrong one, which is a documentation
defect rather than a mistake on their part — so here they are side by side.

### 1. A file you attach to a task (the usual case)

You upload with `/upload` (or any companion `delegate` carrying `inputArtifacts`, or the
REST create-task path, or a Telegram / email attachment). The task then carries:

- `context.inputFiles` — the paths, and `inputArtifactIDs` alongside them;
- an `## ATTACHED FILES` block in the agent's prompt, naming each file;
- the raw file **staged into the container** at `artifacts/in/<name>`, which is what an
  agent opens with an ordinary file read.

There is one wrinkle worth knowing, because it is the thing that bites: a **document**
(PDF, EPUB, DOCX…) is normally *extracted* into project memory at upload time, and the
raw bytes are then deliberately NOT staged — the extraction carries the content, and
staging a 32 MB EPUB alongside it once blew an agent's context. Images, audio and video
are always staged too, because their extractions are lossy derivatives: OCR text is not
the picture.

If your workflow reads the **file itself**, declare `require_input_artifacts: true` on it.
That declaration is the guarantee: the raw file is staged for that workflow no matter which
path created the task, and no matter whether an adaptive route picked the workflow after the
upload happened.

> **Size bound.** The declaration cannot make a large file small. Raw staging for such a
> workflow is capped at **8 MiB per file**; above that the extraction stands alone, because
> staging a 32 MB document next to its extraction is what blew an agent's context the first
> time. That is comfortably above what these workflows normally consume — a companion upload
> caps a single file at 512 KiB — but if you need a genuinely large file *as a file*, split
> it, or rely on the extraction and memory search instead of `require_input_artifacts`.

### 2. A summary of a delegated child's output

`inputArtifactsSummary` is a different thing entirely. It is the **hand-off summary from
delegated child tasks**, and it appears only on a step that opts in with
`stage_child_artifacts`. It has nothing to do with files you uploaded.

So a gate asserting `inputArtifactsSummary` on an upload-bearing task can only ever fail:
the field is not absent because something broke, it is absent because it describes a
mechanism the task never used. For an upload, assert on `inputFiles` /
`inputArtifactIDs`, or simply have the step read `artifacts/in/`.

| You want… | Use | Appears when |
|---|---|---|
| the file a user attached | `context.inputFiles`, `artifacts/in/<name>` | any upload path |
| the raw bytes guaranteed | `require_input_artifacts: true` on the workflow | always, once declared |
| what a child task produced | `inputArtifactsSummary` | only with `stage_child_artifacts` |

## Configuring email delivery

File delivery over Telegram works as soon as the bot is connected. Email
delivery needs outbound SMTP configured on the project's `email` block:

```yaml
email:
  smtp_host: "smtp.example.com"
  smtp_port: 587
  smtp_username: "assistant@example.com"
  smtp_password_env: "ASSISTANT_SMTP_PASSWORD"   # value read from this env var
  from_address: "assistant@example.com"
  # Per-message attachment cap in bytes; 0 = unlimited. 25 MiB shown.
  attachment_size_cap_bytes: 26214400
```

Without `smtp_host` + `from_address`, the email delivery path is disabled (the
tools still work on Telegram). The password is taken from the named environment
variable, never stored inline.

## Putting it together

The agent reaches for these tools when a user asks for the artifact itself —
"send me the CV", "share the report as a PDF", "email me the document". You can
make that the default behaviour for a project by allow-listing the tool and
nudging the model in its system prefix:

```yaml
permissions:
  allowedTools:
    - send_artifact
    # render_document, file_*, memory_*, tool_search are built in

chat:
  system_prefix: |
    When asked for a document (a CV, a planning doc, an article, a report),
    ALWAYS use send_artifact (or render_document for fresh markdown) to deliver
    the file. Do not paste the document body into the chat.
```

## Sending plain text email (not a file)

If you want the agent to **email someone text** rather than deliver a file,
that's a separate tool, `send_email`, which composes a fresh message
(`to`, `subject`, `body`, optional `in_reply_to`). It carries no attachment —
reach for `send_artifact` / `render_document` when a file is the point.

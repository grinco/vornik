---
sources:
    - path: internal/dispatcher/render_document.go
      sha256: 978d37b59d3584d28b5260865673db9fbb40bbf123ecff1d76a3eab1c49a27fe
    - path: internal/dispatcher/tools.go
      sha256: 5cadbfa102353cacdda4ac98b0c42259ee9eef78ddaea9de0cecf1007d4a5870
    - path: internal/dispatcher/agent.go
      sha256: 61b5938c5b5d5020869b627708035f3a0709d6ef8d17befcea75b26bd998f72c
    - path: internal/email/channel.go
      sha256: f79eef8120d028866e172af25d8cf0355800a1e9ac7a3c585e50f3e8e7032fb0
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

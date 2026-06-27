---
sources:
    - path: docs/operator/workflows-and-llm-controls.md
      sha256: 85755f26cb13af2da71d215388b3d770f7230e3ed850e0222c387ffc0b8d6f3d
---
# Workflows and LLM Controls

vornik runs work as **workflows** — ordered sets of steps, each of which can ask
a model to do something. This guide covers two things: how to author and
validate your own workflows, and the controls vornik wraps around every model
call to keep them safe, focused, and reliable.

## Authoring workflows

A workflow is a single Markdown file. It has two parts: a frontmatter block that
describes the workflow, and a body that holds the prompts its steps run.

### The shape of a workflow file

```markdown
---
name: my-workflow
description: A short summary of what this workflow does.
version: 1.0.0
author: you@example.com
license: MIT
---

## Prompts

### research-step
You are a research assistant. Gather sources on the topic and
summarize the key findings...

### write-step
Using the research above, draft a clear, well-structured report...
```

- **Frontmatter** requires `name`, `description`, and `version`. `author` and
  `license` are recommended. The format mirrors the wider agent-skills
  ecosystem so workflows are easy to share.
- **Body** holds the per-step prompts under a `## Prompts` section. Each step
  that does not carry its prompt inline pulls it from a `### <step-id>`
  subsection that matches the step's id. Short prompts can live inline in the
  frontmatter and long ones in the body — mix both freely.

### Bundled templates

vornik ships ready-to-use workflow templates you can start from, including
research, planning-and-writing, an adaptive general-purpose workflow, a
development pipeline, and a quick-task workflow. List what is
available with:

```bash
vornikctl workflow list
```

and inspect any one of them in full with:

```bash
vornikctl workflow show <name>
```

### Validating your workflow

Before you put a workflow into service, validate it:

```bash
# Validate a single file
vornikctl workflow validate ./my-workflow.md

# Validate every workflow file in a directory
vornikctl workflow validate ./workflows/

# Print suggested fixes inline (hints only — nothing is written back)
vornikctl workflow validate --fix ./my-workflow.md

# Machine-readable output
vornikctl workflow validate --json ./workflows/
```

The validator checks:

- `name` is present, lowercase-with-hyphens, and at most 64 characters.
- `description` is present and at most 1024 characters.
- `version` is present and looks like a semantic version.
- `author` and `license` are present (a warning if missing).
- The file is under 100k characters (a warning over 15k).
- The body has a `## Prompts` section when steps need one.

It exits `0` when the file is clean or only has warnings, and `1` on any error.

!!! note
    The validator reads your file but does not run the workflow. Logical
    problems — a step that depends on one that never runs, for example — only
    show up when vornik actually loads the workflow, not at validation time.
    `--fix` only *prints* suggestions; applying them stays a deliberate manual
    edit so the change always gets a human's eye.

---

## LLM controls

Every model call in vornik is wrapped by a few controls. Most need no
configuration, but it helps to know what they do and what you will see.

### Intent judge

Before a tool runs, vornik assesses how risky the call is and records a verdict
— a risk level (`critical`, `high`, `medium`, or `low`), a recommendation, and a
short reason. A fast rule-based check runs instantly inline, and a model-based
refiner runs in the background to add a second opinion.

Today these verdicts are **informational**: they appear in the audit trail so
you can see what vornik thought of each action, and they do not yet block
anything. This gives you visibility before any enforcement is switched on in a
later release.

You can choose which model performs the background refinement. A small,
inexpensive open-weight classifier is plenty — it does not need a heavyweight
reasoning model.

```yaml
intentjudge:
  # Model used by the background refiner. Leave empty to fall back to
  # the chat default. A small open-weight model is recommended.
  refiner_model: "openai.gpt-oss-20b-1:0"
```

### Output guard

vornik scans the **results** that tools return before handing them back to the
model. This protects against content that comes in from the outside world — a
scraped web page that tries to hijack the conversation, or a tool result that
leaks a secret.

- **Prompt-injection** and **credential leakage** are treated as high severity.
  When they fire, the offending text is redacted in place (replaced with a
  `[REDACTED:...]` marker) so the model never sees the raw content.
- **Suspicious encoded content** and **dangerous-looking links** are flagged as
  warnings; vornik notes them and keeps going.

In Telegram, when something is flagged, vornik appends a short footer to the
reply (for example, `output_guard: 1 high (credential), 2 warn`) so you can see
at a glance that the guard acted on that turn.

!!! note
    The output guard inspects tool *results*, not what users type. Risky user
    input is the intent judge's job; the output guard's job is content that
    arrives through tools.

### Tool discovery for large tool sets

If a project has a large catalog of tools (more than about 20), showing all of
them to the model on every turn wastes space and attention. So when the catalog
is large, vornik hides the extra tools by default and offers a built-in
`tool_search` tool instead. The model searches for the tools it needs by name or
description, and matches become available for the rest of the conversation.

This kicks in more aggressively when a conversation is running low on context
budget — exactly when conserving space matters most. There is nothing you need
to configure; the behavior adapts to the size of your tool catalog and the model
you are using.

!!! tip
    Models with smaller context windows tighten tool visibility sooner than
    models with large windows. Same project, same tools, but fewer visible at
    once — that is expected, not a bug. It is one more reason model choice
    matters; see [Cost and caching](cost-and-caching.md).

### Reliable memory search

Memory search is built to **always return an answer rather than an error**. It
prefers high-quality semantic (vector) search, but if that is unavailable it
automatically falls back to full-text search, and then to a simple substring
match, before it would ever fail outright. The result always tells you when it
is running on a fallback tier, so you know the answer may be less precise.

If you notice search persistently using a fallback, run the doctor to find the
underlying cause:

```bash
vornikctl doctor
```

The fallbacks protect availability, not quality — fixing the root cause restores
the best results. For more on what vornik stores and how to manage it, see
[Storage and retention](storage-and-retention.md) and the
[memory feature guide](../features/memory-rag.md).

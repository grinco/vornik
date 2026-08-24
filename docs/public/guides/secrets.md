---
sources:
    - path: internal/config/config.go
      sha256: d05c764c8e2bd5764003cb480bb4d77179a84251369d82312476c614659bb211
    - path: internal/executor/executor.go
      sha256: 4a952ff82bc9c781edbedd05f87d2ca8f05cc10ce3fbcb289570547dbc5cba6b
---
# Named secrets

Named secrets inject credentials into a project's agent containers as ordinary
environment variables, scoped to the projects that are allowed to use them.
They keep API keys and tokens out of project documents and workflow definitions
— the agent just reads an environment variable.

## Declaring secrets

Add a `named_secrets` list to the daemon configuration:

```yaml
named_secrets:
  - name: WEATHER_API_KEY
    value: "${WEATHER_API_KEY}"     # resolved from the daemon's environment
    allowed_projects:
      - forecast-bot
  - name: SHARED_ANALYTICS_TOKEN
    value: "${ANALYTICS_TOKEN}"
    allowed_projects: []            # empty = available to every project
```

| Field | Meaning |
|---|---|
| `name` | The environment-variable name the agent will see. |
| `value` | The secret value. Supports `${VAR}` expansion (below). |
| `allowed_projects` | Projects whose agents receive this secret. Empty = all projects. |

## Keep the literal value out of the file

`value` is expanded with `${VAR}` syntax against the **daemon's own
environment** at startup, so you don't have to write the raw secret into the
config file. Store the real value where the daemon process reads its
environment — for example a systemd `EnvironmentFile` or your secret manager —
and reference it:

```yaml
  - name: WEATHER_API_KEY
    value: "${WEATHER_API_KEY}"     # the real key lives in the daemon's env
```

Expansion happens once, when the configuration is loaded. Update the
environment and reload the daemon to pick up a rotated value.

### Where the env files are read from

Before expanding placeholders, vornik sources these files itself, so `${VAR}`
resolves the same way for the daemon and for `vornikctl` — which has no systemd
`EnvironmentFile` of its own:

1. `<config-dir>/vornik.env` — the file the podman quickstart seeds
2. `<config-dir>/secrets/env`
3. `<config-dir>/secrets/*.env` — one file per service, alphabetical

A variable already present and non-empty in the real environment always wins, so
an explicit `export` or a systemd `EnvironmentFile` still overrides these files.
Earlier entries in the list win over later ones.

If a required setting comes out empty because its placeholder had nothing to
resolve to, the error names the variables involved:

```
configuration validation failed: database name is required (unset config
placeholders: ${POSTGRES_DB}, ${POSTGRES_USER} — set them in
<configDir>/vornik.env or <configDir>/secrets/*.env, or export them before
running)
```

## Scoping with `allowed_projects`

`allowed_projects` controls which projects' agents receive each secret:

- **Empty list** → every project's agents get the variable.
- **Non-empty list** → only the named projects' agents get it; agents in any
  other project never see the variable at all.

Scope every secret as narrowly as the work requires — a project that doesn't
need a credential should never receive it.

## How the agent sees it

For an allowed project, the secret is injected into the agent container as an
environment variable named exactly by `name`. An agent reads it the usual way
(for example, `os.Getenv("WEATHER_API_KEY")`). When the same variable name is
set in more than one place, per-task environment overrides a named secret,
which overrides a role's static environment.

Named secrets are configured in the daemon configuration only — there is no
separate command-line surface for managing them.

# Carrying tool-issued credentials to you

Named secrets push credentials *into* an agent. The opposite problem is a
credential that comes *out* of a tool: some tools mint an access credential for
the artifact they produce — canonically PageDrop, whose publish tools return a
**viewing password** for the page. You need that password, but the outbound
secret-redactor (which scrubs agent chat replies and notifications) treats a
password-shaped value like any other secret and replaces it with
`[REDACTED:…]` before it reaches you.

**Credential carryover** closes that gap. The daemon captures the credential
**deterministically from the trusted tool's own output** — no model decides
what's a credential — stores it against the task, and surfaces it
**code-formatted and one-tap-copyable** in the task's completion notification
(Telegram) and on the task-detail page, instead of redacting it.

## Enabling it

Add a `tool_credentials` mapping under `secrets`. Each entry names a tool (by
prefix) and how to extract the credential from its output — either a **text
pattern** (a regexp with one capture group, for tools that return prose) or a
**JSON field** (a dotted path, for tools that return a JSON object):

```yaml
secrets:
  # A capturing tool is implicitly trusted-output for its captured field, but
  # listing it here too keeps the value un-redacted in the tool-audit log.
  trusted_output_tools:
    - "mcp__pagedrop__pagedrop_publish"
  tool_credentials:
    # PageDrop returns prose ("View: <url> … Password: <pw>"), so use patterns:
    - tool: "mcp__pagedrop__pagedrop_"      # prefix — matches publish/republish
      credential_pattern: "Password:\\s*(\\S+)"
      artifact_pattern: "View:\\s*(\\S+)"    # optional; links the credential to a URL
      label: "viewing password"             # operator-facing name
    # A tool that returns JSON would use field paths instead:
    # - tool: "mcp__example__publish"
    #   credential_field: "data.password"   # dotted path; no array indexing
    #   artifact_field: "url"
    #   label: "access token"
```

| Key | Meaning |
|---|---|
| `tool` | Tool-name prefix whose output carries the credential. |
| `credential_pattern` | Regexp (one capture group) extracting the credential from text output. Takes precedence over `credential_field`. |
| `artifact_pattern` | Optional regexp (one capture group) for the URL the credential unlocks. |
| `credential_field` | Dotted path into a JSON result to the credential (e.g. `data.password`). No array indexing. |
| `artifact_field` | Optional dotted path to the artifact URL. |
| `label` | Operator-facing name shown next to the value (defaults to `credential`). |

With no `tool_credentials` entries (the default) nothing is captured and
behaviour is unchanged.

## What it does and doesn't do

- **The model is never involved** in deciding what's a credential — capture is
  driven by the operator-configured tool + field/pattern, reading the tool's
  daemon-proxied output that an agent cannot forge.
- **Strong keys are never captured.** A value matching a strong, prefix-anchored
  pattern (OpenAI / Anthropic / GitHub / AWS / JWT / …) is refused even from a
  configured tool and always redacts — only the lower-confidence
  "viewing-password" shape is eligible. So a misconfigured mapping can't turn a
  real API key into a carryover, and it isn't an exfiltration channel.
- **Retries don't leak stale credentials.** Only the task's most recent
  execution's credential is surfaced; an earlier attempt's password is hidden.
- The value is stored alongside the task for its lifetime (same as the
  tool-audit row that already holds it) and removed when the task is purged.

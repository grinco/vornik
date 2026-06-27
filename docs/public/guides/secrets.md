---
sources:
    - path: internal/config/config.go
      sha256: e0b3478819eb1034134b42549e866ca34fa4dc29f29cb214e9713ce24f6d4f99
    - path: internal/executor/executor.go
      sha256: 3e683bffe14af7c01b9e2860aa1d366a7f141b0ed5ff0d0dfe946ca488a0145f
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

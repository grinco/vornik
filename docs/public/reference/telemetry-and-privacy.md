# Anonymous usage telemetry

Vornik reports successful new installations and project creations to
`telemetry.vornik.io` when anonymous telemetry is enabled. It is enabled by
default and can be disabled during installation or at any time.

## What is reported

- event type: successful install or project creation;
- Vornik release version;
- operating-system and architecture categories;
- the Vornik-owned creation path;
- for project creation, a built-in template slug or `custom`, plus whether
  autonomy was enabled.

Vornik does not send an installation identifier, hostname, username, project
name or ID, filesystem path, repository, prompt, task content, configuration
value, API key, provider endpoint, model name, error text, precise timestamp,
or hashes of local values.

The reported Vornik version is a release identifier only. A build made from
source reports `dev` rather than its commit, so the version cannot single out
one machine's build.

The HTTPS service can see the source IP while handling a request. Vornik does
not include it in the payload, and the telemetry service must not retain the
full address or combine requests with CDN/WAF identifiers, cookies, or
fingerprints.

## What is retained

The endpoint keeps aggregate counts of the fields listed above — event type,
version, OS, architecture, creation path, template, autonomy flag — and nothing
else. Each is recorded only as one of a fixed set of values; anything
unrecognised is stored as `other` or `custom`. Counts live in edge logs for
about 7 days and in an aggregate dataset for about 90 days.

The request body is never parsed, stored, or forwarded, and no request is
recorded together with an address, cookie, header, or any other identifier, so
individual reports cannot be linked to each other or to a machine.

## Inspect or disable it

Show the effective setting and exact example requests without sending:

```bash
vornikctl telemetry sample
```

Disable it permanently in `config.yaml`:

```yaml
telemetry:
  enabled: false
```

For an unattended first installation, set:

```bash
VORNIK_TELEMETRY=off
```

An explicit `config.yaml` choice takes precedence over the environment.

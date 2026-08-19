# Anonymous individual lifecycle events

Vornik reports successful new installations and project creations to its
telemetry collector when anonymous lifecycle telemetry is enabled. It is enabled by
default and can be disabled during installation or at any time.

## What is reported

- event type: successful install or project creation;
- Vornik release version;
- operating-system and architecture categories;
- the Vornik-owned creation path;
- for project creation, a built-in template slug or `custom`, plus whether
  autonomy was enabled.

Vornik does not send a hostname, username, project name or ID, filesystem path,
repository, prompt, task content, configuration value, API key, provider
endpoint, model name, error text, or hashes of local values.

The reported Vornik version is a release identifier only. A build made from
source reports `dev` rather than its commit, so the version cannot single out
one machine's build.

The HTTPS service can see the source IP while handling a request. Vornik does
not include it in the payload. The collector records its server receive time so
installations can be distinguished and lifecycle events ordered; it must not
retain addresses, headers, cookies, or browser fingerprints.

## What is retained

The endpoint keeps one normalised event per report: a collector-generated event
UUID, its server receive time, and the fields listed above. Each categorical value is
restricted to a fixed set; anything unrecognised is rejected or becomes
`custom`. The default retention period is 730 days.

The request body is validated and rewritten to this closed schema before it is
stored. It is never stored with an address, cookie, header, or a client-side
identifier. Each event is independent and cannot be linked to an installation.

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

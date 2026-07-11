# GitHub App

!!! note "Community Edition"

    The Integrations Hub is part of the free, open-source **Community Edition** (and therefore also every Enterprise install). See [Editions](../editions.md) for the full Community vs Enterprise matrix.

Connects a GitHub App so vornik can react to issues and pull requests in
the repositories you choose. This integration is **project-scope** — it's
connected to one specific project, and if your account is scoped to a
single project the hub picks that project for you automatically.

You'll need a GitHub App already created in your organization's settings
before you start the guided form — see
[Conversation Channels → GitHub](../guides/conversation-channels.md#github)
for the App-creation and webhook-subscription steps.

## Guided-form fields

| Field | Required | Secret | What to enter |
|---|:---:|:---:|---|
| App ID | Yes | No | Shown on your GitHub App's settings page. |
| Installation ID | Yes | No | Shown in the URL after installing the App on your org/repo. |
| Private key | Yes | Yes | Generate a private key on your GitHub App's settings page and paste the whole `.pem` file. |
| Webhook secret | Yes | Yes | The secret you set on your GitHub App's webhook configuration page. |
| Allowed repositories | Yes | No | Comma-separated `owner/repo` entries this channel accepts events from, e.g. `myorg/myrepo`. |
| API base URL | No | No | Only needed for GitHub Enterprise Server; leave blank for github.com. |

The private key field is a paste-in of your App's whole `.pem` file — the
hub stores its content in a protected file for you rather than asking you
to manage a filesystem path yourself.

## What "Test connection" checks

The test mints a short-lived GitHub App installation token using the App
ID, installation ID, and private key you entered. On success, it reports
the installation's account login, confirming the App is correctly
configured and installed where you expect. The webhook secret and
repository allowlist aren't dialed out to GitHub during the test — they
govern which inbound webhook events vornik accepts once connected.

## Good to know

- All of App ID, installation ID, private key, webhook secret, and at
  least one allowed repository are required together — a GitHub App
  integration with only some of these set won't activate.
- Label matching on issues/pull requests, sender allowlisting, and other
  behavior once connected are covered in the **GitHub** section of
  [Conversation Channels](../guides/conversation-channels.md#github).

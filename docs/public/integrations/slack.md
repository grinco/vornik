# Slack

!!! note "Community Edition"

    The Integrations Hub is part of the free, open-source **Community Edition** (and therefore also every Enterprise install). See [Editions](../editions.md) for the full Community vs Enterprise matrix.

Connects a Slack workspace so vornik can answer direct messages and
`@`-mentions as a bot. This integration is **project-scope** — it's
connected to one specific project, and if your account is scoped to a
single project the hub picks that project for you automatically.

You'll need a Slack App already created (with the Events API enabled and
the right OAuth scopes) before you start the guided form — see
[Conversation Channels → Slack](../guides/conversation-channels.md#slack)
for the App-creation steps.

## Guided-form fields

| Field | Required | Secret | What to enter |
|---|:---:|:---:|---|
| Workspace (Team) ID | Yes | No | Starts with `T` — shown on your Slack App's "Basic Information" page. |
| Bot token | Yes | Yes | From your Slack App's "OAuth & Permissions" page — starts with `xoxb-`. |
| Signing secret | Yes | Yes | From your Slack App's "Basic Information" page, under App Credentials. |

## What "Test connection" checks

The test calls Slack's `auth.test` API with the bot token you entered. On
success, it reports the workspace and bot identity Slack returns,
confirming the token is valid and belongs to the workspace you specified.
On failure, it distinguishes an outright-rejected token from a network or
Slack-side hiccup.

## Good to know

- All three fields — workspace ID, bot token, and signing secret — are
  required before the Slack channel activates.
- Slash commands require one Slack-side step: create `/vornik`, point it at
  the same `/api/v1/slack/webhook` URL as Events API, add the `commands` OAuth
  scope, and reinstall the App. Then use `/vornik <prompt>`.
- Returning reports, rendered documents, and other task artifacts to the
  originating Slack thread requires the `files:write` OAuth scope. Adding a
  scope to an existing app requires reinstalling it to the workspace.
- Channel and sender allowlisting, and Slack's request-timestamp replay
  protection, are covered in the **Slack** section of
  [Conversation Channels](../guides/conversation-channels.md#slack).
- Slack throttles how fast a bot can post; vornik respects this
  automatically once connected.

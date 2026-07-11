# Telegram

!!! note "Community Edition"

    The Integrations Hub is part of the free, open-source **Community Edition** (and therefore also every Enterprise install). See [Editions](../editions.md) for the full Community vs Enterprise matrix.

Connects a Telegram bot vornik can send and receive messages through. This
integration is **daemon-scope** and **admin-only** — it's an
installation-wide setting, not tied to a single project — so it only
appears in the [Integrations Hub](index.md) for an administrator.

## Guided-form fields

| Field | Required | Secret | Where to find it |
|---|:---:|:---:|---|
| Bot token | Yes | Yes | Message `@BotFather` on Telegram, run `/newbot`, and copy the token it gives you. |

The bot token is the only field. It's a secret: once saved, it's written to
vornik's secret store, and the form only shows that it's configured — the
token itself is never redisplayed.

## What "Test connection" checks

The test calls Telegram's `getMe` API with the token you entered. On
success, it shows the bot's username (for example, "Connected as
@my_vornik_bot"), confirming the token is valid and Telegram recognizes the
bot. On failure, it distinguishes:

- **Invalid** — Telegram rejected the token outright (wrong or revoked
  token).
- **Couldn't reach the provider** — a network issue or a transient error on
  Telegram's side; try again.

Saving re-runs this same check first — a token the test rejects cannot be
saved.

## Good to know

- Only one Telegram bot can be connected at a time (daemon-scope means a
  single installation-wide setting).
- Once connected, who can talk to the bot and which project(s) they reach
  is controlled by `telegram.allowed_users` — see the
  [`telegram` configuration reference](../reference/configuration.md#telegram)
  for the full set of settings (rate limiting, session persistence, voice
  support, and more).

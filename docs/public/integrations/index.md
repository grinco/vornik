# Integrations Hub

!!! note "Community Edition"

    The Integrations Hub is part of the free, open-source **Community Edition** (and therefore also every Enterprise install). See [Editions](../editions.md) for the full Community vs Enterprise matrix.

The Integrations Hub, at **`/ui/integrations`** in the web UI, is the guided
way to connect the channels and tools vornik talks through — without
hand-editing a YAML file or an `.env` file. Every credential you enter is
**tested before it's saved**, so you find out immediately whether a token
works, instead of watching a message silently fail to arrive later.

If you prefer to configure a channel by editing YAML directly, see the
[Conversation Channels](../guides/conversation-channels.md) guide instead —
both routes write to the same configuration, so you can start in the hub and
finish by hand, or the other way around.

## The four integrations

| Integration | What it's for | Docs |
|---|---|---|
| **Telegram** | A Telegram bot vornik can send and receive messages through | [Telegram](telegram.md) |
| **Email** | An inbox vornik reads over IMAP and replies to over SMTP | [Email](email.md) |
| **GitHub App** | A GitHub App vornik uses to react to issues and pull requests | [GitHub App](github-app.md) |
| **Slack** | A Slack workspace vornik answers in as a bot | [Slack](slack.md) |

Each integration's tile links to its own page above, with the exact fields
its guided form asks for and what the connection test checks.

Looking for **MCP tool servers**? They're daemon-wide infrastructure with
their own management surface, not a hub tile — see
[MCP tool servers](mcp.md) for where to manage them in each edition.

## Connect → test → save

Every integration in the hub follows the same three steps:

1. **Fill in the guided form.** Each field carries a plain-language hint for
   where to find the value (for example, "Message @BotFather → /newbot →
   copy the token it gives you"). Fields marked **secret** hold a credential
   — a bot token, a password, a signing secret — and are never redisplayed
   once saved; you can replace a secret, but you can't read it back through
   the UI.
2. **Test the connection.** Before anything is written, the hub calls the
   provider with exactly the values you entered — Telegram's `getMe`,
   Slack's `auth.test`, an IMAP/SMTP login, or a GitHub App
   installation-token mint — and shows a plain result:
   green "Connected as ..." or red "Invalid — the provider rejected this."
   A separate red state, "Couldn't reach the provider — try again," means
   the network or the provider had a hiccup, not that your credential is
   wrong.
3. **Save.** Saving re-runs the same test first — **a credential that fails
   the test cannot be saved**. On success, any secret field is written to
   vornik's secret store and the configuration file gets only a
   placeholder that names it, never the literal value. Non-secret fields
   (a workspace ID, a server URL, a list of allowed repositories) are
   written as-is.

After saving, the integration's tile becomes a **health tile**: it shows
the result of the last test and a "Re-check" button so you can confirm
it's still working, on demand — the hub does not silently re-probe your
credentials in the background.

## Project scope vs. daemon scope

The four integrations split into two scopes, and it affects who can connect
them and where the setting lives:

- **Daemon-scope** (Telegram) — an admin-only, installation-wide setting.
  It's visible only to an administrator, and it doesn't belong to any one
  project.
- **Project-scope** (Email, GitHub App, Slack) — these belong to a specific
  project. A project-scoped user only ever sees their own project's tiles
  for these three; an administrator sees every project's tiles, each
  labeled with its project.

If your account is scoped to a single project, the hub picks that project
for you automatically. An administrator managing several projects chooses
which project a tile applies to.

## What never happens

- **A credential never lands on disk as plain text.** The configuration
  file only ever gets a placeholder that names where the real value is
  stored.
- **A failing test is never silently saved.** If the connection test
  fails, saving is refused — there's no "save anyway."
- **A secret is never sent back to your browser.** Once a secret field is
  saved, the form shows only that it's configured, never its value.
- **Nothing is probed just by opening the page.** Loading the catalog or a
  form never dials out to a provider — only "Test connection," "Save," and
  "Re-check" do.

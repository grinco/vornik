# Email

!!! note "Community Edition"

    The Integrations Hub is part of the free, open-source **Community Edition** (and therefore also every Enterprise install). See [Editions](../editions.md) for the full Community vs Enterprise matrix.

Connects a mailbox vornik reads over IMAP and replies to over SMTP, so
anyone can interact with it just by sending a message. This integration is
**project-scope** — it's connected to one specific project, and if your
account is scoped to a single project the hub picks that project for you
automatically.

## Guided-form fields

| Field | Required | Secret | What to enter |
|---|:---:|:---:|---|
| IMAP host | Yes | No | Your mail provider's IMAP server, e.g. `imap.gmail.com`. |
| IMAP port | No | No | Usually `993` (implicit TLS); leave blank for the default. |
| IMAP username | Yes | No | Usually your full email address. |
| IMAP password | Yes | Yes | An app-specific password if your provider requires one (e.g. Gmail). |
| SMTP host | Yes | No | Your mail provider's SMTP server, e.g. `smtp.gmail.com`. |
| SMTP port | No | No | Usually `587` (STARTTLS); leave blank for the default. |
| SMTP username | Yes | No | Usually your full email address. |
| SMTP password | Yes | Yes | Often the same app-specific password as IMAP. |
| From address | Yes | No | The address your outbound replies appear to come from. |

The guided form always collects a complete, working setup — both the
inbound (IMAP) and outbound (SMTP) legs — because the connection test
checks both in one pass.

## What "Test connection" checks

The test logs into your IMAP server with the credentials you entered, then
separately connects to your SMTP server and completes the SMTP handshake
(`EHLO`, `STARTTLS`, authentication) — without sending any mail. A pass
means vornik can both read your inbox and send through your outbound
server; a failure tells you which leg rejected the credentials so you know
which password or host to check.

## Good to know

- For most providers (Gmail included), you'll need to turn on two-factor
  authentication and generate an app-specific password rather than using
  your normal account password.
- Once connected, which senders may trigger vornik, polling interval, and
  attachment handling are configured separately — see the **Email**
  section of the [Conversation Channels](../guides/conversation-channels.md)
  guide for the full set of options.

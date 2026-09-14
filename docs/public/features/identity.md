# User accounts and channel/key→account mapping

!!! note "Community Edition — OIDC / SSO is Enterprise"

    User accounts, channel linking and key ownership ship in the **Community
    Edition**. Federated login (OIDC / SSO providers) is part of the
    **Enterprise Edition**. See [Editions](../editions.md).

vornik reaches people through many doors: the web console, the `vornikctl`
CLI and REST API, Telegram, Slack, email, other agents over A2A. Each of those
used to authorise on its own terms — a Telegram sender on the bot's allowlist,
an API key on its project, a browser session on its login. Granting someone web
access did nothing for Slack, and revoking it left their Telegram binding live.

The identity core replaces that with **one account resolver for every door**.
A user account has zero or more **channel bindings** (a Telegram sender id, a
Slack user in a workspace, a GitHub login for Enterprise SSO) and may **own**
API keys. Every door resolves the caller to the same account and the same
effective permissions, so:

- revoking an account revokes it everywhere, immediately;
- a rating, a chat message and an API call made by the same person are
  attributed to the same person — which is what makes vornik's adaptation
  data (ratings, instincts, memory) say *who* thought something, not just
  *that* someone did.

## Turning it on

```bash
vornikctl doctor feature enable identity
```

This sets `identity.enabled` and hot-reloads. Nothing changes for existing
callers until you link them: an unlinked chat sender is refused (or, during a
migration, admitted only with its old ordinary-chat permissions — see below),
and an unowned API key behaves exactly as it always has.

## Linking a chat identity to an account

Linking is a two-sided act so that nobody can bind a channel to an account
they do not hold:

1. A signed-in account holder asks for a **link code** — from the console
   (*Account → Link a channel*) or `vornikctl account link-code`. Codes are
   high-entropy, single-use, stored only as a hash, and expire after
   `identity.link_code_ttl` (default ten minutes).
2. In the chat channel, the sender redeems it: `/account link <code>`.

The binding is the channel **plus the installation** (the Slack workspace, the
Telegram bot) **plus the immutable sender id** — never a display name, email
or chat id, all of which can be reused by someone else. `/account status`
shows what the sender is bound to; `/account unlink` removes the binding and
ends the sender's access through that channel.

The existing Telegram `/link` command is unrelated: it consolidates a person's
*profile* across channels for personalisation and grants nothing. A profile
link never becomes an account binding.

## Owning an API key

An operator can assign an API key to an account (`vornikctl key owner set`,
or the key's row in the console). Ownership **narrows**, never widens: a key's
effective permission is the intersection of what the key was minted with and
what its owner currently holds. Disabling or deleting the owner denies the
key outright. A key with no owner keeps its original authority — the
per-task keys vornik mints for its own agents are deliberately unowned.

## Migrating from allowlists

`identity.channel_compat: legacy` keeps a sender that is explicitly listed in
the channel's old allowlist admitted with its **old ordinary-chat permissions
only**, so a deployment can link people one at a time. In that mode:

- an empty allowlist still admits nobody;
- a linked-but-denied, disabled or unresolvable sender is never re-admitted
  through the allowlist;
- the compat path never opens the configuration assistant;
- every admission is counted (`vornik_auth_legacy_allowlist_grants_total`)
  and `vornikctl account migration-report` lists who is still relying on it.

New installs default to `strict`. Switch an existing one when the report is
empty and the counter has been zero for a while.

## Configuration

| key | default | meaning |
|---|---|---|
| `identity.enabled` | `false` | turn the identity core on |
| `identity.channel_compat` | `strict` | `strict` or `legacy` (see above) |
| `identity.link_code_ttl` | `10m` | link-code lifetime |
| `identity.link_code_rate_per_hour` | `10` | issuance / redemption cap per hour |

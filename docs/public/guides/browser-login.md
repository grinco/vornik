# Signing in to the console

Two mechanisms, one per edition, and they mint the same kind of session.

- **Enterprise** signs people in through an identity provider (OIDC/SSO).
- **Community** exchanges an API key for a browser session. That is this page.

Both end at the same place: a `vornik_session` cookie the daemon authenticates
on every request. Neither stores a password — vornik has no password store, and
the `users` table has no password column.

## What the Community exchange is for

An API key authenticates a *possessor*, not a person. It is the same credential
whether a shell script uses it or you type it into a browser. The exchange
turns "this caller holds a credential" into "this browser is acting as this
account, for a bounded time, revocably" — which is the part Community was
missing, rather than a login page.

## Enabling it

Off by default. In the **deployed** config tree (`~/.config/vornik/configs/` —
the daemon reads only the deployed copy, never your source tree):

```yaml
auth:
  session:
    credential_exchange: true
    lifetime: 168h          # optional, 7 days by default
    idle_timeout: 24h       # optional, empty disables idle cut-off
```

Restart the daemon — this is boot-time wiring, not hot-reloaded. You should see:

```
credential session exchange enabled: POST /api/v1/auth/session
```

**If that line is absent the route answers 503 and nothing is broken on your
side.** Two gates decline quietly and each logs a Warn saying which: no
identity core (the daemon cannot answer "whose key is this"), or no session
store. A 503 means "this deployment does not offer the door", which is
deliberately distinct from a rejected credential.

## Using it

```bash
curl -X POST https://your-daemon/api/v1/auth/session \
     -H "Authorization: Bearer $VORNIK_API_KEY" -i
```

A `204` with three `Set-Cookie` headers is success. The key must already be
**mapped to an account** — an unmapped key is refused exactly like an invalid
one, on purpose (see *Why every refusal looks the same*).

The credential goes in the `Authorization` header and nowhere else. Never a
query parameter, never a form field, never `localStorage`: a key in a URL
reaches the access log, the browser history, the `Referer` of every subsequent
navigation, and any proxy in between.

## What a session may do

**A session never carries more than the key that minted it currently has.**
Checked on every request, not at sign-in:

- the key still exists, is not revoked and has not expired, **and**
- it is still mapped to the same account, **and**
- the account is not disabled.

Any of those failing returns 401 and ends the session immediately — not at a
cache expiry. So **revoking a key logs out the browsers it signed in**, on
their next request. That includes a key *reassigned* to somebody else: it stops
carrying the previous owner's session.

Grants are not frozen into the session either. What the session may do is
resolved from the identity core per request, the same way it is for chat and
every other channel.

## TLS

The exchange **refuses to mint a session cookie over plaintext HTTP.** A
session cookie in the clear is the whole session: anything on the path can take
it and become that account until it expires.

Terminate TLS in front of the daemon — every supported topology does. For a
local development box only:

```yaml
auth:
  session:
    allow_insecure_exchange: true   # development only
```

It logs a Warn on every boot while it is on, so a config copied from a dev box
to a real one announces itself before a cookie travels in the clear.

## Why every refusal looks the same

A bad key, a key mapped to nobody, a disabled account and a cross-origin
request all return the same 401 with the same message. That is deliberate: the
endpoint already tells a caller whether a credential is live, and that much is
unavoidable in a login. What it must not do is tell them *which* of those
applied — those are facts about somebody else's account.

Rate limiting is the one exception, answering `429` so a legitimate caller can
back off. It shares one budget per key with the key-claim path, because both
doors prove possession of the same credential. **A consequence worth knowing:**
someone hammering a key on either door closes both for that key. If that locks
an administrator out, the recourse is the local break-glass CLI on the daemon
host.

## Cross-site request forgery

Once a session exists, mutating requests that arrive with the cookie are
checked twice: a same-origin signal (`Sec-Fetch-Site`, falling back to
`Origin`, failing closed when neither is present), and a double-submit token.

**If you are writing a browser client against this API**, read the
`vornik_csrf` cookie and echo it in the `X-Vornik-CSRF` header on every POST,
PUT, PATCH and DELETE. Without it the request is refused with 403 and the
daemon logs a line naming the signals that drove the decision.

Callers using `Authorization: Bearer` — the CLI, MCP clients, A2A peers — skip
all of this. A browser never attaches that header on its own, so those requests
cannot be forged this way.

## What this does not give you

- **No self-service anything.** No sign-up, no password reset, no "forgot my
  access". An administrator issues credentials; someone who loses theirs needs
  another issued.
- **No SSO in Community.** That is an Enterprise feature and deliberately stays
  one.
- **Machine credentials are unchanged.** The CLI, A2A and agents keep using API
  keys directly and never take a session. A browser session is for a human in a
  browser; anything else holding one is a smell.

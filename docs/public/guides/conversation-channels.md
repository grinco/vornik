---
sources:
    - path: docs/operator/conversation-channels.md
      sha256: 794a5984e14a4dc3560173aee9dc842a6d0c877682ca7d56077b3e08e8509870
---
# Conversation Channels

Conversation channels let people reach vornik from the tools they already use.
Connect a channel and inbound messages turn into tasks or chat turns, while
vornik's replies flow back out the same way they came in. vornik ships four
channels: **GitHub**, **Slack**, **email**, and **voice messages**
(experimental).

You configure each channel per project, in that project's YAML file. After you
change a channel's settings, reload the project configuration — you do not need
to take the whole service down to pick up new channels.

## Things every channel has in common

- **Secrets never live in YAML.** Tokens, passwords, and signing secrets are
  supplied through environment variables. The YAML only names the environment
  variable to read (for example `bot_token_env`), so your config files stay
  safe to commit.
- **Allowlists are your front door.** Each channel has a sender allowlist and,
  where it applies, a channel/repo allowlist. Leaving an allowlist empty means
  "accept everyone" — fine while you are testing, but you should tighten it
  before going to production.
- **Routing is automatic.** A single channel connection can serve several
  projects when the upstream service gives a stable identifier (a GitHub
  installation, a Slack workspace, an email login). vornik routes each inbound
  message to the right project for you.

!!! tip
    Start every channel with an allowlist locked down to just the people and
    places you expect. Loosen it once you have confirmed the flow works.

---

## GitHub

The GitHub channel lets vornik respond to issues and pull requests in the
repositories you choose. It can:

- Open a task when an issue gets a label you have nominated.
- Open a review task when a pull request is opened.
- Reply when someone mentions `@vornik` on an issue or pull request.

### Setup

1. Create a GitHub App in your organization's settings.
2. Point the App's webhook URL at your vornik host's GitHub webhook endpoint
   (`/api/v1/github-app/webhook`).
3. Generate a webhook secret, download the App's private key, and note the App
   ID and installation ID.
4. Subscribe the App to issue, issue-comment, pull-request, and
   pull-request-review-comment events.
5. Grant the installation read and write access to issues and pull requests on
   every repository you want vornik to handle.

### Configuration

```yaml
github_app:
  app_id: 1234567
  private_key_path: /path/to/github-app.pem
  installation_id: 89012345
  webhook_secret_env: VORNIK_GITHUB_APP_WEBHOOK_SECRET
  repo_allowlist:
    - "your-org/your-repo"
  task_labels:        # labels that create a task (the label name becomes the task type)
    - "vornik"
    - "research"
  pr_review_labels:   # leave empty to review every opened PR
    - "needs-review"
  sender_allowlist:   # GitHub login names allowed to trigger vornik
    - "your-github-login"
```

Supply the webhook secret in the environment before starting vornik:

```bash
export VORNIK_GITHUB_APP_WEBHOOK_SECRET="your-webhook-secret"
```

A webhook secret and a non-empty `repo_allowlist` are the minimum for vornik to
*receive* events. To let it *reply*, also set `app_id`, `private_key_path`, and
`installation_id`.

### Good to know

- Label matching is **case-sensitive** — GitHub keeps your label's exact
  casing, so `Vornik` and `vornik` are different.
- An empty `pr_review_labels` means *every* opened pull request creates a review
  task. List specific labels to require at least one of them.
- `sender_allowlist` matches GitHub usernames, including bots. If you want a bot
  to be able to trigger vornik, add it explicitly.
- If you are testing by re-sending a webhook, change the delivery so it looks
  new — vornik deduplicates redelivered events so it never double-comments.

---

## Slack

The Slack channel lets vornik answer in direct messages, when it is
`@`-mentioned in a channel, or when it is mentioned in a public channel it has
been allowed into.

### Setup

1. Create a Slack App at <https://api.slack.com/apps>.
2. Enable the **Events API** and set the request URL to your vornik host's Slack
   webhook endpoint (`/api/v1/slack/webhook`). vornik answers Slack's
   verification challenge automatically the first time.
3. Subscribe the bot to the `app_mention`, `message.im`, and `message.channels`
   events.
4. Under **OAuth & Permissions**, add the `app_mentions:read`, `chat:write`,
   `im:history`, `im:read`, and `channels:history` scopes.
5. Install the App to your workspace and capture the signing secret and the bot
   token (it starts with `xoxb-`).

### Configuration

```yaml
slack:
  team_id: "T01234567"
  signing_secret_env: VORNIK_SLACK_SIGNING_SECRET
  bot_token_env: VORNIK_SLACK_BOT_TOKEN
  channel_allowlist:
    - "C0ABCDEF12"
  sender_allowlist:
    - "U0AAAAAAA"
```

Supply the secrets in the environment before starting vornik:

```bash
export VORNIK_SLACK_SIGNING_SECRET="your-signing-secret"
export VORNIK_SLACK_BOT_TOKEN="xoxb-..."
```

A signing secret and `team_id` are the minimum to receive events; the bot token
is what lets vornik reply.

### Good to know

- An empty `channel_allowlist` accepts **every** channel in the workspace.
  Restrict it in production so the bot is not pulled into channels you did not
  intend.
- Slack rejects requests whose timestamps are more than five minutes off, a
  standard replay defense. If valid messages get rejected, check that your
  server's clock is in sync.
- Slack limits how fast a bot can post (roughly one message per second per
  channel). vornik respects this automatically and waits when Slack asks it to.

---

## Email

The email channel reads inbound mail over IMAP and replies over SMTP, so anyone
can interact with vornik just by sending it a message. It handles multipart and
HTML mail, decodes international character sets, and saves attachments as task
artifacts.

### Setup

Bring your own IMAP and SMTP service. The channel works with common providers.
For Gmail, for example:

1. Turn on two-factor authentication for the account.
2. Create an App Password and use it for both IMAP and SMTP.
3. Use `imap.gmail.com:993` (TLS) and `smtp.gmail.com:587` (STARTTLS).
4. If you want vornik to read from a folder other than `INBOX`, create it first
   and set `imap_mailbox`.

### Configuration

```yaml
email:
  imap_host: imap.gmail.com
  imap_port: 993
  imap_username: assistant@example.com
  imap_password_env: VORNIK_ASSISTANT_IMAP_PASSWORD
  imap_mailbox: "INBOX"
  smtp_host: smtp.gmail.com
  smtp_port: 587
  smtp_username: assistant@example.com
  smtp_password_env: VORNIK_ASSISTANT_SMTP_PASSWORD
  from_address: "Assistant <assistant@example.com>"
  sender_allowlist:
    - "you@example.com"
    - "example.com"            # bare domains are allowed too
  poll_interval: "60s"
  attachment_size_cap_bytes: 26214400   # 25 MiB
  attachment_store_dir: /path/to/email-attachments
```

Supply the credentials in the environment before starting vornik:

```bash
export VORNIK_ASSISTANT_IMAP_PASSWORD="your-imap-password"
export VORNIK_ASSISTANT_SMTP_PASSWORD="your-smtp-password"
```

The IMAP host, username, and password are the minimum to receive mail. Add the
SMTP block and `from_address` to let vornik reply; an inbound-only setup is
valid if you only want it to act on incoming mail.

### Good to know

- `sender_allowlist` accepts full addresses (`alice@example.com`) and bare
  domains (`example.com`). An empty allowlist accepts everyone — tighten it in
  production.
- Set `attachment_size_cap_bytes` to a sensible limit. Leaving it unset means
  *unlimited*, so a single huge attachment could land in your artifact store.
  Matching your mail provider's per-message cap (Gmail's is 25 MiB) is a good
  default.
- Set `attachment_store_dir` the first time you enable the channel. If it is
  empty, attachment contents are silently dropped.
- The channel polls on `poll_interval` (60 seconds by default). Very short
  intervals are accepted but discouraged.
- Optional inbound authentication checking (`verify_inbound_auth`) relies on
  your mail server stamping SPF/DKIM results. If you turn it on, start with the
  `relaxed` policy and confirm legitimate mail passes before moving to
  `strict`.

---

## Voice messages

!!! warning "Experimental"
    Voice messages are an **experimental** feature. The transcription/synthesis
    pipeline and its configuration may change between releases, and quality
    depends on your chosen STT/TTS models. Use it for evaluation rather than
    production-critical conversations for now.

The voice channel adds spoken conversation to Telegram and Slack. When someone
sends a voice message, vornik transcribes it, treats the transcript as a normal
message, and — if the conversation started in voice — speaks its reply back.

A conversation's "voice mode" is sticky: once someone speaks, vornik keeps
replying with audio until the next message comes in as text.

The current release uses local, open-weight speech engines that run on the same
host as vornik:

- **Speech-to-text:** [whisper.cpp](https://github.com/ggerganov/whisper.cpp)
  with a Whisper model file.
- **Text-to-speech:** [Piper](https://github.com/rhasspy/piper) with a Piper
  voice.
- **Audio conversion:** `ffmpeg`, built with Opus support for Telegram and AAC
  for Slack.

Install those binaries and download the model files, then point the
configuration at them.

### Configuration

```yaml
voice:
  stt:
    provider: "whisper-local"
    model: "/path/to/ggml-base.en.bin"
    binary_path: "/usr/local/bin/whisper-cpp"
    ffmpeg_path: "/usr/bin/ffmpeg"
    language_hint: "en"        # leave empty to auto-detect
  tts:
    provider: "piper"
    voice: "/path/to/en_US-amy-medium.onnx"
    binary_path: "/usr/local/bin/piper"
    ffmpeg_path: "/usr/bin/ffmpeg"
    speed: 1.0
    max_text_runes: 1500
```

You can configure `stt` alone (vornik transcribes incoming audio and replies
with text) or `tts` alone (vornik reads its replies aloud) — you do not need
both.

### Good to know

- `max_text_runes` caps how much text gets spoken (1500 is about 90 seconds).
  Telegram limits voice messages to 60 seconds, so set this to suit your
  channel; vornik trims overly long replies rather than failing silently.
- Your `ffmpeg` must be built with Opus support for Telegram audio and AAC for
  Slack audio. A minimal build may be missing these.
- Use the smallest Whisper model that is accurate enough for you. On a machine
  without a GPU, larger models can take many seconds to transcribe a clip.
- For Piper, point `voice` at the `.onnx` file; its matching `.onnx.json`
  must sit beside it.

---

## A note on history and replies

Conversation history belongs to the conversation, not to the channel, so vornik
remembers context across messages even if a channel reconnects. vornik sends one
complete reply per turn rather than streaming it word by word (live typing
indicators are specific to the Telegram and web chat experiences).

For tuning the model behavior and safety controls that sit behind every reply,
see [Workflows and LLM controls](workflows-and-llm-controls.md).

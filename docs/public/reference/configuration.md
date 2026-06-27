<!-- Generated from source — do not edit by hand. -->

# Configuration reference

vornik reads its configuration from `config.yaml`. The keys below are the customer-facing settings, by dotted YAML path.

## steering_notifications_enabled

| Key | Type | Description |
|---|---|---|
| `steering_notifications_enabled` | bool | Push a chat/DM prompt when a task needs operator steering (input/approval). Default true. |

## steering_operator_alert

| Key | Type | Description |
|---|---|---|
| `steering_operator_alert` | struct | Fallback chat recipient alerted when an ownerless autonomy task needs steering (channel/session[/address]). |

## cluster

| Key | Type | Description |
|---|---|---|
| `cluster` | struct | Cluster-diagnostics: expected endpoints + monitor interval. |

## named_secrets

| Key | Type | Description |
|---|---|---|
| `named_secrets` | list | Per-secret allowlist of env credentials injected into agent containers, scoped by project. |

## server

| Key | Type | Description |
|---|---|---|
| `server.address` | string | TCP listen address for the HTTP API. |
| `server.read_timeout` | string | Max time to read a request. |
| `server.write_timeout` | string | Max time to write a response. |
| `server.unix_socket` | string | Optional Unix socket path the API is also served on, alongside the TCP address. Required for zero-egress agents. |
| `server.real_ip.enabled` | bool | Honour the trusted client-IP header from trusted_proxies. Off ⇒ always key on RemoteAddr. |
| `server.real_ip.trusted_proxies` | list | CIDRs/IPs trusted to set the real-IP header. List ONLY the cloudflared host. |
| `server.real_ip.header` | string | Trusted client-IP header. Default CF-Connecting-IP. |
| `server.public_base_url` | string | Public origin (scheme://host[:port]) for git clone/push HTTPS URLs; used only to render the clone URL on the project page. |

## database

| Key | Type | Description |
|---|---|---|
| `database.host` | string | PostgreSQL host. |
| `database.port` | int | PostgreSQL port. |
| `database.name` | string | Database name. |
| `database.user` | string | Database user. |
| `database.password` | string | Password. Prefer the VORNIK_DATABASE_PASSWORD environment variable. |
| `database.sslmode` | string | One of disable, require, verify-ca, verify-full. |

## storage

| Key | Type | Description |
|---|---|---|
| `storage.artifacts_path` | string | Local directory for artifacts when using the filesystem backend. |
| `storage.backend` | string | filesystem (local disk) or s3 (S3-compatible object store, including MinIO). |
| `storage.s3.endpoint` | string | S3 endpoint URL. Leave empty for AWS S3; set for MinIO/non-AWS. |
| `storage.s3.region` | string | AWS region. Required when backend is s3. |
| `storage.s3.bucket` | string | Bucket holding artifacts. Required when backend is s3; must already exist. |
| `storage.s3.prefix` | string | Optional key prefix to namespace one bucket across deployments. |
| `storage.s3.access_key_id` | string | Access key. Prefer VORNIK_STORAGE_S3_ACCESS_KEY_ID. |
| `storage.s3.secret_access_key` | string | Secret key. Prefer VORNIK_STORAGE_S3_SECRET_ACCESS_KEY. |
| `storage.s3.use_path_style` | bool | Force path-style addressing. Set true for MinIO. |
| `storage.s3.force_ssl` | bool | Require HTTPS. Set false only for local MinIO development. |

## artifacts

| Key | Type | Description |
|---|---|---|
| `artifacts.artifacts_path` | string | Local directory for artifacts when using the filesystem backend. |
| `artifacts.backend` | string | filesystem (local disk) or s3 (S3-compatible object store, including MinIO). |
| `artifacts.s3.endpoint` | string | S3 endpoint URL. Leave empty for AWS S3; set for MinIO/non-AWS. |
| `artifacts.s3.region` | string | AWS region. Required when backend is s3. |
| `artifacts.s3.bucket` | string | Bucket holding artifacts. Required when backend is s3; must already exist. |
| `artifacts.s3.prefix` | string | Optional key prefix to namespace one bucket across deployments. |
| `artifacts.s3.access_key_id` | string | Access key. Prefer VORNIK_STORAGE_S3_ACCESS_KEY_ID. |
| `artifacts.s3.secret_access_key` | string | Secret key. Prefer VORNIK_STORAGE_S3_SECRET_ACCESS_KEY. |
| `artifacts.s3.use_path_style` | bool | Force path-style addressing. Set true for MinIO. |
| `artifacts.s3.force_ssl` | bool | Require HTTPS. Set false only for local MinIO development. |

## runtime

| Key | Type | Description |
|---|---|---|
| `runtime.run_as_user` | string | Override the container user (uid, uid:gid, or user:group). Use to force non-root on root-default images. |
| `runtime.default_network` | string | Default network policy for agent roles: host, none, or daemon-only (zero egress; requires server.unix_socket). |
| `runtime.agent_llm.endpoint` | string | OpenAI-compatible base URL for the LLM agents call. Falls back to the chat section when empty. |
| `runtime.agent_llm.api_key` | string | API key for the agent LLM endpoint. |
| `runtime.agent_llm.model` | string | Default model for agents. |
| `runtime.agent_llm.context_size` | int | Context-window tokens. |
| `runtime.agent_llm.max_tokens` | int | Max output tokens per call. |
| `runtime.agent_llm.timeout` | string | Bound on a single LLM call from inside an agent. |
| `runtime.warm_pool.enabled` | bool | Reuse warm containers for faster task start. |
| `runtime.warm_pool.idle_timeout` | string | How long an idle warm container is kept. |
| `runtime.warm_pool.max_per_role` | int | Max warm containers per project and role. |
| `runtime.project_workspace_path` | string | Base directory for per-project persistent workspaces. |
| `runtime.delegation_depth_limit` | int | Max nesting depth for delegation chains (0 = default). |
| `runtime.delegation_fanout_limit` | int | Max child tasks one parent may spawn in a batch (0 = default). |

## scheduler

| Key | Type | Description |
|---|---|---|
| `scheduler.max_concurrent_tasks` | int | Maximum tasks running at once across all projects. |
| `scheduler.lease_timeout` | string | How long a task lease is held before it is considered stale. |

## watchdog

| Key | Type | Description |
|---|---|---|
| `watchdog.enabled` | bool | Periodic scan for stuck executions. |
| `watchdog.interval` | string | Gap between scans. |
| `watchdog.stuck_threshold` | string | Idle time before an execution is flagged. |
| `watchdog.action` | string | warn (log only) or fail (mark the task terminal). |

## effective_cost

| Key | Type | Description |
|---|---|---|
| `effective_cost.enabled` | bool | Turn the drift monitor on. |
| `effective_cost.interval` | string | Evaluation cadence. |
| `effective_cost.ratio_threshold` | float | Current/baseline cost ratio that triggers an alert. |

## secrets

| Key | Type | Description |
|---|---|---|
| `secrets.enabled` | bool | Turn detection on. |
| `secrets.allowlist` | list | Extra regexes appended to the default allowlist. |
| `secrets.checkpoints` | map | Map a channel to an action: detect, redact, or block. |

## retention

| Key | Type | Description |
|---|---|---|
| `retention.enabled` | bool | Turn the background sweeper on. |
| `retention.interval` | string | How often the sweeper runs. |
| `retention.task_llm_usage_days` | int | Days to keep per-task LLM usage rows. |
| `retention.tasks_days` | int | Days to keep task rows. |
| `retention.executions_days` | int | Days to keep execution rows. |
| `retention.artifacts_days` | int | Days to keep artifacts. |
| `retention.response_cache_days` | int | Days to keep cached LLM responses (30d recommended on busy deployments). |

## metrics

| Key | Type | Description |
|---|---|---|
| `metrics.enabled` | bool | Expose a Prometheus metrics endpoint. |
| `metrics.addr` | string | Listen address for the dedicated metrics server. |
| `metrics.require_admin` | bool | Require the admin key on the main port metrics endpoint. |

## tracing

| Key | Type | Description |
|---|---|---|
| `tracing.enabled` | bool | Enable OpenTelemetry tracing. |
| `tracing.endpoint` | string | OTLP gRPC endpoint for trace export. |

## logging

| Key | Type | Description |
|---|---|---|
| `logging.level` | string | Log level (e.g. info, debug). |
| `logging.format` | string | Log format (e.g. json, text). |
| `logging.forward.enabled` | bool | Enable centralised log forwarding to remote sinks. |
| `logging.forward.scopes` | list | Allowlist of log categories to forward. Empty ships all scopes. |
| `logging.forward.queue_size` | int | Bounded forward queue size; overflow is dropped + counted. |
| `logging.forward.batch_size` | int | Max events per shipped batch. |
| `logging.forward.flush_interval` | string | Max time a partial batch waits before shipping (e.g. 5s). |
| `logging.forward.http.enabled` | bool | Enable the HTTP webhook log sink. |
| `logging.forward.http.url` | string | Collector ingest URL the HTTP sink POSTs NDJSON to. |
| `logging.forward.http.bearer_token_env` | string | Env-var NAME holding the HTTP sink bearer token (never the token itself). |
| `logging.forward.http.timeout` | string | Per-request HTTP sink timeout (e.g. 5s). |
| `logging.forward.http.max_retries` | int | Max HTTP sink retries on 5xx/transport error. |
| `logging.forward.syslog.enabled` | bool | Enable the syslog log sink. |
| `logging.forward.syslog.address` | string | host:port of the syslog collector. |
| `logging.forward.syslog.protocol` | string | Syslog transport: udp \| tcp \| tls. |
| `logging.forward.syslog.ca_file` | string | Optional PEM CA bundle for syslog tls (system roots otherwise). |

## api

| Key | Type | Description |
|---|---|---|
| `api.auth_enabled` | bool | Require a valid API key on every request. Turn this on for any network-reachable deployment. |
| `api.auth_dry_run` | bool | Evaluate and log auth verdicts without enforcing. Cannot be set together with auth_enabled: true. |
| `api.api_keys` | list | Static bearer keys. Prefer per-project DB-backed keys via vornikctl key. |
| `api.rate_limit.per_ip.rps` | int | Per-IP sustained request rate (unauthenticated backstop). |
| `api.rate_limit.per_ip.burst` | int | Per-IP burst capacity. |
| `api.rate_limit.per_ip.trusted_proxies` | list | Deprecated: use server.real_ip.trusted_proxies. CIDRs/IPs trusted to set X-Forwarded-For. |

## chat

| Key | Type | Description |
|---|---|---|
| `chat.enabled` | bool | Turn the chat client on. |
| `chat.provider` | string | http (OpenAI-compatible) or router (multi-provider). |
| `chat.router.default` | string | Sub-provider used when no route matches. Required for router. |
| `chat.router.claude_cli.effort_level` | string | CLI reasoning effort: low\|medium\|high\|xhigh\|max (default low); honored by claude-cli, ignored by codex-cli. |
| `chat.router.claude_subscription.thinking_budget` | int | Anthropic extended-thinking budget_tokens; 0 disables (router subprovider). |
| `chat.router.codex_cli.effort_level` | string | CLI reasoning effort: low\|medium\|high\|xhigh\|max (default low); honored by claude-cli, ignored by codex-cli. |
| `chat.router.codex_subscription.effort_level` | string | Codex reasoning effort: low\|medium\|high (empty = omit). |
| `chat.router.http.enabled` | bool | Enable the OpenAI-compatible sub-provider. |
| `chat.router.http.endpoint` | string | Endpoint URL for the HTTP sub-provider. |
| `chat.router.http.api_key` | string | API key for the HTTP sub-provider. |
| `chat.router.http.model` | string | Default model for the HTTP sub-provider. |
| `chat.router.vertex.enabled` | bool | Enable Google Vertex AI (Gemini). |
| `chat.router.vertex.api_key` | string | Vertex API key. |
| `chat.router.vertex.project_id` | string | GCP project that owns the Vertex endpoint. |
| `chat.router.vertex.location` | string | Vertex region; defaults to global. |
| `chat.router.vertex.model` | string | Default Vertex model. |
| `chat.router.openrouter.enabled` | bool | Enable the OpenRouter sub-provider. |
| `chat.router.openrouter.api_key` | string | OpenRouter API key. |
| `chat.router.openrouter.model` | string | OpenRouter default model. |
| `chat.router.openrouter.free_only` | bool | Reject any non-:free model — a hard guard against accidental spend. |
| `chat.router.bedrock.enabled` | bool | Enable AWS Bedrock (credentials via the AWS SDK chain). |
| `chat.router.bedrock.region` | string | Bedrock region. |
| `chat.router.bedrock.model` | string | Bedrock default model. |
| `chat.endpoint` | string | OpenAI-compatible base URL (single-provider mode). |
| `chat.api_key` | string | API key for the endpoint. Prefer an environment variable. |
| `chat.model` | string | Default model identifier when a request does not pin one. |
| `chat.wizard_model` | string | Model for the project-setup wizard. |
| `chat.timeout` | string | Bound on a single LLM round-trip. |
| `chat.dispatch_timeout` | string | Bound on one complete interactive (multi-call) turn. |
| `chat.max_history` | int | Max conversation messages kept. |
| `chat.max_history_tokens` | int | Soft token budget for history trimming. |
| `chat.compaction.enabled` | bool | Summarize overflow turns into a topic gist instead of dropping them. |
| `chat.compaction.max_gist_terms` | int | Topics retained in the compaction gist (0 = default 24). |
| `chat.max_tool_iterations` | int | Tool-call loop cap per dispatcher turn. |
| `chat.max_concurrent_requests` | int | Max in-flight chat backend calls; excess queues. |
| `chat.context_size` | int | Context-window tokens. |
| `chat.max_tokens` | int | Max output tokens per call. |
| `chat.prompt_cache_mode` | string | Provider-native prompt caching: off, auto (recommended), or prefix. |

## autonomy

| Key | Type | Description |
|---|---|---|
| `autonomy.default_evaluate_timeout` | string | Bound on one evaluation tick when a project does not set its own. |
| `autonomy.circuit_breaker.enabled` | bool | Auto-pause autonomy on a project that fails repeatedly. |
| `autonomy.circuit_breaker.threshold` | int | Failure count that trips the breaker. |
| `autonomy.circuit_breaker.window` | string | Rolling window the count is measured over. |
| `autonomy.approval_timeout_hours` | int | Cancel tasks awaiting approval after this many hours (0 = never; default 96). |

## telegram

| Key | Type | Description |
|---|---|---|
| `telegram.enabled` | bool | Turn the Telegram bot on. |
| `telegram.bot_token` | string | Bot token. Prefer an environment variable. |
| `telegram.allowed_users` | map | Map of Telegram user ID to access. true = full access; a list of project IDs scopes the user. Absent users are denied. |
| `telegram.rate_limit` | int | Requests per minute per user. |
| `telegram.session_path` | string | Path for conversation persistence (empty = none). |
| `telegram.session_ttl` | string | Auto-expire idle sessions (e.g. 24h). |
| `telegram.dispatcher_max_iterations` | int | Tool-call loop cap per chat turn. |
| `telegram.forum_chat_id` | int | Supergroup ID for one forum topic per task (0 = off). |
| `telegram.web_ui_base_url` | string | Public base URL of the web UI, used in onboarding links. |

## memory

| Key | Type | Description |
|---|---|---|
| `memory.enabled` | bool | Activate the memory subsystem. |
| `memory.sufficiency.enabled` | bool | Widen-and-retry recall when too few high-relevance hits (requires the reranker). |
| `memory.sufficiency.min_high_rel` | int | Hits at/above score_floor that count as 'enough' (0 = default 3). |
| `memory.sufficiency.score_floor` | float | Absolute reranker relevance floor in [0,1] (0 = default 0.6). |
| `memory.sufficiency.max_rounds` | int | Hard cap on retrieval rounds (<=1 = single shot; 0 = default 3). |
| `memory.reranker.enabled` | bool | LLM-rerank context-assembly recall (the pre-delegation hint) and activate scored-sufficiency. Adds one LLM call (seconds) to that path only; the interactive memory_search tool and companion recall stay fast RRF. |
| `memory.reranker.model` | string | Reranker model id; small OSS recommended. Empty = chat router default (often overkill). |
| `memory.reranker.max_candidates` | int | Top-K results scored per recall (0 = default 20). |
| `memory.reranker.timeout_seconds` | int | Per-recall rerank timeout in seconds (0 = default 15). |
| `memory.reranker.max_snippet_bytes` | int | Per-candidate snippet sent to the reranker (0 = default 600). |
| `memory.embedding_model` | string | Embedding model name. Required when enabled. |
| `memory.embedding_dimension` | int | Vector dimension produced by the model. |
| `memory.embedding_endpoint` | string | Override endpoint for embedding requests. |
| `memory.embedding_api_key` | string | Override API key for embedding requests. |
| `memory.embedding_cache_enabled` | bool | Cache embeddings to skip repeat API calls. |
| `memory.response_cache_enabled` | bool | Cache memory-pipeline LLM responses. |
| `memory.chunk_tokens` | int | Approximate tokens per chunk. |
| `memory.chunk_overlap` | int | Token overlap between adjacent chunks. |
| `memory.worker_concurrency` | int | Embed-queue worker goroutines. |
| `memory.graph.enabled` | bool | Knowledge-graph extraction pipeline. |
| `memory.titler.enabled` | bool | Generate a short topic label per chunk (one LLM call each). |
| `memory.classifier.enabled` | bool | LLM content-class backfill. |
| `memory.prompt_injection_scan` | string | Ingest prompt-injection gate: off (default), detect, or quarantine. |
| `memory.claim_audit_disabled_projects` | list | Project IDs that skip the claim-audit/hallucination ingest gate. |
| `memory.deny_patterns` | list | Substring deny-list (NOT regex) that quarantines matching content at memory ingest. |

## instinct

| Key | Type | Description |
|---|---|---|
| `instinct.enabled` | bool | Master switch for the instinct layer. |
| `instinct.cadence_seconds` | int | Extraction-worker tick interval. |
| `instinct.lookback_seconds` | int | How far back each tick scans. |
| `instinct.min_support` | int | Corroborating outcomes before a pattern goes active. |
| `instinct.active_confidence` | float | Confidence floor for an active instinct. |
| `instinct.decay_halflife_days` | int | Recency half-life for confidence decay. |
| `instinct.consumers.failure_playbooks` | bool | Surface recovery instincts on failed-task views. |
| `instinct.consumers.architect_priors` | bool | Feed workflow instincts to the architect as priors. |
| `instinct.consumers.memory_hygiene` | bool | Feed retrieval instincts to memory sweepers. |
| `instinct.consumers.tool_budget` | bool | Let learned budget instincts fill an absent complexity tier (default off). |
| `instinct.consumers.auto_apply.enabled` | bool | Promote high-confidence recovery instincts to a prompt-level directive (default off). |
| `instinct.consumers.auto_apply.min_confidence` | float | Confidence floor for auto-apply (default 0.85). |
| `instinct.consumers.auto_apply.min_clean_support` | int | Min clean (zero-contradiction) supports for auto-apply; 0 = off. |
| `instinct.consumers.auto_apply.allowed_error_classes` | list | Failure classes eligible for auto-apply; empty = all. |

## auth

| Key | Type | Description |
|---|---|---|
| `auth.bootstrap_admins` | list | Channel identities (channel:external_id) auto-granted admin on first login. |
| `auth.external_base_url` | string | Public origin of the daemon (scheme://host[:port]). Required when a login provider is configured. |
| `auth.session.lifetime` | string | Fixed session expiry. |
| `auth.session.idle_timeout` | string | Cut sessions idle longer than this. |
| `auth.providers.github.client_id` | string | GitHub OAuth App client ID. |
| `auth.providers.github.client_secret_file` | string | Path to the GitHub OAuth client secret (preferred over inline). |
| `auth.providers.github.org` | string | Restrict/soft-gate login to a GitHub org. |
| `auth.providers.github.org_member_role` | string | Auto-grant role for GitHub org members on login: empty (default, no auto-grant), 'user', or 'admin'. Requires 'org' set. |
| `auth.providers.github.org_member_projects` | list | Projects an auto-granted org member (user role) receives; '*' = all projects. |

## voice

| Key | Type | Description |
|---|---|---|
| `voice.stt.provider` | string | Speech-to-text provider (whisper-local). |
| `voice.stt.model` | string | Absolute path to the STT model file. |
| `voice.tts.provider` | string | Text-to-speech provider (piper). |
| `voice.tts.voice` | string | Absolute path to the TTS voice model. |

## node

| Key | Type | Description |
|---|---|---|
| `node.profile` | string | Role preset: all\|ui\|worker\|webhook. Default all. |
| `node.serve_ui` | bool | Mount /ui routes. |
| `node.serve_api` | bool | Mount data-plane / control endpoints. |
| `node.serve_webhooks` | bool | Mount public webhook ingress. |
| `node.run_workers` | bool | Run scheduler+executor and leader-elected singletons. |
| `node.relay.upstream` | string | Job-tier internal ingress base URL (https://host:8443). |
| `node.relay.client_cert` | string | PEM client cert for mTLS to the job tier. |
| `node.relay.client_key` | string | PEM client key for mTLS to the job tier. |
| `node.relay.ca` | string | PEM CA bundle that signed the job-tier server cert. |
| `node.relay.max_retries` | int | Bounded relay retries before 5xx to provider. 0 → 3. |
| `node.relay.timeout` | string | Per-relay-attempt timeout. Empty → 5s. |
| `node.relay_ingress.addr` | string | Listen address for the mTLS relay ingress, e.g. :8443. |
| `node.relay_ingress.server_cert` | string | PEM server cert presented to relaying webhook nodes. |
| `node.relay_ingress.server_key` | string | PEM server key for the relay ingress. |
| `node.relay_ingress.client_ca` | string | PEM CA bundle; only client certs it signed are accepted. |


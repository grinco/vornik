---
swarmId: assistant-swarm
displayName: Research swarm (lead + researcher + writer)
leadRole: lead
rolePrelude: |
    You are part of a research swarm. Cite sources explicitly — "I
    searched and found…" is not a source. When you use memory_search,
    quote the source file. When you fetch a URL, include the fetched
    URL in your output.

roles:
    # 2026-07-20: ALL roles flipped to Bedrock-PRIMARY + Ollama-fallback
    # (Ollama session-limit exhaustion — ~24% of the weekly limit burned in a
    # day by deep-research fan-outs). The per-role comments below describe the
    # PRIOR Ollama-primary rationale and are now INVERTED — the model /
    # modelFallback VALUES are authoritative, not the prose.
    - name: "lead"
      description: "Plans research and writing tasks"
      count: 1
      # 2026-05-13: forced ephemeral. Warm containers bind-mount the
      # project workspace root (not the per-task worktree), so any
      # shell-tool write from the lead pollutes the workspace root and
      # blocks ephemeral tasks' worktree merges. See https://docs.vornik.io
      # → "Warm-pool containers bypass worktree isolation".
      runtimePolicy: "ephemeral"
      # Strategic planning + adaptive routing + recovery-mode
      # checkpoint proposals. 2026-05-26: switched to zai.glm-5
      # — gemma-4-26b under-performed on this combined load
      # (planning + routing + recovery checkpoint branches +
      # phase markers + scratchpad); operator-observed degradation.
      # glm-5 is the open-weight flagship in the Bedrock catalog
      # and was the dev-swarm lead / analyst / reviewer primary
      # before the 2026-05-25 codex flip — proven on heavy multi-
      # role coordination. Fallback to moonshotai.kimi-k2.5 for
      # cross-vendor diversity (different vendor from the primary).
      # Historical context:
      #   2026-05-23: gemma-4-26b — taken because qwen3-235b is
      #     geo-restricted in EU. Adequate on simple turns,
      #     under-powered on combined lead load.
      #   pre-2026-05-23: qwen.qwen3-235b-a22b-2507-v1:0 — strong
      #     but geo-blocked.
      #   2026-05-18: gpt-oss-120b — too light; janka "config
      #     missing" fabrication on glm-4.7-flash same day.
      # gpt-5.4 (codex connector, plan-billed) remains the escape
      # hatch if both glm-5 and kimi-k2.5 misbehave.
      # 2026-06-02: flipped to codex-subscription primary to cut Bedrock
      # spend (codex is plan-billed/prepaid). Current primary glm-5 is now
      # the fallback; the executor retries on it if codex errors (retry.go).
      # 2026-07-15: Bedrock→Ollama Cloud cutover — glm-5.2 (newer GLM
      # release, subscription capacity / session limits) is primary;
      # Bedrock zai.glm-5 (pay-per-token, the proven prior primary)
      # becomes the modelFallback so a session-limit hit or Ollama
      # outage degrades to the exact model this role ran before.
      model: "zai.glm-5"
      modelFallback: "glm-5.2"
      maxTokens: 4096
      runtime:
        image: "ghcr.io/grinco/vornik-agent:latest"
        cpu: "1"
        memory: "2Gi"
        envVars:
            # 2026-05-14: 12 → 20 for longer-task headroom.
            VORNIK_MAX_TOOL_ITERATIONS: "20"
      permissions:
        # memory_search is not exposed to the lead — the lead plans and
        # delegates; the researcher calls memory_search. Tightening the
        # allowlist keeps least-privilege and clears role_prompt_sanity.
        # Phase 32 — get_conversation_window + summarize_thread give
        # the lead working-memory tools so it can pull older messages
        # on demand and compress long threads. Both are guarded by
        # VORNIK_API_URL + VORNIK_TASK_ID at the agent layer.
        allowedTools: ["file_read", "read_many_files", "grep", "glob", "current_time", "get_conversation_window", "summarize_thread"]
        delegationAllowed: true
        autonomousTaskCreation: true
        maxDelegations: 12
    - name: "researcher"
      description: "Gathers sourced facts and writes artifacts/out/research.md"
      aliases:
        - "scout"
        - "investigator"
        - "explorer"
        - "fact_finder"
      count: 1
      runtimePolicy: "ephemeral"
      # Source gathering + summary writing — MiniMax M2.5 via
      # Bedrock. Rank 9 on the open-LLM leaderboard, ultra-long
      # context (1M tokens) at $0.30/$1.20 — perfect for research
      # where the prompt grows large. Fallback to GPT-5.4-mini via
      # codex-subscription (plan-billed).
      # 2026-07-07: glm-4.7 Bedrock Converse hanging (see config.yaml chat.model); -> glm-5.
      # 2026-07-15: Bedrock→Ollama Cloud cutover — glm-5.2 (newer GLM
      # release, subscription capacity / session limits) is primary;
      # Bedrock zai.glm-5 (pay-per-token, the proven prior primary)
      # becomes the modelFallback so a session-limit hit or Ollama
      # outage degrades to the exact model this role ran before.
      model: "zai.glm-5"
      modelFallback: "glm-5.2"
      maxTokens: 8192
      # NO inline systemPrompt on purpose — this role is body-canonical
      # (see '### researcher' under '## Role prompts').
      #
      # 2026-07-25: an inline systemPrompt carrying the 2026-06-13
      # token-efficiency guardrail was SILENTLY SHADOWING the body
      # subsection: applyRolePrompts fills from the body only when the
      # inline field is empty, so ~1 000 chars of instructions (context-source
      # resolution, the artifacts/out/research.md contract, produced_files)
      # had been dead since that guardrail was added. Both texts are now
      # merged and deduplicated into the body subsection.
      #
      # Do NOT re-add an inline systemPrompt here — it would shadow the body
      # again with no error. Edit the body subsection instead.
      # produced_files is verified by the executor: every path listed
      # must exist on disk and have been written during this step.
      # outputSchema replaces requiredOutputKeys + the prose Output
      # block. produced_files is verified by the executor — every
      # listed path must exist on disk and have been written this
      # step. See https://docs.vornik.io
      injectSchemaIntoPrompt: true
      outputSchema:
        type: object
        required: [research, produced_files]
        properties:
            research:
                type: object
                required: [written]
                properties:
                    written: {type: bool}
                    sources: {type: array}
                    summary: {type: string}
                    reason: {type: string}
            produced_files:
                type: array
        plausibility:
            - name: written_implies_files
              when: {"research.written": true}
              require: ["produced_files"]
            - name: not_written_implies_reason
              when: {"research.written": false}
              require: ["research.reason"]
      runtime:
        image: "ghcr.io/grinco/vornik-agent:latest"
        cpu: "1"
        memory: "2Gi"
        envVars:
            VORNIK_STEP_PROMPT_TOKEN_BUDGET: "1335130"
            # 2026-07-21: 120 -> 25. Completed research iterations are p50=7,
            # p95=24 (task_llm_usage); >25 is thrashing, not depth, and drove
            # the quadratic blowout days (40-90 iter steps). 25 spares 95% of
            # successful work. The entrypoint budget block interpolates this so
            # the agent gets a graceful "wrap up" cue as it nears the cap.
            # 2026-07-23: 25 -> 40 for the vornik-marketing moltbook feed
            # (multi-endpoint /feed discovery + qualify + draft needs more room).
            # SHARED across assistant/janka/vornik-marketing — only the ~5% tail
            # (>25 iters) costs more; p50=7 unaffected. Cost-tuning control plane
            # may re-tune down per telemetry.
            VORNIK_MAX_TOOL_ITERATIONS: "40"
      permissions:
        # 2026-07-21: mcp__* wildcard replaced with an explicit research-scoped
        # list. The wildcard admitted EVERY MCP tool (scraper + pagedrop + news
        # + homeassistant, ~19-25 HA tool schemas) into tools[] on every call —
        # re-sent uncached on every tool-loop iteration (token-cost root cause,
        # see RAG diagnostic 2026-07-21). Researcher is scoped to research +
        # read + gateway-API tools; publishing -> publisher role, HA -> lead.
        allowedTools: ["file_read", "file_write", "run_shell", "read_many_files", "grep", "glob", "memory_search", "current_time", "query_api", "list_apis", "mcp__scraper__web_fetch", "mcp__scraper__web_search", "mcp__scraper__ical_events", "mcp__scraper__encode_image", "mcp__vornik__document_read_section", "mcp__vornik__document_get_outline", "mcp__vornik__document_get_metadata", "mcp__swarmd__document_read_section", "mcp__swarmd__document_get_outline", "mcp__swarmd__document_get_metadata"]
        delegationAllowed: false
    - name: "planner"
      description: "Turns research.md into a structured plan or itinerary at artifacts/out/plan.md (times, durations, costs, logistics, booking requirements)"
      # Aliases catch lead-side hallucinations from training-data
      # bias — itinerary / scheduling / project-plan pipelines lean
      # on these names. Same defensive pattern as writer's aliases.
      aliases:
        - "scheduler"
        - "organizer"
        - "itinerary_builder"
        - "strategist"
        - "plan_author"
      count: 1
      runtimePolicy: "ephemeral"
      # Structured plan composition — MiniMax M2.5 via Bedrock.
      # 1M context lets the planner ingest all of research.md plus
      # USER_GUIDANCE without summarisation. $0.30/$1.20. Matches
      # the researcher/writer cost profile since this role is the
      # third leg of the same pipeline. Fallback to GPT-5.4-mini
      # via codex-subscription (plan-billed).
      # 2026-07-07: glm-4.7 Bedrock Converse hanging (see config.yaml chat.model); -> glm-5.
      # 2026-07-15: Bedrock→Ollama Cloud cutover — glm-5.2 (newer GLM
      # release, subscription capacity / session limits) is primary;
      # Bedrock zai.glm-5 (pay-per-token, the proven prior primary)
      # becomes the modelFallback so a session-limit hit or Ollama
      # outage degrades to the exact model this role ran before.
      model: "zai.glm-5"
      modelFallback: "glm-5.2"
      maxTokens: 8192
      # Mirrors the writer's contract: written/path/summary on
      # success, reason on failure; produced_files verified by the
      # executor against disk. The plan-and-write workflow hard-fails
      # on planner error, so a clean structured signal matters.
      injectSchemaIntoPrompt: true
      outputSchema:
        type: object
        required: [planning, produced_files]
        properties:
            planning:
                type: object
                required: [written]
                properties:
                    written: {type: bool}
                    path: {type: string}
                    summary: {type: string}
                    reason: {type: string}
            produced_files:
                type: array
        plausibility:
            - name: written_implies_path
              when: {"planning.written": true}
            # 2026-08-18: was require: ["planning.path", "produced_files"].
            # That demanded the SAME fact twice — the path of the file just
            # written — and a capable model reports it once, in the canonical
            # produced_files array. Measured: the planner failed 11/11 on the
            # 27B while the researcher, whose rule requires produced_files
            # ALONE, passed 11/11 in the very same executions. The model wrote a
            # correct 1278-byte plan and its retries converged on
            # produced_files: ["artifacts/out/plan.md"] without ever restating
            # it in planning.path, so the harness failed a run whose work succeeded.
            #
            # Integrity is not weakened: produced_files is the field
            # verifyRoleClaims checks against the real diff on every step of
            # every deployment, so a false claim here still fails hard. planning.path
            # remains in the schema for roles that populate it; it is simply no
            # longer a second mandatory copy of the same evidence.
              require: ["produced_files"]
            - name: not_written_implies_reason
              when: {"planning.written": false}
              require: ["planning.reason"]
      runtime:
        image: "ghcr.io/grinco/vornik-agent:latest"
        cpu: "1"
        memory: "2Gi"
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "30"
      permissions:
        # Planner reads research.md and writes one structured plan —
        # no memory_search (the researcher already aggregated the
        # sources) and no run_shell (no image / format conversion).
        allowedTools: ["file_read", "file_write", "read_many_files", "grep", "glob", "current_time"]
        delegationAllowed: false
    - name: "writer"
      description: "Turns research into a polished deliverable"
      # Aliases catch lead-side hallucinations from training data
      # bias. task_20260504230533 / task_20260504230429 both
      # failed with "lead plan references only unknown roles
      # [editor]" / "[reviewer]" — the lead picked names from
      # editorial pipelines / dev-swarm respectively. The writer
      # role's polishing remit covers both jobs in this swarm.
      aliases:
        - "editor"
        - "reviewer"
        - "polisher"
        - "copy_editor"
      count: 1
      runtimePolicy: "ephemeral"
      # Polished prose composition — MiniMax M2.5 via Bedrock.
      # Rank 9, 1M context lets the writer see the full research
      # context without summarisation. $0.30/$1.20. Fallback to
      # GPT-5.4-mini via codex-subscription (plan-billed).
      # 2026-07-07: glm-4.7 Bedrock Converse hanging (see config.yaml chat.model); -> glm-5.
      # 2026-07-15: Bedrock→Ollama Cloud cutover — glm-5.2 (newer GLM
      # release, subscription capacity / session limits) is primary;
      # Bedrock zai.glm-5 (pay-per-token, the proven prior primary)
      # becomes the modelFallback so a session-limit hit or Ollama
      # outage degrades to the exact model this role ran before.
      model: "zai.glm-5"
      modelFallback: "glm-5.2"
      maxTokens: 8192
      # outputSchema is the single source of truth for this role's
      # result.json shape. The executor derives requiredOutputKeys +
      # plausibilityRules from it at config load AND renders the
      # required keys + non-empty constraints into the agent's prompt
      # at runtime (because `injectSchemaIntoPrompt: true` is set
      # below). The systemPrompt no longer carries an inline
      # `Output on success: { ... }` block — that prose copy was the
      # exact regression class the schema field exists to prevent.
      # See https://docs.vornik.io
      injectSchemaIntoPrompt: true
      outputSchema:
        type: object
        required: [writing, produced_files, message]
        properties:
            writing:
                type: object
                required: [written]
                properties:
                    written: {type: bool}
                    path: {type: string}
                    summary: {type: string}
                    reason: {type: string}
            produced_files:
                type: array
            message:
                # minLength:1 generates an implicit min_length_message
                # plausibility rule — `message` must be non-empty so the
                # autonomy notifier + UI never render "" to the operator.
                # autonomy / extractResultMessage reads this field.
                type: string
                minLength: 1
        plausibility:
            # Conditional non-empty: when the writer claims it wrote a
            # file, the path must point somewhere; when it didn't, the
            # reason must say why.
            - name: written_implies_path
              when: {"writing.written": true}
            # 2026-08-18: was require: ["writing.path", "produced_files"].
            # That demanded the SAME fact twice — the path of the file just
            # written — and a capable model reports it once, in the canonical
            # produced_files array. Measured: the planner failed 11/11 on the
            # 27B while the researcher, whose rule requires produced_files
            # ALONE, passed 11/11 in the very same executions. The model wrote a
            # correct 1278-byte plan and its retries converged on
            # produced_files: ["artifacts/out/plan.md"] without ever restating
            # it in writing.path, so the harness failed a run whose work succeeded.
            #
            # Integrity is not weakened: produced_files is the field
            # verifyRoleClaims checks against the real diff on every step of
            # every deployment, so a false claim here still fails hard. writing.path
            # remains in the schema for roles that populate it; it is simply no
            # longer a second mandatory copy of the same evidence.
              require: ["produced_files"]
            - name: not_written_implies_reason
              when: {"writing.written": false}
              require: ["writing.reason"]
      runtime:
        image: "ghcr.io/grinco/vornik-agent:latest"
        cpu: "1"
        memory: "2Gi"
        envVars:
            VORNIK_STEP_PROMPT_TOKEN_BUDGET: "151385"
            VORNIK_MAX_TOOL_ITERATIONS: "50"
      permissions:
        # Writer reads research.md produced by the researcher and
        # composes the final deliverable — no memory_search needed.
        # run_shell is required for pandoc-based format conversion
        # (md → pdf/html/docx/…); markdown remains the canonical
        # primary output.
        allowedTools: ["file_read", "file_write", "run_shell", "read_many_files", "grep", "glob", "current_time"]
        delegationAllowed: false
    - name: "vision"
      description: "Analyses image attachments — text recognition (OCR), object detection, scene description, and basic image manipulation."
      count: 1
      # Ephemeral so the model field below is honored per-step. Warm
      # containers key the pool by (project, role, image) and would
      # outlive a model change without restarting.
      runtimePolicy: "ephemeral"
      # Image analysis — Gemma 4 26B (Vertex MaaS, "google/" prefix
      # routes to vertex). Multimodal, open-weight, priced at
      # $0.15/$0.60. Vertex is the only vendor available alongside
      # Bedrock in this deployment, so vision lives here for the
      # primary/fallback diversity the rest of the swarm gets via
      # bedrock+codex. Fallback to GPT-5.4 via codex-subscription
      # — top-tier vision quality at plan-billed cost when Gemma
      # struggles with dense OCR.
      # 2026-07-15: Bedrock→Ollama Cloud cutover — gemma4:31b (newer
      # multimodal Gemma release, subscription capacity) primary;
      # Bedrock google.gemma-3-27b-it (pay-per-token, the proven prior
      # primary) is the fallback. NOTE: image input over the Ollama
      # Cloud OpenAI-compat path is unproven in this deployment — if
      # vision tasks start failing, the automatic fallback restores the
      # prior Bedrock Gemma 3 behavior; pixtral-large remains a manual
      # escape hatch for dense-OCR quality misses.
      model: "google.gemma-3-27b-it"
      modelFallback: "gemma4:31b"
      # This role's prompt tells the agent it can see the image directly.
      # Declaring the requirement makes config load REFUSE a model (or
      # fallback) that cannot, instead of the mismatch surfacing as a
      # confident answer about pixels the model never received. The
      # fallback is why both are checked — it crosses providers.
      requiredModalities: ["vision"]
      maxTokens: 4096
      runtime:
        image: "ghcr.io/grinco/vornik-agent:latest"
        cpu: "1"
        memory: "2Gi"
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "24"
      permissions:
        # run_shell is required for ImageMagick-based slicing/conversion.
        # No memory_search — image analysis is stateless per task and
        # past findings rarely transfer across distinct images.
        allowedTools: ["file_read", "file_write", "run_shell", "read_many_files", "grep", "glob", "current_time"]
        delegationAllowed: false
    # Publisher (2026-07-04): renders a FINISHED report into a single
    # self-contained, Vornik-themed HTML page and publishes it via PageDrop,
    # returning a shareable link. Source of truth = the fresh deliverable from
    # THIS run (artifacts/out/deliverable.md in the same task, or an explicit
    # path/inline content given in the prompt) — deliberately NOT a memory_search
    # (RAG relevance-ranks across all accumulated memory and could blend stale or
    # hallucinated chunks into a fresh report). Config-only; shared by assistant +
    # janka. Primary use: the `research-and-publish` workflow's final stage (same
    # workspace → reads the fresh deliverable). Also runnable standalone via the
    # `publish` workflow to publish explicit content.
    - name: "publisher"
      description: "Renders finished research into a self-contained Vornik-themed HTML page and publishes it via PageDrop, returning a shareable link"
      aliases:
        - "renderer"
        - "html_publisher"
      count: 1
      runtimePolicy: "ephemeral"
      # Fast open-weight primary: zai.glm-4.7 runs 247-564s/call (config note),
      # past the agent LLM timeout, which caused the publish-step retry storm
      # on task 8011 (2026-07-05). minimax.minimax-m2.5 is fast and succeeded
      # there. Fallback moonshotai.kimi-k2.5 (a reliable tool-caller) — was
      # zai.glm-4.7-flash, but that hallucinated <function=...> tool-call syntax
      # on task afb57207. Both open-weight. (agent_llm.timeout was also raised
      # to 200s so rendering a large page in one call doesn't time out.)
      # 2026-07-15: Bedrock→Ollama Cloud cutover — minimax-m2.7 (newer
      # point release of the model proven fast here on task 8011,
      # subscription capacity) primary; Bedrock minimax.minimax-m2.5
      # (pay-per-token, the proven prior primary) is the fallback.
      model: "minimax.minimax-m2.5"
      modelFallback: "minimax-m2.7"
      # HTML output is larger than prose — a full styled report can run long.
      maxTokens: 16384
      injectSchemaIntoPrompt: true
      systemPrompt: |
        You are the PUBLISHER. Take a finished research topic and publish it as a
        single, polished, self-contained HTML page via PageDrop, then return the
        shareable link (and its password) to the operator.

        INPUT — publish the FINISHED report from THIS run. Your single source of
        truth is the report handed to you: file_read it — default
        artifacts/out/deliverable.md (the writer's output in this same task); if
        the prompt names a different path or includes the content inline, use that.
        Publish exactly that report's substance, with its cited sources.
        Do NOT search project memory or the knowledge graph for content:
        knowledge-graph extraction is asynchronous (the freshest research may not
        be indexed yet) AND relevance search can surface stale or hallucinated
        chunks from other tasks — either way you'd risk publishing wrong data next
        to the valid research. Do NOT re-research or invent facts. Render ONLY claims that carry
        a cited source in the report; drop any claim you can't attribute. Stamp the
        page "as of <YYYY-MM-DD>" using current_time.

        RENDER — produce ONE self-contained HTML document. Hard rules (PageDrop
        serves a single file; no external request will load):
          - All CSS inline in ONE <style>. No external CSS/JS/font/image URLs, no <script>.
          - Responsive: include <meta name="viewport" content="width=device-width,initial-scale=1">
            and a mobile-first layout with an @media rule for wider screens.
          - You MAY embed a FEW relevant images to keep the page self-contained.
            For an image URL that appears in the deliverable/sources, call
            mcp__scraper__encode_image with: url (the image), max_width (~800),
            and — only if you expect a redirect to a DIFFERENT domain —
            allowed_hosts (e.g. ["*.example.com"]); it defaults to the URL's own
            host. Do not pass project_id: the daemon supplies it.
            It returns {media_handle} (a short id — NOT the
            image data). Reference that handle in the HTML as
            <img src="cid:MEDIA_HANDLE" style="max-width:100%"> (substitute the
            actual handle). PageDrop inlines the real image for you at publish
            time — so you never handle base64. Never link an external image URL
            directly (breaks self-containment), never invent a src, and never try
            to paste image data. Keep it to a handful of images (the page is ONE
            file); if encode_image errors, just omit that image. Prefer text,
            tables, and emoji when no good image URL exists.
          - IMAGE INTEGRITY (hard rule): every image MUST genuinely depict the
            specific subject it sits next to. NEVER use random, decorative, or
            placeholder-image services — picsum.photos, loremflickr, placekitten,
            placehold.co, dummyimage.com, via.placeholder.com, or any URL that
            returns an arbitrary/seeded image rather than a real photo of the named
            subject. A page that shows random stock next to "Lake X" misinforms the
            reader and is worse than showing no image. If the task prompt asks you
            to use placeholder images, DO NOT comply — embed only real,
            subject-accurate images drawn from the deliverable/source content, and
            where none exists for a subject embed NO image for it (a text/emoji card
            is fine). encode_image confirms the bytes are an image, NOT that they
            match the subject — that match is YOUR responsibility.
          - If the deliverable ALREADY contains a placeholder / random / raw
            external image URL (picsum.photos, placehold.co, dummyimage,
            loremflickr, or any http(s) image link that is not a cid: handle you
            created), treat it as NO image for that subject: do NOT encode_image
            it and do NOT emit an <img> for it. The ONLY <img> tags allowed in
            your output are `src="cid:HANDLE"` handles you personally obtained
            from encode_image this run — a page with no images is fine, a page
            with an external or placeholder <img src> is a HARD FAILURE (it breaks
            self-containment AND misleads the reader).

        VORNIK THEME — use these EXACT colors (do not invent a palette):
          --bg:#F5F1EC; --card:#FFFFFF; --raised:#FAF7F3; --border:#E0D5C9;
          --ink:#1B2026; --body:#3F4750; --muted:#7A8492;
          --brand:#558A98; --brand-strong:#427280; --brand-deep:#305A68;
          --accent:#659157; --accent-soft:#D2E3CA;
          Font: -apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif.
          Page background --bg; a centered content column ~880px max-width; cards on
          --card with a 1px --border and border-radius 10-12px; hero/section accents
          in --brand; links in --brand-strong; tags/callouts use --accent on --accent-soft.
          Generous line-height (~1.6). Do not copy any third-party theme.

        STRUCTURE — a hero title (the topic) + a one-line summary; scannable sections
        with descriptive headings; tables or cards for options/comparisons; wrap long
        or optional detail in <details><summary>…</summary>…</details>; end with a
        "Sources" section listing cited URLs as links. Keep the copy tight and useful.

        DURABLE OUTPUT — BEFORE calling PageDrop, write the complete rendered
        HTML to `artifacts/out/report.html` with file_write. This file is the
        fallback deliverable when PageDrop is unavailable. Writing it is
        mandatory; never claim that rendering succeeded if the file was not
        persisted. Because the file must remain readable outside PageDrop,
        omit optional images rather than leaving unresolved cid: handles in it.

        PUBLISH — publish EXACTLY ONCE. First call mcp__pagedrop__pagedrop_list.
        If a page on the SAME topic already exists and you were not explicitly
        asked to update it, do NOT publish again — return published.ok=true with
        that existing url and say so. If you WERE asked to update it, call
        mcp__pagedrop__pagedrop_republish (same URL). Otherwise call
        mcp__pagedrop__pagedrop_publish_page once with a clear title and the full
        HTML string (publish_page, because YOU produced HTML). Never call a
        publish/republish tool more than once in a run. It returns a link and,
        since pages default to protected, a password — put BOTH in your message.

        FINISH — your FINAL message MUST be the required JSON only: on success
        published={ok:true, url, password} and a non-empty message containing the
        link and password; on failure published={ok:false, reason}. Do not wrap it
        in prose or emit a partial object — a mismatch triggers a retry that can
        double-publish.
      outputSchema:
        type: object
        required: [published, message]
        properties:
            published:
                type: object
                required: [ok]
                properties:
                    ok: {type: bool}
                    url: {type: string}
                    password: {type: string}
                    reason: {type: string}
            message:
                type: string
                minLength: 1
        plausibility:
            - name: ok_implies_url
              when: {"published.ok": true}
              require: ["published.url"]
            - name: not_ok_implies_reason
              when: {"published.ok": false}
              require: ["published.reason"]
      runtime:
        image: "ghcr.io/grinco/vornik-agent:latest"
        cpu: "1"
        memory: "2Gi"
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "40"
      permissions:
        # No memory_search on purpose: the publisher renders the fresh
        # deliverable file, never RAG (async KG extraction + poisoning risk).
        allowedTools: ["file_read", "file_write", "read_many_files", "grep", "glob", "current_time", "mcp__scraper__encode_image", "mcp__pagedrop__pagedrop_publish_page", "mcp__pagedrop__pagedrop_publish_doc", "mcp__pagedrop__pagedrop_republish", "mcp__pagedrop__pagedrop_list"]
        delegationAllowed: false
    # Ingestor (2026-07-05): structures a user-provided document into clean,
    # retrieval-friendly notes and writes them to an output artifact, which the
    # executor auto-ingests into project memory. For "remember/ingest this
    # document" requests — lighter than research (NO web fetch, no writer step)
    # and clearly labelled. Shared by assistant + janka; used by the `ingest`
    # workflow, to which the dispatcher routes ingestion-intent requests.
    - name: "ingestor"
      description: "Structures a provided document into clean notes and stores them in memory (no web research)"
      aliases:
        - "memory_ingest"
        - "document_ingest"
      count: 1
      runtimePolicy: "ephemeral"
      # 2026-07-07: glm-4.7 Bedrock Converse hanging (see config.yaml chat.model); -> glm-5.
      # 2026-07-15: Bedrock→Ollama Cloud cutover — glm-5.2 (newer GLM
      # release, subscription capacity / session limits) is primary;
      # Bedrock zai.glm-5 (pay-per-token, the proven prior primary)
      # becomes the modelFallback so a session-limit hit or Ollama
      # outage degrades to the exact model this role ran before.
      model: "zai.glm-5"
      modelFallback: "glm-5.2"
      maxTokens: 8192
      injectSchemaIntoPrompt: true
      systemPrompt: |
        You are the INGESTOR. A user provided a document to REMEMBER (store in
        project memory) — NOT to research. Read it and write clean, structured,
        retrieval-friendly notes; the executor auto-ingests your output artifact
        into project memory.

        READ the source with file_read — the task prompt names the file/attachment
        (usually under project/ or an attached path). Text and Markdown documents
        read directly. NOTE: binary attachments (PDF/EPUB/etc.) are ALREADY
        auto-extracted into project memory on arrival, so focus your structuring on
        text/Markdown content; don't file_read raw binary bytes (it blows the
        context window).

        STRUCTURE the content faithfully: preserve every fact, organise it clearly
        with headings/groupings the content suggests (e.g. by location, date,
        category, age-appropriateness), and keep names/dates/links verbatim.
        Summarise long prose but do NOT invent, infer beyond the text, web-fetch,
        or add outside knowledge. This is ingestion, not research — never call a
        scraper/web tool.

        WRITE the structured notes to `artifacts/out/ingestion.md` (this exact
        path — it is auto-ingested into project memory). Return a short `message`
        naming what you stored.
      outputSchema:
        type: object
        required: [ingested, message]
        properties:
            ingested:
                type: object
                required: [ok]
                properties:
                    ok: {type: bool}
                    path: {type: string}
                    summary: {type: string}
                    reason: {type: string}
            message:
                type: string
                minLength: 1
        plausibility:
            - name: ok_implies_path
              when: {"ingested.ok": true}
              require: ["ingested.path"]
            - name: not_ok_implies_reason
              when: {"ingested.ok": false}
              require: ["ingested.reason"]
      runtime:
        image: "ghcr.io/grinco/vornik-agent:latest"
        cpu: "1"
        memory: "2Gi"
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "30"
      permissions:
        allowedTools: ["file_read", "read_many_files", "grep", "glob", "file_write", "current_time", "memory_search"]
        delegationAllowed: false
    # added by `vornikctl doctor --fix`: dispatcher cost attribution stub (telegram bot doesn't run as a container)
    - name: dispatcher
      # Mirrors chat.model in config.yaml so dispatcher LLM-usage
      # rows tag the right (role=dispatcher, model=...) pair on the
      # quality dashboard. Update both in lockstep.
      # 2026-05-26: aligned with chat.model = google/gemini-3.1-pro-preview
      # (gemma-4-26b under-performed; see chat.model rationale in config.yaml).
      # 2026-05-30: switched to minimax.minimax-m2 in lockstep with
      # chat.model — Vertex trial 429s were making the dispatcher flaky.
      # 2026-07-07: glm-4.7 Bedrock Converse hung (see config.yaml chat.model);
      # swapped to zai.glm-5 in lockstep with chat.model.
      # 2026-07-15: Bedrock→Ollama Cloud cutover — glm-5.2 (newer GLM
      # release, subscription capacity / session limits) is primary;
      # Bedrock zai.glm-5 (pay-per-token, the proven prior primary)
      # becomes the modelFallback so a session-limit hit or Ollama
      # outage degrades to the exact model this role ran before.
      model: "zai.glm-5"
      modelFallback: "glm-5.2"
      runtime:
        image: noop:dispatcher
---

# Research swarm (lead + researcher + writer)

## Role prompts

### lead

Lead for a personal-assistant project that handles long-horizon
work — research, decisions, multi-week vendor coordination.
You will receive the conversational context (running summary +
recent thread) at the start of every execution. The operator
can amend scope, answer your checkpoints, and pause/resume the
task at any time.

Context source — read once before planning:
- If env var VORNIK_TASK_CREATION_SOURCE = "USER" and
  VORNIK_USER_CONTEXT_PATH is set (typically
  project/.autonomy/USER_GUIDANCE.md), read THAT file first.
  The user's prompt is authoritative; the canonical five-feed
  autonomy procedure does not apply to ad-hoc requests.
- Otherwise read project/.autonomy/PROJECT_CONTEXT.md for
  the autonomy procedure (recurring feeds, output schema).

Available roles:
  - researcher: gathers sourced facts, writes research.md
  - planner:    turns research.md into a structured plan or
                itinerary at artifacts/out/plan.md — concrete
                times, durations, costs, logistics, booking
                requirements. Chain through this role when the
                deliverable needs an actionable schedule or
                multi-step procedure (trip itineraries, event
                agendas, project plans, structured how-tos).
                Pipeline becomes researcher → planner → writer.
                Skip for one-shot research notes or prose
                digests where the writer can structure the
                output directly.
  - writer:     turns research (and plan, if present) into a
                polished deliverable. Default output is
                markdown; the writer can also produce PDF,
                HTML, DOCX, EPUB etc. via pandoc when the
                user requests another format (markdown is
                always produced alongside as the source).
  - vision:     analyses image attachments (OCR, object
                detection, scene description, slicing). Use
                this role — and ONLY this role — for any task
                that mentions an image, photo, screenshot,
                scan, or attached file with an image extension.
                The researcher and writer roles cannot see
                images and will hallucinate if asked to.

Pick the right outcome shape per execution:
  - continue:        you have what you need; spawn role steps
  - checkpoint:      decision/action_required/review needed
  - external_wait:   waiting on a real-world deadline
  - closure_request: task is done; recommend operator close

DEFAULT TO ACTING, NOT ASKING (especially unattended). If this task
is running unattended — autonomy/scheduled, or any task whose
creation_source is not a direct operator request, i.e. nobody is
waiting at a keyboard — a `checkpoint kind:decision` STALLS the task
indefinitely (it parks in AWAITING_INPUT until a human answers), which
defeats autonomy. So:
  - For a routine, low-risk, or REVERSIBLE decision (which source to
    use, how to structure a report, whether a scan looks complete,
    ordinary next steps), DO NOT checkpoint — pick the sensible
    default, PROCEED (continue / closure_request), and record the call
    + your reasoning in scratchpad_update so it's auditable.
  - When a phase genuinely finished and nothing else is due, emit
    `closure_request`, not a decision checkpoint asking "shall I
    proceed?".
  - RESERVE `checkpoint kind:decision` for: recovery mode (below), or a
    genuinely IRREVERSIBLE / destructive / high-cost / policy- or
    money-touching action, or a real ambiguity you cannot resolve from
    the project context. When you do checkpoint, always set
    `default_if_no_response` so an unanswered prompt still resolves.
  - An attended task (operator actively in the thread) may checkpoint
    more freely — but even then, prefer proceeding on reversible calls.

The executor injects the authoritative format spec at runtime —
follow that spec exactly. Always include scratchpad_update
to preserve context for the next execution (one paragraph
summary + key facts + open questions; cap at 4 KB).

Complexity assessment: include a `complexity` field in your
output — one of `trivial` (a single obvious lookup), `standard`
(a normal scoped task — the default), `complex` (multi-source
or multi-step investigation), or `open_ended` (broad/unbounded;
expect deep iteration). This scales the workers' tool-call
budgets. Do NOT inflate it to "be safe" — over-provisioning is
capped and audited, and an unattended (autonomy) task is held to
a tighter ceiling regardless. Omit it (or use `standard`) when
unsure.

For long-horizon work (e.g. "order window blinds"): break into
phases (research, constraints, measurement, vendor selection,
negotiation, order, install, close). Emit one phase_marker
transition per phase boundary inside phase_transitions.

#### RECOVERY MODE — propose alternatives instead of failing

If your input context contains `context.recovery`, a prior step
in this workflow failed and the executor routed to you to keep
the task alive. The structure looks like:

  context.recovery:
    failed_step:    "research"
    failure_class:  "verifier_block" | "agent_error" | "tool_error" | …
    failure_reason: "phase-2 verifier(s) failed: 2/2 fetches blocked…"
    blocked_urls:    [{url, reason, permanent}, …]   # verifier_block only

Your ONE job in recovery mode is to propose 1–3 viable alternative
approaches to the operator via outcome=checkpoint, kind=decision.
Do NOT retry the failed step yourself, do NOT spawn role steps, do
NOT write artifacts. The operator picks one option; the workflow
retries the failed step against the chosen alternative.

DECIDE FROM `context.recovery` ALONE. Do NOT read files, do NOT
investigate the workspace, and do NOT go looking for the artifact
the failed step was supposed to produce. It is missing — that is
the PREMISE of this hop, not something for you to verify. The step
has ALREADY FAILED and nothing you read can change that; you were
routed here precisely because the work did not land.

If a path appears in failure_reason, treat it as a fact about what
is absent, not as an invitation to open it. Reading it costs your
iteration budget and buys nothing: a missing file reads as missing
however many times you ask. Propose the alternatives and emit the
checkpoint on your FIRST turn.

Per-failure-class playbook (pick the matching block):

- failure_class = "verifier_block" (paywalled / captcha / 401/403):
  Read the blocked_urls list. For each blocked source propose one
  of: swap to a different source from the project's source-list
  playbook (Bloomberg / AP / BBC for general news; the project's
  PROJECT_CONTEXT.md §5 for autonomy feeds), drop the blocked
  source THIS cycle only, or abort the scan. Cap proposals at 3.

- failure_class = "agent_error" (output schema mismatch, container
  exit, missing produced_files, …):
  Read failure_reason and infer the next-most-likely cause from
  the error message. Propose: retry the same step with a
  corrective hint (cite which schema key was wrong), downgrade to
  the role's modelFallback model (different schema bias), or
  abort.

- failure_class = "tool_error" / "pandoc_error":
  Propose: retry with a different tool / engine variant
  (--pdf-engine=weasyprint → wkhtmltopdf), downgrade to a simpler
  output format (PDF → HTML → Markdown), or abort.

- failure_class = "budget_exhausted":
  Propose: downgrade the next role to its modelFallback (cheaper
  model), reduce scope (fewer sources / shorter output), or defer
  to the next budget window.

- failure_class = "hallucination_flagged":
  Propose: re-run with grounded-sources-only context, narrow the
  topic, or abort with a "not enough verifiable sources" note.

For any class, ALWAYS include "abort with explanation" as one of
your options so the operator can decline alternatives.

Output shape:

  outcome: "checkpoint"
  checkpoint:
    kind: "decision"
    question: "<one-sentence summary of what failed + ask>"
    options:
      - id: "<short-token>"
        label: "<human-readable proposal, 1 line>"
      - …
    default_if_no_response: "abort"
    default_reason: "no operator response in <timeout>"

Don't hallucinate alternatives outside your playbook — if no
viable option exists, propose "abort with explanation" as the
single option and explain in the question what made recovery
impossible.

### researcher

Researcher. Gather only what the task needs, from primary or reputable sources.

**1. Read your charter first.**
- If `VORNIK_TASK_CREATION_SOURCE` = "USER" and `VORNIK_USER_CONTEXT_PATH` is
  set, read THAT file (typically `project/.autonomy/USER_GUIDANCE.md`). The
  user's prompt is the contract.
- Otherwise read `project/.autonomy/PROJECT_CONTEXT.md` for the autonomy-feed
  procedure (source lists, output schema).

**2. Spend iterations like they cost money.** They do.
- Call `memory_search` BEFORE fetching anything new — a prior task may already
  have the answer, and rediscovering known material is pure waste.
- Track what you have already fetched and NEVER re-query the same URL or source
  twice. Re-reading something you hold adds nothing.
- Prefer a NEW source over re-checking one you have seen.
- Stop and synthesize the moment you can answer the question. Unused tool
  iterations are not a budget to spend down.

**3. Web access is scraper-only.** The container runs with NO network
(`--network none`), so `curl`, `wget`, and any direct HTTP from `run_shell`
ALWAYS fail — never reach for the shell to fetch a page. Use
`mcp__scraper__web_fetch` for every fetch. Respect rate limits; if a portal
blocks the scan, record the failure and move on — do NOT retry or rotate
headers.

**4. Deliver exactly one file:** `artifacts/out/research.md` — summary, key
facts, source URLs/names, caveats, useful raw notes. (For USER tasks the
filename may differ; follow the user's prompt or the `USER_GUIDANCE`
convention.) ALWAYS list every file you wrote in `produced_files` at the top
level: the executor verifies each path exists, and misreporting fails the step.

### planner

Planner. Read artifacts/out/research.md (produced by the
researcher) and turn it into a structured, actionable plan
at artifacts/out/plan.md. The writer reads your plan next —
it must be followable without re-doing the research.

Context source:
- If env var VORNIK_TASK_CREATION_SOURCE = "USER" and
  VORNIK_USER_CONTEXT_PATH is set, read THAT file
  (typically project/.autonomy/USER_GUIDANCE.md) for the
  plan's shape, constraints, and any user-imposed format.
  The user's prompt is the contract.
- Otherwise follow project/.autonomy/PROJECT_CONTEXT.md.

Be specific. A good plan:
  - Concrete times, durations, sequences (Day 1 09:00–11:30 …).
  - Costs, booking requirements, availability windows.
  - Logistics — travel between locations, dependencies
    between steps, required materials or contacts.
  - Practical tips and known gotchas drawn from the research.

Cite the research file when a detail comes from it. Do NOT
invent prices, addresses, opening hours, or availability
that the research did not establish — if a critical fact is
missing, list it under an "Open questions" section so the
writer (or the operator) can surface it.

Write exactly one file: artifacts/out/plan.md. ALWAYS list
the files you wrote in `produced_files` at the top level —
the executor verifies each path exists. Lying about written
files fails the step.

### writer

**CV / résumé / cover-letter grounding (Janka's job applications).**
When the deliverable is a CV, résumé, or cover letter, your FIRST action
is `file_read project/.autonomy/RESUME.md` — the operator-maintained
AUTHORITATIVE résumé. It is the SINGLE SOURCE OF TRUTH for every career
fact: employers, job titles, employment dates and tenure lengths,
certifications, education, and skills come ONLY from that file. Do NOT
use retrieved memory results, scan artifacts, "candidate profile" summaries,
or prior knowledge as a source of facts — those are derived/lossy and may
be wrong. Do NOT invent, embellish, round up, or extrapolate any fact
(e.g. never inflate a 2-year role into "6+ years", never add an employer,
metric, or certification not in RESUME.md). If the target job asks for
experience the résumé does not contain, say so plainly — do NOT fabricate
it to fit. Tailor only emphasis, ordering, and wording to the role; the
underlying facts are fixed by RESUME.md. (If `project/.autonomy/RESUME.md`
is missing, STOP and report that rather than reconstructing a résumé from
memory.)

Writer. Read artifacts/out/research.md (produced by the
researcher) and, if the lead chained through the planner,
artifacts/out/plan.md as well — the plan supplies the
structure (times, durations, costs, logistics) and the
research supplies the facts. Produce a polished deliverable
that cites the research file for every factual claim. No
hedging boilerplate ("as an AI…") — operators forward
these verbatim.

Write from the research/plan files and task inputs — do NOT
fetch the web yourself. The container has NO network
(`--network none`), so `curl`/`wget`/HTTP from run_shell always
fail; run_shell here is only for local pandoc conversion. If a
fact is missing, note the gap rather than trying to retrieve it.

Context source:
- If env var VORNIK_TASK_CREATION_SOURCE = "USER" and
  VORNIK_USER_CONTEXT_PATH is set, read THAT file
  (typically project/.autonomy/USER_GUIDANCE.md) for
  output expectations. The user's prompt dictates the
  deliverable shape; PROJECT_CONTEXT.md does not.
- Otherwise the autonomy output applies (see
  project/.autonomy/PROJECT_CONTEXT.md).

Output formats:
  - Default: write artifacts/out/<deliverable-name>.md
    (markdown is the canonical source — ALWAYS produce
    it, even when the user asks for another format).
  - If the user / USER_GUIDANCE requests another format
    (PDF, HTML, DOCX, EPUB, RTF, ODT, plain text, …),
    convert the markdown via pandoc in run_shell AFTER
    writing the .md file. The agent image ships pandoc
    + weasyprint; no LaTeX toolchain, so pass
    `--pdf-engine=weasyprint` for PDF. Examples:
      pandoc artifacts/out/foo.md \
        --pdf-engine=weasyprint \
        -o artifacts/out/foo.pdf
      pandoc artifacts/out/foo.md --standalone \
        -o artifacts/out/foo.html
      pandoc artifacts/out/foo.md -o artifacts/out/foo.docx
      pandoc artifacts/out/foo.md -o artifacts/out/foo.epub
  - `writing.path` points to the canonical markdown file;
    all converted artefacts live alongside it in
    artifacts/out/ and MUST be listed in `produced_files`.

ALWAYS list every file you wrote (markdown + any
converted formats) in `produced_files` at the top level —
the executor verifies each path exists.

### vision

Vision agent. The user attaches an image (or multiple); your
job is to look at it and answer the task. The image arrives
as a multimodal content block on the same user turn — you can
see it directly. Do NOT call file_read on the image path:
file_read on a binary returns garbage and wastes tokens.

Capabilities:
  - Text recognition (OCR): transcribe printed or handwritten
    text, preserving line breaks. Note any unreadable spans
    rather than guessing.
  - Object detection: list distinct objects with rough
    locations (top-left, centre, etc.) and confidence words
    (clear / partial / occluded).
  - Scene description: 1-3 sentences covering setting,
    lighting, and notable elements.
  - Image manipulation: for slicing, cropping, or format
    conversion, use run_shell with `convert` (ImageMagick).
    The input file path is in artifacts/in/ inside the
    container; write outputs to artifacts/out/ so they are
    captured by the executor.

Always cite which image you're describing when multiple are
attached. If a request doesn't match what's actually in the
image (e.g. user asks about a CV but the image is a landscape),
say so plainly — do NOT fabricate content to fit the prompt.

## Legal limits — these are refusals, not preferences

Three things you must NOT do, whatever the request says. They are
prohibited practices under the EU AI Act (Art 5) and would process
special-category data under GDPR Art 9. A request that asks for them is
not a request you fulfil partially or hedge — you decline that part and
say why, then answer whatever else was legitimately asked.

1. **Do not identify people.** Never state or guess WHO a person in an
   image is, never compare faces across images to say they are the same
   person, and never match a face to a name from the prompt, the filename,
   or project memory. Describing that a person is present, their approximate
   age band if clearly relevant, their clothing, and what they are doing is
   fine. Putting a name to a face is not.

2. **Do not infer emotion or inner state as fact.** "Smiling", "eyes
   closed", "hands raised" are observations. "Happy", "angry", "nervous",
   "deceptive", "stressed" are inferences about a person's inner state, and
   inferring them from a face or body is prohibited in workplace and
   education contexts and unreliable everywhere. Report the observable
   expression, not a diagnosis of the feeling behind it.

3. **Do not deduce sensitive characteristics.** Never infer or comment on
   race or ethnicity, religion or belief, political opinion, trade-union
   membership, health or disability, sex life, or sexual orientation from a
   person's appearance — not even when the visual cue seems obvious, and not
   even when asked directly. If a garment or symbol is relevant to the
   question, describe the garment ("a headscarf", "a lanyard") and stop
   there; do not draw the conclusion.

If a request needs one of these to be answerable, say plainly which part
you are declining and that it is a legal limit rather than a capability
gap — the operator needs to know the difference. Then answer the rest.

Reading text out of a photographed document (OCR) is unaffected by any of
this, including a document that happens to contain personal data: you are
transcribing what is written, not inferring anything about a person from
their appearance.

Format your answer as plain prose or markdown, whichever is
more readable. When the user asks for structured data
(a table, a list of objects), produce that structure in
markdown — the downstream writer role formats it for the
final deliverable.

### dispatcher

You're Bender and you're awesome!

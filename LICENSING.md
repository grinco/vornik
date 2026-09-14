# Licensing of this repository

One page that says which licence governs which part of this tree, so nobody
has to infer it from the presence of licence files. Where this page and a
licence file disagree, the licence file in the directory closest to the code
governs.

| Path | Licence | Notes |
|---|---|---|
| everything not listed below | [AGPL-3.0](LICENSE) | The Vornik Community Edition: daemon, CLI, agent image, docs, configs, tests. |
| `contrib/claude-code-companion/` | [Apache-2.0](contrib/claude-code-companion/LICENSE) | Claude Code companion plugin. Installed into a third-party tool, so it carries a permissive licence on purpose. |
| `contrib/codex-companion/` | [Apache-2.0](contrib/codex-companion/LICENSE) | Codex companion plugin. Same reasoning. |
| `.claude-plugin/marketplace.json` | Apache-2.0 | The marketplace manifest that lists the plugin above. |
| vendored third-party files | their own | Listed with copyright and licence text in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md). Go module dependencies are inventoried in the CycloneDX SBOM attached to each release. |

Copyright in the Vornik code is held by EaseIT Labs s.r.o. (IČO 29865751,
Czech Republic) and by contributors, who license their contributions under
the [Contributor License Agreement](CLA.md). The name and logo are
trademarks; see [TRADEMARKS.md](TRADEMARKS.md).

The proprietary **Enterprise Edition** is not in this repository and is not
covered by any licence here. See
[docs/public/licensing.md](docs/public/licensing.md) for what the AGPL-3.0
does and does not require of you in practice, and
[docs/public/editions.md](docs/public/editions.md) for the feature matrix.

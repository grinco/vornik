# Releasing Vornik

This repo (`grinco/vornik-enterprise`) is the development source of truth.
A release fans out to three other repos:

| Target | Repo | Visibility | What lands |
|--------|------|-----------|------------|
| Docs site | `grinco/vornik-docs` (→ docs.vornik.io) | public | rendered HTML only |
| Community Edition | `grinco/vornik` | **public** | leak-scanned CE source export |
| Upstream parent | `grinco/vornik-ee` | private | a sync PR (never a direct push) |

Everything is published from **current `main`** — never a release tag's tree —
so the published content and the IP/leak gates stay in lockstep with `HEAD`.

## Automated path (GitHub Actions)

When a GitHub **release is published** on this repo, three workflows fire:

- `docs.yaml` → builds + publishes docs to `vornik-docs`.
- `publish-ce.yaml` → runs `scripts/export-public-ce.sh --publish` (leak-gated
  direct push to `grinco/vornik` main).
- `release-upstream-pr.yaml` → pushes a sync branch to `grinco/vornik-ee` and
  opens a same-repo PR for the operator to merge.

A tag push (`202x.*`) also triggers `release.yaml` (GoReleaser) to build
binaries/packages and attach them to the release **in the repo the tag was
pushed to**.

**Invariant — publishing runs only on this fork.** `docs.yaml`,
`publish-ce.yaml`, and `release-upstream-pr.yaml` are guarded with
`if: github.repository == 'grinco/vornik-enterprise'`. These workflow files
get mirrored into `grinco/vornik-ee` via the sync PR, but the parent must not
re-publish docs/CE cross-repo or open a sync PR to itself, so they no-op there.
GoReleaser has no hardcoded cross-repo target — it releases to whatever repo the
tag lives in.

**Prerequisites (repo secrets)** — required only for the automated path:
`DOCS_DEPLOY_TOKEN` (push to `vornik-docs`), `CE_PUBLISH_TOKEN` (push to
`grinco/vornik`), `UPSTREAM_SYNC_TOKEN` (Contents+PRs write on
`grinco/vornik-ee`), and `GPG_PRIVATE_KEY` for signed enterprise packages.

## Manual path (Actions disabled / minutes exhausted)

Actions is currently **disabled** on this repo (missing cross-repo tokens + to
conserve free-tier minutes). Publish by hand from a clean checkout on `main`.

Auth: `gh` logged in as an account with push rights to all three targets
(the operator uses `grinco`, which owns `grinco/vornik` + `grinco/vornik-ee`
and has org access to `vornik-docs`). No repo secrets needed — git uses gh's
credential helper.

```sh
# 0. Be on current main, clean.
git checkout main && git pull --ff-only

# 1. Docs → docs.vornik.io  (no-op if docs/public is unchanged)
scripts/publish-docs-manual.sh            # --dry-run to preview

# 2. CE source → grinco/vornik main  (leak-gated; aborts on any leak)
#    SKIP_TESTS=1: structural + leak gates still run and must pass; the full
#    `go test` gate needs Postgres and is this repo's own CI, not the publisher.
SKIP_TESTS=1 scripts/export-public-ce.sh --publish

# 3. Upstream sync PR → grinco/vornik-ee  (fork → parent; operator merges)
REL=<tag>                                 # e.g. 2026.7.0
git push --force upstream "HEAD:refs/heads/sync/release-${REL}"
gh pr create --repo grinco/vornik-ee --base main \
  --head "sync/release-${REL}" \
  --title "Sync vornik-enterprise -> upstream (release ${REL})" \
  --body "Syncs the release commits into grinco/vornik-ee:main."
```

### Tagging + GitHub Releases across the repos

All three EE-line repos use the **date scheme** (`2026.7.0`); tag them
consistently:

```sh
# Private repos (this repo + the parent): reuse the real release commit's tag.
git push upstream refs/tags/<tag>                       # grinco/vornik-ee
gh release create <tag> --repo grinco/vornik-ee --verify-tag \
  --title "<tag>" --notes-file <ee-notes.md>

# Public CE: tag the CE main tip. Body MUST be public-safe — do NOT reuse the
# EE release notes (they carry EE-only detail). Point at docs.vornik.io.
gh release create <tag> --repo grinco/vornik --target <ce-main-sha> \
  --title "<tag>" --notes-file <ce-safe-notes.md>
```

## Release notes

- Internal, full notes: `docs/release-notes/<tag>.md` (EE-inclusive).
- Public, curated notes: a `## <tag>` section in
  `docs/public/release-notes/index.md` — **public-safe only** (no operator
  tokens, internal paths, leak-scan mechanics, or private trading specifics).
  This file ships to the public CE repo and to docs.vornik.io.

## Gotchas

- **Publish from `main` HEAD**, not the release tag — the gates are
  version-coupled to the checked-out tree.
- **A PR to `grinco/vornik` can never be deleted** — that is why CE syncs push
  straight to `main` (force-overwritable history) behind the leak scan, rather
  than opening a PR. A new operator token goes in `scripts/docs-ip-denylist.txt`
  only — never hardcode it anywhere shipped.
- **`grinco/vornik-ee` Actions are separate** from this org's. Creating a
  release / pushing a tag there triggers its mirrored workflows; the fork guards
  above keep the publish/sync ones inert, but confirm no unexpected runs.

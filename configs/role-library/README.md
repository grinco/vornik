# Role library

This directory is the seeded, curated **parts bin** the NL automation
composer draws from when it synthesizes a bundle (see
`https://docs.vornik.io` §5.3, §5.4,
§6). Every file here is a `RoleArchetype` (`internal/rolelibrary`):
YAML frontmatter plus a prompt body.

## Security model

- **Each archetype's `tools` list is the immutable-by-policy outer
  boundary of every composed automation.** The composer can select
  which archetypes to use and fill in `promptParams`, but it cannot
  grant a composed role any tool beyond what its archetype already
  lists. Least-privilege here is not a suggestion — it is the ceiling.
- **The library doctor check (`internal/rolelibrary.CheckLibrary`,
  wired at `POST /api/v1/doctor` / `make lint` / `vornikctl doctor` via
  `internal/api/doctor_role_library.go`) flags allowlist growth.** Any
  archetype whose `tools` list contains a wildcard entry, `run_shell`,
  or more than 8 entries produces a `SeverityFlag` finding — loud,
  review-worthy, not fatal on its own, but it turns the whole check
  `WARNING` instead of `OK`.
- **`run_shell` (arbitrary command execution) and other broad grants
  are excluded from this library by default.** Roles that genuinely
  need a shell are template territory — a hand-reviewed, versioned
  workflow template — not something a prose-generated composition
  should be able to reach for. None of the seeded archetypes grant
  `run_shell`; `coder` and `tester` instead carry the narrow built-ins
  (`file_read`/`file_write`/`file_edit`, `grep`/`glob`,
  `git_status`/`git_diff`/`git_log` for coder;
  `test_run`/`lint_run`/`typecheck_run`/`file_read`/`grep`/`glob` for
  tester) that cover their real need without a general-purpose shell.
- **Adding `run_shell`, a wildcard, or otherwise broadening any seeded
  archetype's `tools` list is a security-review-worthy change.** Treat
  a PR that does so the same as a permissions change, not a routine
  edit — it widens the outer boundary for every future composed
  automation, not just the one you're thinking about.

The seeded library is expected to produce **zero** findings (no
`ERROR`, no `FLAG`/`WARNING`) from `CheckLibrary` — see
`internal/rolelibrary.TestSeededArchetypesPass`. If your change makes
that test fail, that is the signal to reconsider the change rather
than to loosen the test.

## Adding or editing an archetype

1. Keep the `tools` list to the narrowest built-ins
   (`internal/agenttools`), system-handler names, or `mcp__server__tool`
   references the role's actual body of work requires.
2. If a tool feels indispensable but isn't least-privilege on its
   face (e.g. `file_write` on a role that only ever creates new
   files), add a one-line rationale note in the archetype's prompt
   body so the next reader can tell "deliberate" from "oversight".
3. Run `go test ./internal/rolelibrary/... ./internal/api/...` and the
   `role_library` doctor check before committing — the seeded library
   must stay at zero findings.

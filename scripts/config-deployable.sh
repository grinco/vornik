#!/usr/bin/env bash
# config-deployable.sh — the ONE source of truth for which repo config subtrees
# deploy, which are host-local, which are ignored, and which are operator-tunable.
#
# Sourced by (never re-derived elsewhere — that duplication is the bug this file
# kills; LLD 2026-07-16-config-tree-drift-coverage-design.md):
#   - Makefile `install-config-assets`  (what to copy into the deployed tree)
#   - scripts/config-drift-check.sh     (what to compare repo <-> deployed)
#   - scripts/config-manifest-lint.sh   (every repo configs/ dir must be classified)
#
# Adding a new config subtree = ONE edit here. Forgetting to ship or check it
# (the workflows-2026-05-27 and role-library-2026-07-16 incidents) becomes
# impossible: the installer copies it, the drift-check covers it, and the
# completeness lint fails CI until it is classified.
#
# This file is data only; it must be `source`-able with no side effects.

# Directories shipped from repo configs/ into the deployed tree. config-drift-check.sh
# surfaces ALL divergence here as exit 1 (no severity tier today — the [tunable]/
# [canonical] label it prints is cosmetic). The tunable/canonical split
# (CONFIG_TUNABLE_DIRS) governs the deploy-time SELF-CHECK in config-deploy.sh,
# not drift-check severity. (A STRICT_CONFIG_DEPLOY-driven drift-severity tier is
# a tracked follow-up, not yet built.)
CONFIG_DEPLOYABLE_DIRS=(swarms workflows role-library project-templates)

# Top-level files shipped from repo configs/ (copied singly, not per-dir).
CONFIG_DEPLOYABLE_FILES=(pricing.yaml)

# Present in both trees but per-operator/host-specific: never force-synced. The
# drift-check reports divergence at INFO and never sets the drift exit code.
CONFIG_HOSTLOCAL_DIRS=(projects)

# Never deployed. Non-runtime artifact DIRECTORIES. The completeness lint
# (config-manifest-lint.sh) enforces this as a CLOSED set: every entry here must
# also appear in the lint's hard-coded KNOWN_NON_RUNTIME_IGNORES allowlist, so
# silencing a real subtree — e.g. role-library — by dumping it here does NOT
# work: it requires a second, reviewed edit to the lint itself. (A code-grep
# "is this referenced by runtime code" check was tried and rejected — subtrees
# are bare quoted literals, so it false-positives on generic names.)
CONFIG_IGNORED=(evals examples)

# Top-level repo configs/ files that are never deployed (examples/templates).
# Matched as globs by the completeness lint so they don't read as unclassified.
CONFIG_IGNORED_GLOBS=('*.example' '*.dev.yaml')

# Operator-tunable deployables. Used ONLY by config-deploy.sh's post-copy
# self-check: tunable dirs get the looser any-file presence check (a partial I/O
# failure on a non-daemon-critical dir won't abort), while canonical dirs NOT
# listed here (role-library, pricing.yaml) get per-file completeness and abort
# under STRICT_CONFIG_DEPLOY if a file fails to land. This does NOT affect
# config-drift-check.sh, which surfaces all drift as exit 1 regardless of tier.
CONFIG_TUNABLE_DIRS=(swarms workflows project-templates)

# config_is_tunable <name> -> 0 if the dir is operator-tunable, 1 otherwise.
config_is_tunable() {
	local name="$1" t
	for t in "${CONFIG_TUNABLE_DIRS[@]}"; do
		[ "$t" = "$name" ] && return 0
	done
	return 1
}

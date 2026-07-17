#!/usr/bin/env bash
# backport-ce-pr_test.sh — regression test for the PR-range bounding in
# backport-ce-pr.sh.
#
# Incident (2026-07-17): the helper used `git format-patch -1 FETCH_HEAD`, which
# captures ONLY the tip commit. grinco/vornik PR #5 carried two commits; the
# helper grabbed just the fixup commit and `git am` conflicted on the missing
# base. The fix bounds the PR to merge-base(head, branch)..head and patches the
# full range. This test builds a synthetic 2-commit PR (with the CE branch
# advanced past the fork point, to prove merge-base — not a fixed offset — finds
# the base) and asserts BOTH commits are captured and the export path rewrite is
# still reversed.
set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }

WORK="$(mktemp -d /tmp/backport-test.XXXXXX)"
trap 'rm -rf "$WORK"' EXIT
cd "$WORK"

git -c init.defaultBranch=main init -q
git config user.email t@example.com
git config user.name Tester
git config commit.gpgsign false

# main: base commit, then a later commit AFTER the PR forks (simulates CE main
# moving on while the contributor's PR branch stays at the older base).
echo base > README.md
git add README.md
git commit -qm "base"
FORK="$(git rev-parse HEAD)"

# PR branch off FORK with TWO commits — the second depends on the first, so
# tip-only patching would fail to apply.
git checkout -q -b pr "$FORK"
printf 'package p\nimport _ "github.com/grinco/vornik/internal/x"\n' > a.go
git add a.go
git commit -qm "PR commit 1: add feature"
printf 'package p\nimport _ "github.com/grinco/vornik/internal/x"\n// tweak\n' > a.go
git add a.go
git commit -qm "PR commit 2: fixup"
PR_HEAD="$(git rev-parse HEAD)"

# Advance main past the fork point.
git checkout -q main
echo more >> README.md
git commit -qam "unrelated main commit"

# --- The logic under test (mirrors backport-ce-pr.sh steps 1-2) ---
BASE="$(git merge-base "$PR_HEAD" main)"
NCOMMITS="$(git rev-list --count --no-merges "$BASE..$PR_HEAD")"

[ "$BASE" = "$FORK" ] || fail "merge-base should be the fork point ($FORK), got $BASE"
[ "$NCOMMITS" = "2" ] || fail "expected 2 PR commits, got $NCOMMITS"

PATCH="$WORK/pr.mbox"
git format-patch "$BASE..$PR_HEAD" --stdout > "$PATCH"
LC_ALL=C sed -i \
  -e 's#github\.com/grinco/vornik#github.com/grinco/vornik#g' \
  "$PATCH"

grep -q "PR commit 1: add feature" "$PATCH" || fail "base commit missing from patch (the -1 bug)"
grep -q "PR commit 2: fixup" "$PATCH"       || fail "tip commit missing from patch"
grep -q "github.com/grinco/vornik/internal/x" "$PATCH" \
  || fail "export path rewrite was not reversed"
grep -q "grinco/vornik" "$PATCH" && fail "unreversed grinco path leaked into patch"

# The full range must apply cleanly onto the (advanced) main via 3-way.
git checkout -q -b applied main
git am -3 "$PATCH" >/dev/null 2>&1 || fail "git am of the full range failed to apply"
[ "$(git rev-list --count main..HEAD)" = "2" ] || fail "expected 2 commits applied onto main"

echo "PASS: backport-ce-pr range logic captures all PR commits + reverses paths"

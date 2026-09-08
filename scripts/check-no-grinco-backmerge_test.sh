#!/usr/bin/env bash
# Self-test for check-no-grinco-backmerge.sh --trunk.
#
# The incident (2026-09-08): the trunk scan reported all seven acknowledged
# back-merges as unacknowledged, blocking every commit in the tree. Nothing
# about the merges had changed. The scan prints git's AUTO-ABBREVIATED %h,
# whose width grows with the repository's object count, and then requires an
# exact match against the prefixes recorded in the ack file. Those were written
# at 8 characters; the repo had since grown enough for %h to widen to 9, so
# "^${sha}[[:space:]]" could no longer match any of them.
#
# The bug is comparing a variable-width abbreviation against a fixed-width
# record. It would have recurred at 10 characters, and it fails CLOSED — the
# gate blocks all work rather than letting a back-merge through — which is why
# it surfaced as a mystery rather than as a leak.
set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="$repo_root/scripts/check-no-grinco-backmerge.sh"
failures=0

# fixture builds a throwaway repo whose trunk carries one back-merge-shaped
# commit, with an ack file recording it at the given prefix length.
fixture() {
  local prefix_len="$1" dir
  export script
  dir="$(mktemp -d)"
  (
    cd "$dir" || exit 1
    git init -q -b main .
    git config user.email t@example.com
    git config user.name test
    git commit -q --allow-empty -m "base"
    git commit -q --allow-empty -m "Merge branch 'grinco:main' into main"
    mkdir -p scripts
    # The script cds to its OWN repo root, so a fixture must host a copy or
    # the scan reads this repository instead of the one under test.
    cp "$script" scripts/
    local sha
    sha="$(git rev-parse HEAD | cut -c "1-${prefix_len}")"
    printf '# ack\n%s kept deliberately\n' "$sha" >scripts/grinco-backmerge-acknowledged.txt
  ) || return 1
  printf '%s' "$dir"
}

run_case() {
  local name="$1" prefix_len="$2" want="$3" dir out rc
  dir="$(fixture "$prefix_len")" || { echo "FAIL: $name (fixture)"; failures=$((failures + 1)); return; }
  out="$(cd "$dir" && TRUNK_REF=main bash scripts/check-no-grinco-backmerge.sh --trunk 2>&1)"
  rc=$?
  if [ "$rc" != "$want" ]; then
    echo "FAIL: $name — exit $rc, want $want"
    printf '%s\n' "$out" | sed 's/^/    /'
    failures=$((failures + 1))
  else
    echo "ok   - $name"
  fi
  rm -rf "$dir"
}

# The regression. A 6-character ack entry is SHORTER than any abbreviation git
# will print, which is exactly the shape that broke: the recorded prefix
# identifies the commit unambiguously, and the gate must honour it.
run_case "a shorter ack prefix than git's abbreviation is still acknowledged" 6 0

# The full sha, the other end of the range.
run_case "a full 40-character ack entry is acknowledged" 40 0

# And the gate must still FAIL on a back-merge nobody recorded: a fix that
# accepted everything would pass the two cases above and defeat the control.
dir="$(mktemp -d)"
(
  cd "$dir" || exit 1
  git init -q -b main .
  git config user.email t@example.com
  git config user.name test
  git commit -q --allow-empty -m "base"
  git commit -q --allow-empty -m "Merge branch 'grinco:main' into main"
  mkdir -p scripts
  cp "$script" scripts/
  printf '# ack, deliberately empty\n' >scripts/grinco-backmerge-acknowledged.txt
)
if (cd "$dir" && TRUNK_REF=main bash scripts/check-no-grinco-backmerge.sh --trunk >/dev/null 2>&1); then
  echo "FAIL: an unrecorded back-merge must still fail the gate"
  failures=$((failures + 1))
else
  echo "ok   - an unrecorded back-merge still fails the gate"
fi
rm -rf "$dir"

# A prefix that matches no commit must not silently acknowledge one either.
dir="$(mktemp -d)"
(
  cd "$dir" || exit 1
  git init -q -b main .
  git config user.email t@example.com
  git config user.name test
  git commit -q --allow-empty -m "base"
  git commit -q --allow-empty -m "Merge branch 'grinco:main' into main"
  mkdir -p scripts
  cp "$script" scripts/
  printf '# ack\ndeadbeef some other merge entirely\n' >scripts/grinco-backmerge-acknowledged.txt
)
if (cd "$dir" && TRUNK_REF=main bash scripts/check-no-grinco-backmerge.sh --trunk >/dev/null 2>&1); then
  echo "FAIL: an ack entry for a DIFFERENT commit must not acknowledge this one"
  failures=$((failures + 1))
else
  echo "ok   - an ack entry for a different commit acknowledges nothing"
fi
rm -rf "$dir"

if [ "$failures" -ne 0 ]; then
  echo "FAILED: $failures case(s)"
  exit 1
fi
echo "all cases passed"

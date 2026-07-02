#!/usr/bin/env bash
#
# affected-go-packages.sh — print the Go import paths affected by the current
# branch's changes, for the branch-scoped CI unit-test lane.
#
# "Affected" = the packages that contain changed .go files, PLUS every package
# whose transitive dependency set includes one of them. That dependent-closure
# is the safety property: a change deep in a widely-imported package (e.g.
# internal/persistence) expands to all of its importers, so a branch can't go
# green while a reverse dependency it broke goes untested. (This is the
# verify-at-CI-scope lesson — narrow "changed dirs only" testing is what let a
# past PR break the build.)
#
# Prints "./..." (test everything) when:
#   - go.mod / go.sum changed (a dependency bump can affect any package), or
#   - the base ref can't be resolved (fail safe).
# Prints nothing when no .go files changed relative to the base.
#
# Usage: affected-go-packages.sh [base-ref]
#   base-ref defaults to origin/$GITHUB_BASE_REF (PRs) then origin/main.
set -euo pipefail

base="${1:-${GITHUB_BASE_REF:+origin/$GITHUB_BASE_REF}}"
base="${base:-origin/main}"

if ! git rev-parse --verify --quiet "$base" >/dev/null 2>&1; then
	echo "./..."
	exit 0
fi

changed="$(git diff --name-only "$base"...HEAD -- '*.go' 'go.mod' 'go.sum' || true)"
[ -n "$changed" ] || exit 0

# A module-graph change can affect anything — test the whole tree.
if printf '%s\n' "$changed" | grep -qxE 'go\.(mod|sum)'; then
	echo "./..."
	exit 0
fi

module="$(go list -m)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# Map each changed .go file to its package import path (module + dir).
printf '%s\n' "$changed" | grep -E '\.go$' | while IFS= read -r f; do
	d="$(dirname "$f")"
	if [ "$d" = "." ]; then echo "$module"; else echo "$module/$d"; fi
done | sort -u >"$tmp/changed.txt"
[ -s "$tmp/changed.txt" ] || exit 0

# One line per package: "<importpath> <transitive-dep> <transitive-dep> ...".
if ! go list -f '{{.ImportPath}}{{range .Deps}} {{.}}{{end}}' ./... >"$tmp/deps.txt" 2>/dev/null; then
	echo "./..."
	exit 0
fi

# Keep only changed paths that are real, current packages (drop deleted or
# non-package dirs so `go test` never sees a bogus import path).
awk '{print $1}' "$tmp/deps.txt" | sort -u >"$tmp/all.txt"
comm -12 "$tmp/changed.txt" "$tmp/all.txt" >"$tmp/seed.txt"
[ -s "$tmp/seed.txt" ] || exit 0

# Emit the seed packages plus any package that transitively depends on one.
{
	cat "$tmp/seed.txt"
	awk 'NR==FNR{c[$1]=1;next}{for(i=2;i<=NF;i++)if($i in c){print $1;break}}' \
		"$tmp/seed.txt" "$tmp/deps.txt"
} | sort -u

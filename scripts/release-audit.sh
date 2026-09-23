#!/usr/bin/env bash
#
# release-audit.sh — for the last N EE releases, did the publication actually
# produce the artifacts the release promised?
#
# Gaps 5 and 6 of enterprise-packaging-design.md's 2026-09-22 amendment. Both
# are OUTCOMES that the 2026-09-06 and 2026-09-22 work mechanised the TRIGGER
# for and left unverified:
#
#   Gap 5 — a CE tag fires publish-agent-image, but nothing asserts the image
#           :<tag> ever reached the registry. A build failure, a registry push
#           failure or an exhausted quota leaves no versioned image and the
#           release is still declared complete.
#   Gap 6 — release-upstream-pr pushes a branch and (since 2026-09-22) a tag,
#           but a HUMAN merges the PR. The mirror's main was current only
#           because the same person merged #76, #77 and #78 — the "survived
#           because the same person did it each time" pattern this design has
#           already named once.
#
# Usage: scripts/release-audit.sh [count]
set -uo pipefail

COUNT="${1:-5}"
EE="${RELEASE_AUDIT_EE_REPO:-grinco/vornik-enterprise}"
CE="${RELEASE_AUDIT_CE_REPO:-grinco/vornik}"
MIRROR="${RELEASE_AUDIT_MIRROR_REPO:-grinco/vornik-ee}"
IMAGE="${RELEASE_AUDIT_IMAGE:-ghcr.io/grinco/vornik-agent}"

problems=0
# The `return 0` is not decoration. Without it the function's status is the
# `[ -n ... ]` test, which is FALSE whenever GITHUB_STEP_SUMMARY is unset — so
# the last note in the success path made the whole script exit 1 while printing
# "All audited releases produced what they promised". A control that reports
# success and exits failure is the tenet-4 failure in its purest form, and this
# one was in the script written to catch that class.
note() {
	printf '%s\n' "$1"
	[ -n "${GITHUB_STEP_SUMMARY:-}" ] && printf '%s\n' "$1" >> "$GITHUB_STEP_SUMMARY"
	return 0
}
bad()  { note "- **$1**"; problems=$((problems + 1)); }

tags=$(gh release list -R "$EE" -L "$COUNT" 2>/dev/null | cut -f1)
[ -n "$tags" ] || { echo "release-audit: no releases found on $EE"; exit 1; }

note "## Release audit — last $COUNT releases of $EE"

for tag in $tags; do
	note ""
	note "### $tag"

	# --- Gap 5: the versioned agent image ---------------------------------
	#
	# ANONYMOUS. ghcr.io/grinco/vornik-agent is public, so reading its
	# manifest needs no token and no new secret — the same reasoning
	# wait-ce-ci.sh already uses for the public CE run.
	#
	# A MISS IS NOT IMMEDIATELY A FAILURE. GHCR is eventually consistent and
	# a freshly pushed manifest can be unresolvable for tens of seconds. But
	# EXHAUSTING the attempts IS a failure — a "poll until it resolves" with
	# no terminal state turns a missing image into a slow green, which is the
	# whole defect class this audit exists for.
	found=0
	for attempt in 1 2 3; do
		if token=$(curl -fsS "https://ghcr.io/token?scope=repository:${IMAGE#ghcr.io/}:pull&service=ghcr.io" 2>/dev/null | \
		           python3 -c 'import json,sys;print(json.load(sys.stdin).get("token",""))' 2>/dev/null) && [ -n "$token" ]; then
			if curl -fsS -o /dev/null -H "Authorization: Bearer $token" \
			   -H 'Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json' \
			   "https://ghcr.io/v2/${IMAGE#ghcr.io/}/manifests/$tag" 2>/dev/null; then
				found=1; break
			fi
		fi
		[ "$attempt" -lt 3 ] && sleep 10
	done
	if [ "$found" -eq 1 ]; then
		note "- agent image \`$IMAGE:$tag\` resolves"
	else
		bad "NO agent image \`$IMAGE:$tag\` — the CE tag fires publish-agent-image, but nothing produced a versioned image for this release"
	fi

	# --- Gap 6: did the source reach the mirror? --------------------------
	#
	# WHICH COMMIT: the EE release tag's commit, asserted to be an ancestor
	# of the mirror's default branch. Ancestry is TRANSITIVE and that is the
	# point — a sync branch is cut from main, so a later sync carries every
	# earlier commit. A release commit that no merged sync ever carried is
	# exactly the case that should report, and the only one that does.
	sha=$(git rev-parse "$tag^{commit}" 2>/dev/null)
	if [ -z "$sha" ]; then
		bad "$tag has no local commit — cannot check whether it reached $MIRROR"
	else
		if ! git ls-remote --exit-code --tags "https://github.com/$MIRROR" "refs/tags/$tag" >/dev/null 2>&1; then
			bad "$MIRROR carries no tag \`$tag\`"
		fi
		if gh api "repos/$MIRROR/compare/main...$sha" -q .status 2>/dev/null | grep -qE 'identical|behind'; then
			note "- release commit \`${sha:0:9}\` is on \`$MIRROR\` main"
		else
			bad "release commit \`${sha:0:9}\` is NOT on \`$MIRROR\` main — the sync PR was never merged. Merge it, or close it unmerged AND delete the mirror tag (a mirror tag for a rejected sync names content the mirror refused; the never-move rule protects PUBLISHED tags, and that one was advertised to nobody)"
		fi
	fi

	# --- the CE release object -------------------------------------------
	if gh release view "$tag" -R "$CE" >/dev/null 2>&1; then
		note "- $CE has a release for \`$tag\`"
	else
		bad "$CE has a TAG but no RELEASE for \`$tag\`"
	fi
done

note ""
if [ "$problems" -gt 0 ]; then
	note "**$problems problem(s).** Each names its resolution above."
	exit 1
fi
note "**All audited releases produced what they promised.**"
exit 0

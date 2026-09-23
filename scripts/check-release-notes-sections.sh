#!/usr/bin/env bash
#
# check-release-notes-sections.sh — every EE release-notes file must have a
# matching curated section in the PUBLIC notes index, extractable by the same
# rule the CE release workflow uses.
#
# WHY THIS EXISTS. The CE release workflow (scripts/public-ce-templates/
# publish-release.yml) builds a release body by extracting "## <tag>" from
# docs/public/release-notes/index.md, and fails closed when the section is
# absent. Fail-closed is right, but at release time the FIRST release after a
# format drift is the canary — and that is exactly what happened to 2026.8.4:
# the CE tag was pushed before the public prose for it was written, so the
# section did not exist and the release was silently never created. Nobody
# noticed for five weeks.
#
# So the same check runs here, in EE CI, at PR time: the drift is caught where
# it is introduced rather than in a release run that has already published a
# draft. This reads the ENTERPRISE tree's copy; the workflow reads the exported
# one. Two files in two repositories, so they can still disagree — which is why
# the workflow's failure names the file's size and hash. This check makes that
# disagreement rare, not impossible.
#
# Usage: scripts/check-release-notes-sections.sh [notes-dir] [public-index]
set -euo pipefail

NOTES_DIR="${1:-docs/release-notes}"
INDEX="${2:-docs/public/release-notes/index.md}"

[ -d "$NOTES_DIR" ] || { echo "check-release-notes-sections: no $NOTES_DIR; nothing to check"; exit 0; }
[ -f "$INDEX" ] || { echo "::error::$INDEX is missing — the CE release workflow has no body to publish from"; exit 1; }

# The extractor the CE workflow uses, kept identical on purpose: anchored on
# the tag TOKEN with a boundary so a titled heading still matches, stopping at
# the next top-level "## ".
section_lines() {
	awk -v tag="$1" '
		$0 ~ "^## " tag "([^0-9.]|$)" { f=1; next }
		/^## / { f=0 }
		f { print }
	' "$INDEX" | grep -c '[^[:space:]]' || true
}

# THE FLOOR, and why there is one. Releases older than the public repository's
# own history are deliberately collapsed into a single "## Earlier releases"
# rollup rather than given a section each — an editorial choice, not a gap, and
# those versions were never published to grinco/vornik at all. Requiring a
# section for them would make this check fire permanently on history nobody is
# going to rewrite.
#
# The floor is DERIVED from the index rather than hardcoded, so curating an
# older release into its own section lowers it automatically and nobody has to
# remember to edit a constant here.
FLOOR="$(grep -oE '^## 20[0-9]{2}\.[0-9]+\.[0-9]+' "$INDEX" | sed 's/^## //' | sort -V | head -1)"
[ -n "$FLOOR" ] || { echo "::error::$INDEX has no release sections at all"; exit 1; }

older_than_floor() {
	[ "$1" = "$FLOOR" ] && return 1
	[ "$(printf '%s\n%s\n' "$1" "$FLOOR" | sort -V | head -1)" = "$1" ]
}

missing=0
checked=0
skipped=0
for f in "$NOTES_DIR"/*.md; do
	[ -e "$f" ] || continue
	tag="$(basename "$f" .md)"
	case "$tag" in
		20[0-9][0-9].*) ;;
		*) continue ;;
	esac
	if older_than_floor "$tag"; then
		skipped=$((skipped + 1))
		continue
	fi
	checked=$((checked + 1))
	if [ "$(section_lines "$tag")" -eq 0 ]; then
		echo "::error::no '## $tag' section in $INDEX — the CE release for $tag would refuse to publish"
		missing=$((missing + 1))
	fi
done

# A nested "##" inside a section truncates the extracted body silently, so the
# convention forbids it (subsections are "###"). Linted rather than trusted:
# the failure is invisible in the published release, which just looks short.
# "## Earlier releases (…)" is the rollup the floor above refers to and is an
# intended top-level heading, not a nested one — it ends the last real section
# rather than truncating it. Anything ELSE at this level does truncate.
ALLOWED='^[0-9]+:## (20[0-9][0-9]\.|Release Notes|Earlier releases)'
if grep -nE '^## ' "$INDEX" | grep -qvE "$ALLOWED"; then
	echo "::error::$INDEX has a '## ' heading that is not a release section:"
	grep -nE '^## ' "$INDEX" | grep -vE "$ALLOWED" | sed 's/^/    /'
	echo "    A nested '## ' truncates the extracted release body. Use '### ' for subsections."
	missing=$((missing + 1))
fi

if [ "$missing" -gt 0 ]; then
	echo "check-release-notes-sections: $missing problem(s) across $checked release(s)"
	exit 1
fi
echo "check-release-notes-sections: OK — $checked release(s) have an extractable public section (floor $FLOOR; $skipped older, covered by the rollup)"

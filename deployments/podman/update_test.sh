#!/usr/bin/env bash
# update_test.sh — vornik-update.sh must survive checking out a new version of
# ITSELF.
#
# THE INCIDENT (2026-09-06). Step 2 of the updater runs
#
#     git -C "$REPO_DIR" checkout --quiet "$TARGET_REF"
#
# and the script being executed lives inside $REPO_DIR, so that line rewrites
# the file bash is reading. Bash reads a script lazily and seeks back to a
# saved byte offset after each command; once the file underneath changes, it
# resumes at that offset inside the NEW file and executes whatever byte lands
# there. Every CE upgrade whose vornik-update.sh changed size died there:
#
#     vornik-update.sh: line 200: F: command not found      (exit 127)
#
# 2026.8.x (13023 B) -> 2026.9.x (17612 B) and 2026.9.1 -> 2026.9.2 (20487 B)
# both did. Nothing after the checkout ran — no binary build, no image
# rebuild, no sidecar recreate, no cutover. And because the checkout HAD
# already moved, the operator's retry printed "Checkout already at target
# commit. Nothing to do." and exited 0: a wholly un-updated install reporting
# success, which is worse than the half-applied state contract C3 forbids.
#
# WHY NO EXISTING TEST CAUGHT IT. Every check in update_test.go is a
# strings.Contains over the script TEXT. TestUpdaterBuildsImagesBeforeCutover
# proves rebuild_images appears before the install line in the file; it never
# runs the file, so a defect that stops execution before reaching that code is
# invisible to all nine of them. This suite EXECUTES the script — that is the
# whole point of it existing alongside the Go one.
#
# Run: bash deployments/podman/update_test.sh
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UPDATER="$HERE/vornik-update.sh"
[ -f "$UPDATER" ] || { echo "FAIL: $UPDATER not found"; exit 1; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/vornik-update-test.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

# ---------------------------------------------------------------------------
# The two script versions. "old" is the real shipped script; "new" models the
# next release by inserting a comment block near the top, which is what shifts
# every byte offset below it. Simulating the release this way rather than
# pinning two git tags keeps the test hermetic and keeps it meaningful after
# the tags roll.
# ---------------------------------------------------------------------------
cp "$UPDATER" "$WORK/old.sh"
awk 'NR==2 { for (i = 0; i < 40; i++) print "# next release adds a line here, and every offset below it moves." } { print }' \
  "$UPDATER" > "$WORK/new.sh"
if [ "$(wc -c < "$WORK/old.sh")" -eq "$(wc -c < "$WORK/new.sh")" ]; then
  echo "FAIL: the fixture did not change the script size; the test would prove nothing"
  exit 1
fi

# ---------------------------------------------------------------------------
# A fake CE install: a checkout, a config, a bin dir, and stubs on PATH for
# everything the updater shells out to. The git stub does the one thing that
# matters — its `checkout` swaps old.sh for new.sh under the running script,
# exactly as the real one does.
# ---------------------------------------------------------------------------
REPO="$WORK/repo"
mkdir -p "$REPO/.git" "$REPO/deployments/podman" "$REPO/.bin" "$WORK/bin" "$WORK/cfg" "$WORK/localbin" "$WORK/home"
cp "$WORK/old.sh" "$REPO/deployments/podman/vornik-update.sh"
chmod +x "$REPO/deployments/podman/vornik-update.sh"
printf 'listen: ":8080"\n' > "$WORK/cfg/config.yaml"
: > "$REPO/.bin/vornik"; : > "$REPO/.bin/vornikctl"
chmod +x "$REPO/.bin/vornik" "$REPO/.bin/vornikctl"

# The manifest emitter the updater builds in step 2. One always-on row, with
# the "-" placeholder in the target column (C8).
ROW='ghcr.io/grinco/vornik-agent:latest\timages/vornik-agent/Containerfile\t-\t.\talways\n'
cat > "$REPO/.bin/vornik-images" <<EMIT
#!/usr/bin/env bash
# Stands in for the real emitter. With -obtain it prints only the rows that
# still need a LOCAL BUILD, which is the contract the updater relies on.
# OBTAIN_MODE=nothing models the obtain step having pulled everything.
if [ "\${1:-}" = -obtain ] && [ "\${OBTAIN_MODE:-}" = nothing ]; then
  exit 0
fi
printf '$ROW'
EMIT
chmod +x "$REPO/.bin/vornik-images"

BUILD_LOG="$WORK/podman-build.log"
: > "$BUILD_LOG"

cat > "$WORK/bin/git" <<GIT
#!/usr/bin/env bash
case "\$*" in
  *"checkout --quiet"*)
      # THE DEFECT UNDER TEST: checking out the target rewrites the script
      # that is running right now.
      cp "$WORK/new.sh" "$REPO/deployments/podman/vornik-update.sh"
      touch "$WORK/.moved" ;;
  *"rev-parse --short HEAD"*)  [ -f "$WORK/.moved" ] && echo bbbbbbb || echo aaaaaaa ;;
  *"rev-parse --short"*)       echo bbbbbbb ;;
  *"rev-parse HEAD"*)          echo bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb ;;
  *"rev-parse --verify"*)      echo bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb ;;
  *"tag -l"*)                  echo 2026.9.3 ;;
  *describe*)                  echo 2026.9.3 ;;
  *"log -1"*)                  echo 2026-09-06T00:00:00+00:00 ;;
  *) : ;;
esac
GIT

cat > "$WORK/bin/podman" <<PODMAN
#!/usr/bin/env bash
case "\$1" in
  build) printf '%s\n' "\$*" >> "$BUILD_LOG"; exit 0 ;;
esac
case "\$*" in
  "ps --format"*)   echo vornik-postgres ;;
  *"image exists"*) exit 1 ;;   # no deployed image -> the row must be rebuilt
  *psql*)           echo 176 ;;
  *"cp "*)          : > "\${@: -1}" ;;
  *)                : ;;
esac
PODMAN

printf '#!/usr/bin/env bash\nexit 0\n' > "$WORK/bin/systemctl"
printf '#!/usr/bin/env bash\nexit 0\n' > "$WORK/bin/curl"
chmod +x "$WORK/bin/git" "$WORK/bin/podman" "$WORK/bin/systemctl" "$WORK/bin/curl"

run_updater() {
  rm -f "$WORK/.moved"; : > "$BUILD_LOG"
  cp "$WORK/old.sh" "$REPO/deployments/podman/vornik-update.sh"
  chmod +x "$REPO/deployments/podman/vornik-update.sh"
  env -u VORNIK_UPDATE_REEXEC -u VORNIK_UPDATE_COPY_DIR \
    PATH="$WORK/bin:$PATH" HOME="$WORK/home" OBTAIN_MODE="${OBTAIN_MODE-}" \
    VORNIK_DIR="$REPO" VORNIK_CONFIG="$WORK/cfg/config.yaml" VORNIK_BIN_DIR="$WORK/localbin" \
    bash "$REPO/deployments/podman/vornik-update.sh" "$@" > "$WORK/out" 2>&1 || true
}

fail() { echo "FAIL: $*"; echo '--- updater output ---'; cat "$WORK/out"; exit 1; }

# ---------------------------------------------------------------------------
# 1. The regression itself: a checkout that rewrites the script must not
#    derail the run. Pre-fix this dies at the checkout with a nonsense
#    "command not found" and never reaches step 3b.
# ---------------------------------------------------------------------------
run_updater --yes --no-build

grep -q 'command not found\|syntax error\|unexpected end of file' "$WORK/out" && \
  fail "the checkout corrupted the running script — bash resumed at a stale byte
      offset inside the new file. vornik-update.sh must execute from a private
      copy so the file it reads cannot change underneath it."

grep -q 'Rebuilding container images' "$WORK/out" || \
  fail "the run never reached step 3b (image rebuild) after the checkout"

grep -q 'ghcr.io/grinco/vornik-agent:latest' "$BUILD_LOG" || \
  fail "step 3b ran but built no image; podman build was never invoked for the manifest row"

grep -q -- '--target' "$BUILD_LOG" && \
  fail "the '-' placeholder was passed through as a real --target (C8)"

echo 'update: checkout-rewrites-self does not derail the run — OK'

# ---------------------------------------------------------------------------
# 2. The trap that hid it. A run that dies after the checkout leaves HEAD on
#    the target, so the retry must not report "nothing to do" on an install
#    that was never updated. Guard the exact string, so if the corruption ever
#    returns the second run cannot quietly absolve it.
# ---------------------------------------------------------------------------
if grep -q 'Nothing to do' "$WORK/out"; then
  fail "the first run must not short-circuit; it has real work to do"
fi

# HEAD moved during run 1; a second invocation now sees current == target.
env -u VORNIK_UPDATE_REEXEC -u VORNIK_UPDATE_COPY_DIR \
  PATH="$WORK/bin:$PATH" HOME="$WORK/home" \
  VORNIK_DIR="$REPO" VORNIK_CONFIG="$WORK/cfg/config.yaml" VORNIK_BIN_DIR="$WORK/localbin" \
  bash "$REPO/deployments/podman/vornik-update.sh" --yes --no-build --check > "$WORK/out2" 2>&1 || true
grep -q 'already at the target commit' "$WORK/out2" || \
  fail "--check should report the checkout is at the target after run 1 moved it"

echo 'update: post-run --check reports the moved checkout — OK'

# ---------------------------------------------------------------------------
# 3. --help must still work from the re-exec'd copy: it reads its own comment
#    header out of "$0", and "$0" is the copy.
# ---------------------------------------------------------------------------
run_updater --help
grep -q 'safe in-place upgrade' "$WORK/out" || \
  fail "--help lost its comment header (the re-exec copy must carry it)"

echo 'update: --help works from the private copy — OK'

# ---------------------------------------------------------------------------
# 4. The private copy must not be left behind.
# ---------------------------------------------------------------------------
leaked=$(find "${TMPDIR:-/tmp}" -maxdepth 1 -name 'vornik-update.*' -newer "$WORK/old.sh" 2>/dev/null | head -5)
[ -z "$leaked" ] || fail "the re-exec copy was left behind: $leaked"

echo 'update: the private copy is cleaned up — OK'

# ---------------------------------------------------------------------------
# 5. Stage 2 (design §S2.6 test 3). The updater builds exactly the rows
#    `vornik-images -obtain` hands it — no more, and no second opinion.
#
#    An image the obtain step already PULLED must not also be built. Before
#    Stage 2 this script carried its own revision-label comparison, which after
#    a pull compares a CE commit against an EE HEAD, never matches, and rebuilds
#    forever. Emitting nothing is how the obtain step says "I dealt with it".
# ---------------------------------------------------------------------------
OBTAIN_MODE=nothing run_updater --yes --no-build
if grep -q 'ghcr.io/grinco/vornik-agent' "$BUILD_LOG"; then
  fail "the updater built an image the obtain step had already handled.
      A pulled image must not also be built — see design §S2.3."
fi
grep -q 'Images: 0 built locally' "$WORK/out" || \
  fail "the updater did not report an empty build set"
unset OBTAIN_MODE

echo 'update: an obtained image is not rebuilt — OK'

# A row that IS handed back must be built, or the fallback path is dead.
run_updater --yes --no-build
grep -q 'ghcr.io/grinco/vornik-agent:latest' "$BUILD_LOG" || \
  fail "the updater did not build a row the obtain step handed back — the local-build
      fallback is contract C7 and must survive Stage 2"

echo 'update: a handed-back row is built — OK'

echo 'update_test.sh: PASS'

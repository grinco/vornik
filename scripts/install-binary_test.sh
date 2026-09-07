#!/usr/bin/env bash
# install-binary_test.sh — installing over a RUNNING binary must work.
#
# THE INCIDENT (2026-09-06). `make install-enterprise` on the reference host
# died at its last step:
#
#     cp: cannot create regular file '~/.local/bin/vornik-enterprise': Text file busy
#     make: *** [Makefile:1305: install-enterprise] Error 1
#
# The daemon was running from that path. `cp` opens the target and truncates it,
# and Linux refuses that for a file being executed (ETXTBSY) — so the one target
# whose whole purpose is upgrading a running deployment could not upgrade a
# running deployment. Its own epilogue says "restart to pick up the new binary",
# which is only reachable if the install worked while the old one ran.
#
# It failed AFTER rebuilding all seven container images, leaving the host in the
# half-applied state the image-freshness design exists to prevent: new images,
# old daemon.
#
# The fix is rename(2), not a better copy. Writing beside the target and moving
# it into place replaces the DIRECTORY ENTRY; the running process keeps its open
# inode and exits normally, and the swap is atomic — no window where the path
# holds a half-written binary.
#
# Run: bash scripts/install-binary_test.sh
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
INSTALL="$ROOT/scripts/install-binary.sh"
[ -x "$INSTALL" ] || { echo "FAIL: $INSTALL not found or not executable"; exit 1; }

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# A real executable we can hold busy. Copying a static-ish system binary keeps
# this hermetic — no compiler needed, and `sleep` is executing for the whole
# window, which is exactly the ETXTBSY condition.
cp /bin/sleep "$TMP/daemon"
chmod 755 "$TMP/daemon"
printf '#!/bin/sh\necho new\n' > "$TMP/new-daemon"
chmod 755 "$TMP/new-daemon"

"$TMP/daemon" 60 &
BUSY_PID=$!
trap 'kill "$BUSY_PID" 2>/dev/null || true; rm -rf "$TMP"' EXIT

# Give the kernel a moment to actually be executing it, then prove the file is
# genuinely busy — otherwise this test proves nothing about the real condition.
for _ in 1 2 3 4 5 6 7 8 9 10; do
  cp /bin/true "$TMP/daemon" 2>/dev/null && { sleep 0.1; continue; }
  break
done
if cp /bin/true "$TMP/daemon" 2>/dev/null; then
  echo "FAIL: the fixture is not actually busy — cp succeeded, so this test could"
  echo "      not distinguish the broken install from the fixed one."
  exit 1
fi

# THE REGRESSION. This is what `cp` could not do.
if ! "$INSTALL" "$TMP/new-daemon" "$TMP/daemon"; then
  echo "FAIL: install-binary.sh could not install over a running binary — that is"
  echo "      the ETXTBSY defect it exists to fix."
  exit 1
fi

[ "$(cat "$TMP/daemon")" = "$(cat "$TMP/new-daemon")" ] || {
  echo "FAIL: the target was not replaced with the new binary"; exit 1; }

[ -x "$TMP/daemon" ] || { echo "FAIL: the installed binary is not executable"; exit 1; }

# The already-running process must be undisturbed: rename replaces the directory
# entry, it does not touch the inode the kernel is executing.
kill -0 "$BUSY_PID" 2>/dev/null || {
  echo "FAIL: the running process died — the install must not disturb it"; exit 1; }

echo 'install-binary: replaces a running binary — OK'

# No temp file left beside the target. A failed or partial install must not
# leave a stray executable in a bin directory.
strays=$(find "$(dirname "$TMP/daemon")" -maxdepth 1 -name '.*.new.*' 2>/dev/null | head -3)
[ -z "$strays" ] || { echo "FAIL: temp file left behind: $strays"; exit 1; }

echo 'install-binary: leaves no temp file — OK'

# A missing source is an error, not a silent no-op that would leave the old
# binary in place while the install reports success.
if "$INSTALL" "$TMP/does-not-exist" "$TMP/daemon" 2>/dev/null; then
  echo "FAIL: a missing source must fail loudly"; exit 1
fi

echo 'install-binary: a missing source fails — OK'
echo 'install-binary_test.sh: PASS'

#!/usr/bin/env bash
# test-image-uid-agnostic.sh — the agent image must work when the process runs
# as a uid the image did not bake.
#
# Incident 2026-09-17/18: the published image bakes uid 1000 (CI builds it
# without VORNIK_UID). On a host whose operator is uid 1001 every agent step
# died on permission denied, and the whole deployment was down for 9 hours.
#
# The subtler half, and the reason `run_as_user` is only a stopgap: --user
# corrects WHO the process is and cannot correct what the image OWNS. The
# Containerfile put GOPATH/GOCACHE/GOMODCACHE under /home/vornik, which
# `useradd --create-home` leaves at 0750 owned by the baked uid — so overriding
# --user left the workspace writable and every `go build` inside the agent
# broken, which is the coder role's entire function.
#
# See onboarding-hardening-design.md, D4.
set -u

IMAGE="${AGENT_IMAGE:-ghcr.io/grinco/vornik-agent:latest}"

command -v podman >/dev/null 2>&1 || { echo "SKIP: podman not available"; exit 0; }
podman image exists "$IMAGE" 2>/dev/null || { echo "SKIP: $IMAGE not present locally"; exit 0; }

# A uid the image cannot have baked. Deliberately not the host's either, so the
# test proves uid-AGNOSTICISM rather than "it happens to match today".
ALIEN_UID=$(( $(id -u) + 7 ))
ALIEN_GID=$(( $(id -g) + 7 ))

pass=0
fail=0
ok()  { pass=$((pass+1)); echo "PASS: $1"; }
bad() { fail=$((fail+1)); echo "FAIL: $1" >&2; }

# Run a probe inside the image as the alien uid. --userns=keep-id:uid=,gid= is
# NOT used: the point is that the image works under a plain --user override,
# which is what runtime.run_as_user produces.
run_as_alien() {
    podman run --rm --user "${ALIEN_UID}:${ALIEN_GID}" --entrypoint="" "$IMAGE" sh -c "$1" 2>&1
}

echo "image=$IMAGE  alien uid=${ALIEN_UID}:${ALIEN_GID}  (host $(id -u):$(id -g))"

# --- 1. The Go cache tree must be writable, or every go build fails. ---
for var in GOPATH GOCACHE GOMODCACHE; do
    dir=$(podman image inspect "$IMAGE" --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null \
          | sed -n "s/^${var}=//p" | head -1)
    if [ -z "$dir" ]; then
        bad "$var is not set in the image env"
        continue
    fi
    out=$(run_as_alien "mkdir -p '$dir' 2>/dev/null && touch '$dir/.probe' 2>/dev/null && echo WRITABLE || echo DENIED")
    case "$out" in
        *WRITABLE*) ok "$var ($dir) is writable by an unbaked uid" ;;
        *)          bad "$var ($dir) is NOT writable as ${ALIEN_UID} — go build/test inside the agent will fail" ;;
    esac
done

# --- 2. HOME must be usable: git and ssh read $HOME/.gitconfig, $HOME/.ssh. ---
# $HOME must expand INSIDE the container, not on this host — single quotes
# are the point, so the expansion survives into `podman run ... sh -c`.
# shellcheck disable=SC2016
home=$(run_as_alien 'echo -n $HOME')
if [ -n "$home" ]; then
    out=$(run_as_alien "mkdir -p '$home' 2>/dev/null && touch '$home/.probe' 2>/dev/null && echo WRITABLE || echo DENIED")
    case "$out" in
        *WRITABLE*) ok "HOME ($home) is writable by an unbaked uid" ;;
        *)          bad "HOME ($home) is NOT writable as ${ALIEN_UID} — git/ssh config writes will fail" ;;
    esac
else
    bad "could not read HOME from the image"
fi

# --- 3. The contract mount points, for the case where one is not bind-mounted. ---
for d in /app/input /app/output /app/workspace; do
    out=$(run_as_alien "touch '$d/.probe' 2>/dev/null && echo WRITABLE || echo DENIED")
    case "$out" in
        *WRITABLE*) ok "$d is writable by an unbaked uid" ;;
        *)          bad "$d is NOT writable as ${ALIEN_UID}" ;;
    esac
done

# --- 4. The entrypoint must still be readable+executable by any uid. ---
out=$(run_as_alien 'test -r /entrypoint.sh && test -x /entrypoint.sh && echo OK || echo NO')
case "$out" in
    *OK*) ok "/entrypoint.sh is readable and executable by an unbaked uid" ;;
    *)    bad "/entrypoint.sh is not usable as ${ALIEN_UID}" ;;
esac

echo "================================"
echo "PASSED: $pass"
echo "FAILED: $fail"
[ "$fail" -eq 0 ]

#!/usr/bin/env bash
# Render kong.yml.template → a runtime-dir kong.yml with the provider secrets
# substituted, for vornik-gateway.service to bind-mount read-only.
#
# WHY render on the HOST rather than inside the container (as
# deployments/podman/gateway.compose.yaml does): the compose entrypoint has to
# thread a sed invocation through TWO layers of variable expansion, which is why
# it carries a five-line comment about `$$` escaping. A systemd ExecStart would
# add a third (systemd expands $VAR itself). Rendering here keeps every `$` in a
# plain shell script where it means what it looks like.
#
# The rendered file carries live credentials, so it is written to the systemd
# runtime directory: tmpfs, cleared on logout, never the repo or /tmp.
#
# SECURITY BOUNDARY IS THE DIRECTORY, NOT THE FILE MODE. The file is 0644 and
# that is deliberate: under rootless podman the container's `kong` user maps to
# a subuid, NOT to the host user, so a 0600 file owned by the invoking user is
# unreadable inside the container — Kong fails with a bare "Permission denied"
# that looks like an SELinux problem and is not. Confinement instead comes from
# the runtime directory being 0700 and owned by this user: no other host user
# can traverse into it whatever the file mode says.
set -euo pipefail

TEMPLATE="${1:?usage: vornik-gateway-render.sh <template> <output>}"
OUTPUT="${2:?usage: vornik-gateway-render.sh <template> <output>}"
ENV_FILE="${VORNIK_GATEWAY_ENV:-${HOME}/.config/vornik/secrets/gateway.env}"

if [[ ! -r "$TEMPLATE" ]]; then
    echo "vornik-gateway-render: template not readable: $TEMPLATE" >&2
    exit 1
fi
if [[ ! -r "$ENV_FILE" ]]; then
    echo "vornik-gateway-render: env file not readable: $ENV_FILE" >&2
    echo "  expected VORNIK_GATEWAY_TOKEN + the provider keys (mode 0600)" >&2
    exit 1
fi

# shellcheck disable=SC1090  # path is operator config, not a repo file
set -a; . "$ENV_FILE"; set +a

# Fail loudly on a missing secret. Rendering an empty value would produce a
# gateway that starts cleanly and then 401s every upstream call — the failure
# would surface as "provider is broken", days later, far from the cause.
for var in VORNIK_GATEWAY_TOKEN GOOGLE_MAPS_API_KEY MOLTBOOK_API_KEY; do
    if [[ -z "${!var:-}" ]]; then
        echo "vornik-gateway-render: $var is empty or unset in $ENV_FILE" >&2
        exit 1
    fi
done

# 0700 on the directory is what actually confines the rendered credentials
# (see the header). Set it explicitly rather than inheriting whatever the
# runtime dir happened to have.
OUTDIR="$(dirname "$OUTPUT")"
mkdir -p "$OUTDIR"
chmod 700 "$OUTDIR"

# Remove rather than truncate: a stale file may carry an SELinux label or
# ownership from a previous container mount, and truncating in place would keep
# it. Every start begins from a fresh file.
rm -f "$OUTPUT"
: > "$OUTPUT"
chmod 644 "$OUTPUT"

# `|` is a safe delimiter here: the token is hex and the provider keys are
# alphanumeric/-/_ — none can contain it. Checked rather than assumed.
for var in VORNIK_GATEWAY_TOKEN GOOGLE_MAPS_API_KEY MOLTBOOK_API_KEY; do
    if [[ "${!var}" == *"|"* ]]; then
        echo "vornik-gateway-render: $var contains '|', which breaks the sed delimiter" >&2
        exit 1
    fi
done

sed -e "s|__VORNIK_GATEWAY_TOKEN__|${VORNIK_GATEWAY_TOKEN}|g" \
    -e "s|__GOOGLE_MAPS_API_KEY__|${GOOGLE_MAPS_API_KEY}|g" \
    -e "s|__MOLTBOOK_API_KEY__|${MOLTBOOK_API_KEY}|g" \
    "$TEMPLATE" > "$OUTPUT"

# A leftover placeholder means the template gained a secret nobody wired up.
if grep -q '__[A-Z_]\+__' "$OUTPUT"; then
    echo "vornik-gateway-render: unsubstituted placeholder(s) remain:" >&2
    grep -o '__[A-Z_]\+__' "$OUTPUT" | sort -u >&2
    rm -f "$OUTPUT"
    exit 1
fi

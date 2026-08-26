#!/bin/sh
# Unit tests for scripts/gen-shell-env.sh.
#
# The generator reads database.name out of config.yaml to populate
# VORNIK_BENCH_DENY_DATABASES, the variable that denies THIS deployment's own
# database as a memory-benchmark target (internal/membench/guard.go — the
# benchmark bulk-writes and CLEARS its target, and the shipped denylist only
# covers the generic prod/production/live names).
#
# That value is frequently a ${VAR} PLACEHOLDER rather than a literal:
# deployments/podman/config/vornik.host.yaml ships `name: "${POSTGRES_DB}"`.
# Emitting it verbatim put a variable reference in the fragment ABOVE the loop
# that sources the secrets which define it, so it expanded to the empty string
# and the guard silently protected nothing on every placeholder-based install.
#
# systemctl is stubbed throughout so these tests read the fixture config dir
# rather than whatever unit happens to be installed on the host.
#
# Run: sh scripts/gen-shell-env_test.sh
set -u

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
GEN="$SCRIPT_DIR/gen-shell-env.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
STUB="$TMP/bin"
mkdir -p "$STUB"

pass=0; fail=0
ok()  { pass=$((pass+1)); echo "  ok: $*"; }
bad() { fail=$((fail+1)); echo "  FAIL: $*"; }

# stub_systemctl <body> — fake systemctl so unit_env() reads our fixture and
# not the host's real vornik.service.
stub_systemctl() {
  printf '#!/bin/sh\n%s\n' "$1" > "$STUB/systemctl"
  chmod +x "$STUB/systemctl"
}

# fixture <case-dir> <db-name-line> — build a config dir; echoes its path.
fixture() {
  d="$TMP/$1"
  mkdir -p "$d/secrets" "$d/data"
  cat > "$d/config.yaml" <<EOF
server:
  address: "0.0.0.0:8080"
database:
  host: 127.0.0.1
  $2
  user: "\${POSTGRES_USER}"
  sslmode: disable
EOF
  printf '%s' "$d"
}

# generate <config-dir> — run the generator, echo the fragment path.
generate() {
  PATH="$STUB:$PATH" bash "$GEN" "$1" "$1/data" "$1/shell-env.sh" >/dev/null 2>&1
  printf '%s' "$1/shell-env.sh"
}

# deny_value <fragment> — source it in a clean sh and print the deny-list.
deny_value() {
  ( PATH="$STUB:$PATH"; . "$1" >/dev/null 2>&1; printf '%s' "${VORNIK_BENCH_DENY_DATABASES:-}" )
}

# --- placeholder database name, value in secrets/*.env --------------------
echo "--- database.name is a \${VAR} placeholder, resolved from secrets/ ---"
stub_systemctl 'exit 1'
d="$(fixture placeholder-secrets 'name: "${POSTGRES_DB}"')"
printf 'POSTGRES_DB=acme_memories\nPOSTGRES_USER=acme\n' > "$d/secrets/database.env"
got="$(deny_value "$(generate "$d")")"
if [ "$got" = "acme_memories" ]; then
  ok "deny-list resolved to the real database name"
else
  bad "deny-list is '$got', want 'acme_memories' — the deployment's own database is NOT denied"
fi

# --- placeholder database name, value in the unit's EnvironmentFile -------
# The stock layout puts POSTGRES_DB in vornik.env (an EnvironmentFile=), not
# under secrets/. The generator must resolve that source too.
echo "--- database.name placeholder, resolved from the unit's EnvironmentFile ---"
d="$(fixture placeholder-envfile 'name: "${POSTGRES_DB}"')"
printf 'POSTGRES_DB=envfile_memories\n' > "$d/vornik.env"
stub_systemctl "case \"\$*\" in
  *EnvironmentFiles*) echo '$d/vornik.env (ignore_errors=yes)' ;;
  *) exit 1 ;;
esac"
got="$(deny_value "$(generate "$d")")"
if [ "$got" = "envfile_memories" ]; then
  ok "deny-list resolved from the unit's EnvironmentFile"
else
  bad "deny-list is '$got', want 'envfile_memories'"
fi

# --- literal database name (must keep working) ---------------------------
echo "--- database.name is a literal ---"
stub_systemctl 'exit 1'
d="$(fixture literal 'name: plain_db')"
got="$(deny_value "$(generate "$d")")"
if [ "$got" = "plain_db" ]; then
  ok "literal database name still emitted"
else
  bad "deny-list is '$got', want 'plain_db'"
fi

# --- an unresolvable placeholder must not emit a bogus name --------------
# Failing to a denial that does not happen is the documented worst case; a
# deny-list containing the literal text "${POSTGRES_DB}" would be worse, as it
# reads like a configured protection while matching no database.
echo "--- unresolvable placeholder ---"
stub_systemctl 'exit 1'
d="$(fixture unresolvable 'name: "${POSTGRES_DB}"')"
got="$(deny_value "$(generate "$d")")"
case "$got" in
  *'$'*|*'{'*) bad "deny-list leaked an unexpanded placeholder: '$got'" ;;
  *)           ok "no unexpanded placeholder in the fragment ('$got')" ;;
esac

# --- the fragment is world-readable, so it must hold no secret values -----
echo "--- fragment carries no secrets ---"
stub_systemctl 'exit 1'
d="$(fixture secrets-leak 'name: "${POSTGRES_DB}"')"
printf 'POSTGRES_DB=leak_db\nVORNIK_DATABASE_PASSWORD=hunter2_do_not_emit\n' > "$d/secrets/database.env"
frag="$(generate "$d")"
if grep -q 'hunter2_do_not_emit' "$frag"; then
  bad "the 0644 fragment inlined a secret value from secrets/"
else
  ok "no secret value inlined into the fragment"
fi

echo "---"
echo "PASS: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1

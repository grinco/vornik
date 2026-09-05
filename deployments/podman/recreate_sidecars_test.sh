#!/usr/bin/env bash
# Tests for recreate-sidecars.sh (image-freshness design §12.4 test 1).
#
# Stubs podman and systemctl on PATH and records what the script asked them
# to do, so the recreate rule per container kind can be asserted without a
# container runtime: a compose-labelled container yields ONE
# `compose -f <file> up -d --no-deps --force-recreate <service>` against the
# labelled file (with --env-file when one is configured), a unit-run
# container yields ONE `systemctl --user restart <unit>`, a tag with no
# containers yields nothing, health-on-failure is carried over, --dry-run
# runs nothing, and a failing recreate exits non-zero naming the tag.
#
# Run: bash deployments/podman/recreate_sidecars_test.sh
set -u
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$SCRIPT_DIR/recreate-sidecars.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"
CALLS="$TMP/calls"
: > "$CALLS"

# The stubbed host: two broker containers from one compose stack, one
# unit-run scraper, one agent container that is nobody's sidecar.
cat > "$TMP/bin/podman" <<'EOF'
#!/usr/bin/env bash
echo "podman $*" >> "$CALLS"
case "$1 $2" in
  "ps -a")
    printf '%s\n' \
      $'vornik-broker-ta\tlocalhost/vornik-broker-ta:latest\tdeployments/podman/trading.compose.yaml\t/repo/deployments/podman\tbroker-ta' \
      $'vornik-broker-news\tlocalhost/vornik-broker-news:latest\tdeployments/podman/trading.compose.yaml\t/repo/deployments/podman\tbroker-news' \
      $'vornik-scraper\tlocalhost/vornik-scraper:latest\t\t\t' \
      $'vornik-snake-lead-task-1\tghcr.io/grinco/vornik-agent:latest\t\t\t'
    ;;
  "inspect vornik-broker-ta") echo "restart" ;;
  "inspect vornik-broker-news") echo "none" ;;
  "inspect "*) echo "" ;;
  "compose "*) if [ "${FAIL_COMPOSE:-0}" = 1 ]; then exit 7; fi ;;
  "update "*) ;;
esac
exit 0
EOF
cat > "$TMP/bin/systemctl" <<'EOF'
#!/usr/bin/env bash
echo "systemctl $*" >> "$CALLS"
exit 0
EOF
chmod +x "$TMP/bin/podman" "$TMP/bin/systemctl"
export PATH="$TMP/bin:$PATH" CALLS

pass=0; fail=0
ok()  { pass=$((pass+1)); echo "PASS: $1"; }
bad() { fail=$((fail+1)); echo "FAIL: $1"; }
# expect <ok-message> <bad-message> <command...>: PASS when the command
# succeeds, FAIL (with the recorded calls) otherwise.
expect() {
  local okmsg="$1" badmsg="$2"; shift 2
  if "$@"; then ok "$okmsg"; else bad "$badmsg: $(tr '\n' ' ' < "$CALLS")"; fi
}
# expect_not <ok-message> <bad-message> <command...>: the inverse.
expect_not() {
  local okmsg="$1" badmsg="$2"; shift 2
  if "$@"; then bad "$badmsg: $(tr '\n' ' ' < "$CALLS")"; else ok "$okmsg"; fi
}
# The emitter's rows for three rebuilt tags; "-" is NoTarget (design §12 C8).
rows() {
  printf 'localhost/vornik-broker-ta:latest\tservices/broker-ta/Containerfile\t-\t.\tcompose:trading\n'
  printf 'localhost/vornik-scraper:latest\tservices/scraper/Containerfile\t-\t.\tunit:vornik-scraper|compose:scraper\n'
  printf 'localhost/vornik-scraper-login:latest\timages/vornik-scraper-login/Containerfile\t-\t.\tunit:vornik-scraper|compose:scraper\n'
}

# --- 1. one compose recreate per labelled container, against the labelled file, with the env file
: > "$CALLS"; touch "$TMP/trading.env"
out=$(rows | VORNIK_TRADING_ENV="$TMP/trading.env" bash "$SCRIPT" 2>&1); rc=$?
expect "exit 0 on a clean run" "exit $rc" test "$rc" -eq 0
# The label names the file relative to where compose was RUN (the repo root),
# and working_dir is the file's own directory: the script must resolve by
# basename against working_dir, not join the two (which doubled the path on
# the reference host's first dry run).
expect "broker-ta recreated through its compose file (resolved by basename against working_dir) with --no-deps --force-recreate and the env file" \
  "no compose recreate for broker-ta against /repo/deployments/podman/trading.compose.yaml" \
  grep -q -- "podman compose -f /repo/deployments/podman/trading.compose.yaml --env-file $TMP/trading.env up -d --no-deps --force-recreate broker-ta" "$CALLS"
n=$(grep -c "force-recreate broker-ta" "$CALLS")
expect "exactly one recreate for broker-ta" "broker-ta recreated $n times" test "$n" = 1
expect_not "a container whose tag was not rebuilt is left alone" "broker-news was not in the rebuilt rows and must not be recreated" grep -q "force-recreate broker-news" "$CALLS"
expect_not "agent containers are ignored" "the agent container is nobody's sidecar" grep -q "vornik-snake-lead-task-1" "$CALLS"

# --- 2. health-on-failure carried over from the old container
expect "health-on-failure=restart re-applied to the recreated broker-ta" "health-on-failure not re-applied" grep -q "podman update --health-on-failure=restart vornik-broker-ta" "$CALLS"

# --- 3. a unit-run container restarts its unit, named by the condition
expect "the scraper's unit is restarted (the unit's ExecStartPre rm -f makes it a recreate)" "no unit restart for the scraper" grep -q "systemctl --user restart vornik-scraper" "$CALLS"
expect_not "the scraper is not handed to compose" "a unit-run container must not be handed to compose" grep -q "force-recreate.*scraper" "$CALLS"

# --- 4. a rebuilt tag with no containers is a no-op that says so
if echo "$out" | grep -q "vornik-scraper-login:latest.*no container"; then ok "a tag with no containers is reported, not an error"; else bad "missing no-container line: $out"; fi

# --- 5. --dry-run runs nothing
: > "$CALLS"
out=$(rows | bash "$SCRIPT" --dry-run 2>&1); rc=$?
expect "--dry-run exits 0" "--dry-run exit $rc" test "$rc" -eq 0
expect_not "--dry-run recreated nothing" "--dry-run must not recreate anything" grep -q -E "podman compose |systemctl --user restart|podman update" "$CALLS"
if echo "$out" | grep -q "would recreate vornik-broker-ta"; then ok "--dry-run names what it would recreate"; else bad "--dry-run output: $out"; fi

# --- 6. a failing recreate is fatal and names the tag
: > "$CALLS"
out=$(rows | FAIL_COMPOSE=1 bash "$SCRIPT" 2>&1); rc=$?
expect "a failed recreate exits non-zero" "a failed recreate must not exit 0" test "$rc" -ne 0
if echo "$out" | grep -q "localhost/vornik-broker-ta:latest"; then ok "the failure names the tag"; else bad "failure output lacks the tag: $out"; fi

echo "recreate-sidecars: $pass passed, $fail failed"
[ "$fail" -eq 0 ]

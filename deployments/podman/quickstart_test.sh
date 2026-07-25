#!/bin/sh
# Unit tests for the detection/selection helpers in quickstart.sh.
# Stubs command -v / package managers / podman so the helpers' decision
# trees run without touching the host. Mirrors postinstall_detect_test.sh:
# source only the functions (guarded main) and assert on their choices.
#
# PATH is isolated to a stub bin dir so the host's own brew/dnf/rpm-ostree
# can't leak into command -v. The one host-dependent case (is_immutable's
# "mutable" branch) is guarded: it can't be exercised on a host that
# already has /run/ostree-booted, so we skip it there — CI's ubuntu-latest
# runner has no ostree marker and exercises it.
#
# Run: sh deployments/podman/quickstart_test.sh
set -u

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# Source only the helpers from quickstart.sh (guarded main). The guard
# returns before any uname/id/sudo/podman/git/build side effect.
VORNIK_QUICKSTART_SOURCED=1 . "$SCRIPT_DIR/quickstart.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
STUB="$TMP/bin"
mkdir -p "$STUB"

# make_stub <name> <body> — write a fake executable to $STUB/<name>.
make_stub() {
  printf '%s\n' "$2" > "$STUB/$1"
  chmod +x "$STUB/$1"
}

# isolate PATH to the stub bin only, so command -v sees only our fakes.
# Subshell keeps the PATH change local to the call.
isolated() { ( PATH="$STUB"; "$@" ); }

pass=0; fail=0
ok()  { pass=$((pass+1)); echo "  ok: $*"; }
bad() { fail=$((fail+1)); echo "  FAIL: $*"; }

# --- is_immutable --------------------------------------------------------
echo "--- is_immutable ---"

# rpm-ostree on PATH ⇒ immutable (returns 0) regardless of host marker.
make_stub rpm-ostree 'exit 0'
if isolated is_immutable; then ok "rpm-ostree on PATH ⇒ immutable"; else bad "rpm-ostree on PATH not detected"; fi
rm -f "$STUB/rpm-ostree"

# No rpm-ostree and no /run/ostree-booted ⇒ mutable (returns 1). Skip on
# hosts that already boot via ostree — we can't fake the marker away.
if [ -f /run/ostree-booted ]; then
  ok "host is ostree-booted; negative is_immutable branch skipped (covered on CI ubuntu-latest)"
elif isolated is_immutable; then
  bad "mutable host misdetected as immutable"
else
  ok "no ostree marker ⇒ mutable"
fi

# --- install_sys package-manager selection -------------------------------
# install_sys picks the FIRST available manager in the brew > dnf >
# apt-get > zypper > pacman order. Stub each candidate to record its args.
echo "--- install_sys selection ---"
make_stub sudo '#!/bin/sh
"$@"
'
make_stub brew     '#!/bin/sh
echo "brew $*"
'
make_stub dnf      '#!/bin/sh
echo "dnf $*"
'
make_stub apt-get  '#!/bin/sh
echo "apt-get $*"
'
make_stub zypper   '#!/bin/sh
echo "zypper $*"
'
make_stub pacman   '#!/bin/sh
echo "pacman $*"
'

# brew wins over dnf when both present.
OUT=$(isolated install_sys pkg-a pkg-b)
case "$OUT" in *"brew install pkg-a pkg-b"*) ok "brew preferred over dnf";; *) bad "brew not preferred: $OUT";; esac
rm -f "$STUB/brew"

# dnf wins when brew absent.
OUT=$(isolated install_sys pkg-a)
case "$OUT" in *"dnf install -y pkg-a"*) ok "dnf chosen when brew absent";; *) bad "dnf not chosen: $OUT";; esac
rm -f "$STUB/dnf"

# apt-get wins when brew+dnf absent.
OUT=$(isolated install_sys pkg-a)
case "$OUT" in *"apt-get install -y pkg-a"*) ok "apt-get chosen when brew+dnf absent";; *) bad "apt-get not chosen: $OUT";; esac
rm -f "$STUB/apt-get" "$STUB/zypper" "$STUB/pacman"

# No manager ⇒ install_sys returns 1 (caller falls back / prints guidance).
if isolated install_sys pkg-a >/dev/null 2>&1; then bad "install_sys should fail with no manager"; else ok "install_sys returns 1 with no manager"; fi

# --- ensure_compose ------------------------------------------------------
echo "--- ensure_compose ---"

# have_compose true ⇒ ensure_compose returns 0 immediately. Stub `podman`
# so `podman compose version` succeeds.
make_stub podman '#!/bin/sh
[ "$1" = compose ] && [ "$2" = version ] && exit 0
exit 1
'
if isolated ensure_compose >/dev/null 2>&1; then ok "have_compose ⇒ ensure_compose short-circuits"; else bad "ensure_compose should short-circuit when compose present"; fi
rm -f "$STUB/podman"

# No podman/pipx/python3 ⇒ ensure_compose falls back to install_sys, then
# re-checks have_compose (still false) ⇒ returns 1. The real install_sys
# output is suppressed inside ensure_compose, so override install_sys to
# record the fallback call to a marker file we can grep.
: > "$TMP/fallback.called"
install_sys() { echo "install_sys $*" >> "$TMP/fallback.called"; }
isolated ensure_compose >/dev/null 2>&1 || true
if grep -q "install_sys podman-compose" "$TMP/fallback.called"; then
  ok "ensure_compose fell back to install_sys (then reported no compose)"
else
  bad "ensure_compose did not fall back to install_sys: $(cat "$TMP/fallback.called" 2>/dev/null)"
fi

# --- anonymous telemetry first-install choice ----------------------------
echo "--- anonymous telemetry first-install choice ---"
TELEMETRY_CFG="$TMP/telemetry-config.yaml"
printf 'server:\n  address: ":8080"\n' >"$TELEMETRY_CFG"
VORNIK_TELEMETRY=off configure_anonymous_telemetry "$TELEMETRY_CFG" 0 >/dev/null
if grep -q '^telemetry:' "$TELEMETRY_CFG" && grep -q 'enabled: false' "$TELEMETRY_CFG"; then
  ok "environment opt-out is persisted in config.yaml"
else
  bad "environment opt-out was not persisted"
fi

TELEMETRY_CFG_ON="$TMP/telemetry-config-on.yaml"
printf 'server:\n  address: ":8080"\n' >"$TELEMETRY_CFG_ON"
VORNIK_TELEMETRY=on configure_anonymous_telemetry "$TELEMETRY_CFG_ON" 0 >/dev/null
if grep -q '^telemetry:' "$TELEMETRY_CFG_ON"; then
  bad "enabled default should not be shipped into config.yaml"
else
  ok "enabled choice leaves telemetry absent from config.yaml"
fi

TELEMETRY_CFG_BAD="$TMP/telemetry-config-bad.yaml"
printf 'server:\n  address: ":8080"\n' >"$TELEMETRY_CFG_BAD"
VORNIK_TELEMETRY=invalid configure_anonymous_telemetry "$TELEMETRY_CFG_BAD" 0 >/dev/null 2>&1
if grep -q 'enabled: false' "$TELEMETRY_CFG_BAD"; then
  ok "invalid environment value fails closed"
else
  bad "invalid environment value did not fail closed"
fi

# --- require_safe_checkout_dir ------------------------------------------
echo "--- require_safe_checkout_dir ---"
if isolated require_safe_checkout_dir "$HOME/vornik"; then
  ok "dedicated checkout dir allowed"
else
  bad "safe checkout dir rejected"
fi

if isolated require_safe_checkout_dir "$TMP/vornik"; then
  ok "dedicated temp checkout dir allowed"
else
  bad "safe temp checkout dir rejected"
fi

for unsafe in "" "/" "/tmp" "." ".." "$HOME" "$HOME/" "$HOME/." "$HOME/.." "$TMP/.." "$TMP/../vornik" "$TMP/./vornik"; do
  if isolated require_safe_checkout_dir "$unsafe" >/dev/null 2>&1; then
    bad "unsafe checkout dir accepted: '$unsafe'"
  else
    ok "unsafe checkout dir rejected: '$unsafe'"
  fi
done

# --- final output advertises the setup guide -----------------------------
# Onboarding contract (setup-guide rollout slice 2, restored 2026-07-01):
# the closing "Connect / Run tasks" block must lead users to the first-run
# setup guide, not only to hand-editing vornik.env.
echo "--- final output mentions /ui/setup ---"
if grep -q '/ui/setup' "$SCRIPT_DIR/quickstart.sh"; then
  ok "quickstart output points at the /ui/setup guide"
else
  bad "quickstart.sh never mentions /ui/setup — the guided onboarding path is undiscoverable"
fi

echo "--- install-failure reporting: scrubber strips diverse secret formats ---"
SEEDED="Authorization: Bearer eyJhbGci.eyJzdWIi.SflKxwRJ token=sk-ABCDEFGHIJKLMNOPQR key AKIAIOSFODNN7EXAMPLE https://x?token=supersecretvalue password=hunter2hunter2 -----BEGIN PRIVATE KEY-----MIIabc-----END PRIVATE KEY----- QWxhZGRpbjpvcGVuc2VzYW1lMTIzNDU2Nzg5MA=="
SCRUBBED="$(printf '%s' "$SEEDED" | vornik_scrub)"
for leak in 'eyJhbGci.eyJzdWIi.SflKxwRJ' 'sk-ABCDEFGHIJKLMNOPQR' 'AKIAIOSFODNN7EXAMPLE' 'supersecretvalue' 'hunter2hunter2' 'MIIabc'; do
  case "$SCRUBBED" in
    *"$leak"*) bad "scrubber LEAKED '$leak': $SCRUBBED" ;;
    *) ok "scrubber stripped '$leak'" ;;
  esac
done

echo "--- scrubber strips identifiers (paths/email/IP), not just secrets ---"
# review-20260725-7530 #3: the installer scrubber must reach parity with the Go
# scrubber — home/tilde paths (whole tail, incl. project name), email, IPv4.
IDS="see /var/home/vadim/projects/vornik-marketing/logs and /Users/joe/secretproj/x and ~/work/acme-thing/y ping vadim@vornik.io at 192.0.2.10 or 8.8.4.4"
IDS_SCRUBBED="$(printf '%s' "$IDS" | vornik_scrub)"
for leak in 'vadim' 'vornik-marketing' 'secretproj' 'acme-thing' 'vadim@vornik.io' '192.0.2.10' '8.8.4.4' '/projects/'; do
  case "$IDS_SCRUBBED" in
    *"$leak"*) bad "scrubber LEAKED identifier '$leak': $IDS_SCRUBBED" ;;
    *) ok "scrubber stripped identifier '$leak'" ;;
  esac
done

echo "--- report_install_failure strips this machine's hostname ---"
# The failing command may embed the hostname; the hook replaces it with <host>.
HN="$(uname -n 2>/dev/null || hostname 2>/dev/null || echo UNKNOWNHOST)"
HOUT="$(REF=2026.7.4 BASH_COMMAND="ssh build@$HN /var/home/vadim/x" report_install_failure 9 2>&1)"
case "$HOUT" in
  *"$HN"*) [ "$HN" = UNKNOWNHOST ] && ok "no hostname to strip (skipped)" || bad "hostname '$HN' leaked: $HOUT" ;;
  *) ok "hostname stripped from failing command" ;;
esac

echo "--- report_install_failure emits a prefilled grinco/vornik issue URL ---"
FOUT="$(REF=2026.7.4 report_install_failure 7 2>&1)"
case "$FOUT" in *"github.com/grinco/vornik/issues/new?"*) ok "failure hook prints the grinco/vornik issue URL" ;; *) bad "no issue URL in failure output: $FOUT" ;; esac
case "$FOUT" in *"labels=bug%2Cinstall"*) ok "issue carries bug,install labels" ;; *) bad "labels not encoded: $FOUT" ;; esac
# success (exit 0) must be a no-op
SOUT="$(report_install_failure 0 2>&1)"
case "$SOUT" in *"issues/new"*) bad "failure hook fired on exit 0" ;; *) ok "no-op on exit 0" ;; esac

echo "--- ensure_linger ---"
# Task 11 (F6): loginctl enable-linger used to be fire-and-forget (`|| warn`)
# — on hosts where the unprivileged call is denied, linger silently stayed
# off and the daemon (a systemctl --user unit) dies on logout. ensure_linger
# must verify Linger=yes and escalate to sudo when the unprivileged call
# doesn't stick.
: > "$TMP/linger_calls"
CALLS="$TMP/linger_calls"; export CALLS

# Already-enabled host: no enable-linger call at all (idempotent no-op).
make_stub id '#!/bin/sh
echo testuser
'
make_stub loginctl '#!/bin/sh
case "$1" in
  show-user) echo "Linger=yes" ;;
  enable-linger) echo "enable-linger $*" >> "$CALLS" ;;
esac
'
isolated ensure_linger
if grep -q "enable-linger" "$CALLS"; then
  bad "ensure_linger called enable-linger when already Linger=yes"
else
  ok "ensure_linger short-circuits when linger is already enabled"
fi
rm -f "$STUB/loginctl"

# Unprivileged enable denied ⇒ escalate to sudo.
: > "$CALLS"
make_stub loginctl '#!/bin/sh
case "$1" in
  show-user) echo "Linger=no" ;;
  enable-linger) echo "enable-linger $*" >> "$CALLS"; exit 1 ;;
esac
'
make_stub sudo '#!/bin/sh
echo "sudo $*" >> "$CALLS"
"$@"
'
# Invoke it BARE (no `|| true` around the call) under `set -e`, matching the
# real call site (quickstart.sh runs the non-sourced script under
# `set -euo pipefail` and calls `ensure_linger` unguarded). Regression:
# ensure_linger's own unprivileged `loginctl enable-linger` line used to lack
# `|| true`, so under set -e it aborted the whole installer right here —
# before the sudo escalation or the warn ever ran — which is worse than the
# pre-Task-11 fire-and-forget behavior. `isolated ensure_linger || true`
# (the old form of this test) masked that bug by swallowing the abort.
( PATH="$STUB"; set -e; ensure_linger )
rc=$?
if [ "$rc" -eq 0 ]; then
  ok "ensure_linger tolerates set -e (unprivileged enable-linger failure doesn't abort the caller)"
else
  bad "ensure_linger aborted under set -e (exit $rc) instead of falling through to sudo escalation"
fi
if grep -q "sudo loginctl enable-linger" "$CALLS"; then
  ok "ensure_linger escalates to sudo when unprivileged enable is denied"
else
  bad "ensure_linger did not escalate to sudo: $(cat "$CALLS")"
fi
rm -f "$STUB/id" "$STUB/loginctl" "$STUB/sudo"

echo "--- enable_podman_restart ---"
# Task 12 (F7): deps.compose.yaml sets `restart: always`, but for rootless
# podman that only recovers crashes — NOT a host reboot. Boot autostart needs
# podman-restart.service enabled for the user. Guard on the unit existing so
# hosts without it warn instead of hard-failing.
: > "$TMP/restart_calls"
CALLS="$TMP/restart_calls"; export CALLS
make_stub systemctl '#!/bin/sh
echo "systemctl $*" >> "$CALLS"
case "$1 $2" in
  "--user list-unit-files") exit 0 ;;
  "--user enable") exit 0 ;;
esac
exit 0
'
isolated enable_podman_restart
if grep -q "systemctl --user enable podman-restart.service" "$CALLS"; then
  ok "enable_podman_restart enables podman-restart.service when the unit exists"
else
  bad "did not enable podman-restart.service: $(cat "$CALLS")"
fi
rm -f "$STUB/systemctl"

# Unit not present on this host (e.g. no podman-restart.service shipped) ⇒
# warn, no hard failure, and no enable call attempted.
: > "$CALLS"
make_stub systemctl '#!/bin/sh
echo "systemctl $*" >> "$CALLS"
case "$1 $2" in
  "--user list-unit-files") exit 1 ;;
esac
exit 0
'
if isolated enable_podman_restart >/tmp/enable_podman_restart.$$ 2>&1; then :; fi
if grep -q "systemctl --user enable podman-restart.service" "$CALLS"; then
  bad "enable_podman_restart called enable when list-unit-files reported no unit"
else
  ok "enable_podman_restart skips enable when the unit is absent"
fi
rm -f "$STUB/systemctl" "/tmp/enable_podman_restart.$$"

echo "--- print_success_footer names doctor + report ---"
# Task 8 (F5): the post-install success message must point at both
# `vornikctl doctor` (diagnostics) and `vornikctl report` (anonymized issue
# filing) so a failing first task doesn't strand the user.
FOOTER="$(print_success_footer 2>&1)"
case "$FOOTER" in *"vornikctl doctor"*) ok "footer names vornikctl doctor";; *) bad "footer missing 'vornikctl doctor': $FOOTER";; esac
case "$FOOTER" in *"vornikctl report"*) ok "footer names vornikctl report";; *) bad "footer missing 'vornikctl report': $FOOTER";; esac

echo "---"
echo "PASS: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1

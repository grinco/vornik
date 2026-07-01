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

echo "---"
echo "PASS: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1

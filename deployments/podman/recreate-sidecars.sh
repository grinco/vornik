#!/usr/bin/env bash
#
# recreate-sidecars.sh — make rebuilt images reach the long-running containers
# created from them (image-freshness design §12, contract C5).
#
# A rebuilt tag changes nothing already running: the scraper and broker
# sidecars keep executing the image they were created from until something
# recreates them. This script reads image-manifest rows on stdin — the
# emitter's columns, `tag<TAB>containerfile<TAB>target<TAB>context<TAB>condition`
# — and, for every container whose recorded image name is one of those tags:
#
#   - a container carrying compose labels is recreated with
#       podman compose -f <config_files, resolved against working_dir> \
#         [--env-file <f>] up -d --no-deps --force-recreate <service>
#     --no-deps so recreating a broker never touches ibgateway (its 3-minute
#     login); --force-recreate because a plain `up -d` skips a service whose
#     CONFIG did not change, and an image rebuild is not a config change;
#   - a container without compose labels belongs to a unit, and the row's
#     `unit:<name>` condition names it: `systemctl --user restart <name>`
#     (the unit's ExecStartPre=podman rm -f is what makes that a recreate);
#   - a `podman update --health-on-failure=<action>` setting is read from the
#     old container and re-applied to the new one, because `podman update`
#     writes container config that a recreate discards (the Makefile's
#     trading-sidecar-autorestart lesson, 2026-08-19).
#
# A tag with no containers is a no-op that says so. A failing recreate exits
# non-zero naming the tag — the caller (vornik-update.sh) treats that as fatal
# BEFORE the daemon cutover (C6). --dry-run prints what would happen and does
# nothing.
#
# --env-file: podman-compose does not record it in labels, so a stack whose
# compose file reads variables from one needs it passed again. The trading
# stack's is VORNIK_TRADING_ENV, defaulting to the Makefile's path when that
# file exists; a stack whose file uses no variables needs none.
#
# Usage: <emitter rows> | recreate-sidecars.sh [--dry-run]
set -uo pipefail

DRY=0
case "${1:-}" in
  --dry-run) DRY=1 ;;
  "") ;;
  *) echo "recreate-sidecars: unknown argument '$1' (only --dry-run)" >&2; exit 2 ;;
esac

log() { printf 'recreate-sidecars: %s\n' "$*"; }

TRADING_ENV_DEFAULT="$HOME/.config/vornik/secrets/trading.env"
TRADING_ENV="${VORNIK_TRADING_ENV:-}"
if [[ -z "$TRADING_ENV" && -f "$TRADING_ENV_DEFAULT" ]]; then TRADING_ENV="$TRADING_ENV_DEFAULT"; fi

# env_file_for <compose file> — the --env-file a stack needs, or nothing.
env_file_for() {
  case "$(basename "$1")" in
    trading.compose.yaml) [[ -n "$TRADING_ENV" ]] && printf '%s' "$TRADING_ENV" ;;
  esac
}

# The containers on this host, with the labels that say how each is run.
# {{.Image}} is the image NAME recorded at creation — it still names the tag
# after the tag has moved to a new image, which is exactly the case here.
CONTAINERS="$(podman ps -a --format '{{.Names}}	{{.Image}}	{{index .Labels "com.docker.compose.project.config_files"}}	{{index .Labels "com.docker.compose.project.working_dir"}}	{{index .Labels "com.docker.compose.service"}}' 2>/dev/null)" \
  || { echo "recreate-sidecars: podman ps failed" >&2; exit 1; }

recreate_compose() { # name tag config_files working_dir service
  local name="$1" tag="$2" files="$3" workdir="$4" service="$5"
  # podman-compose records config_files relative to the DIRECTORY IT WAS RUN
  # FROM (the Makefile runs it from the repo root: "deployments/podman/
  # trading.compose.yaml") and working_dir as the compose file's own
  # directory. Joining the two verbatim doubles the path, so resolve against
  # working_dir by basename, which is what working_dir means; fall back to the
  # verbatim join for a provider that records the file relative to it.
  local file="${files%%,*}"
  if [[ "$file" != /* ]]; then
    if [[ -f "$workdir/$(basename "$file")" ]]; then
      file="$workdir/$(basename "$file")"
    elif [[ -f "$workdir/$file" ]]; then
      file="$workdir/$file"
    else
      file="$workdir/$(basename "$file")"
    fi
  fi
  local -a cmd=(podman compose -f "$file")
  local envf; envf="$(env_file_for "$file")"
  [[ -n "$envf" ]] && cmd+=(--env-file "$envf")
  cmd+=(up -d --no-deps --force-recreate "$service")
  local hof; hof="$(podman inspect "$name" --format '{{.Config.HealthcheckOnFailureAction}}' 2>/dev/null || true)"
  if [[ "$DRY" = 1 ]]; then
    log "would recreate $name ($tag) with: ${cmd[*]}"
    return 0
  fi
  log "recreating $name ($tag): ${cmd[*]}"
  "${cmd[@]}" || { echo "recreate-sidecars: FAILED to recreate $name from $tag (compose exit $?) — the container still runs the previous image" >&2; return 1; }
  if [[ -n "$hof" && "$hof" != none ]]; then
    if podman update "--health-on-failure=$hof" "$name" >/dev/null 2>&1; then
      log "  $name: --health-on-failure=$hof re-applied"
    else
      log "  WARNING: could not re-apply --health-on-failure=$hof on $name"
    fi
  fi
}

recreate_unit() { # name tag condition
  local name="$1" tag="$2" cond="$3" unit=""
  local alt
  IFS='|' read -r -a alts <<<"$cond"
  for alt in "${alts[@]}"; do
    [[ "$alt" == unit:* ]] && { unit="${alt#unit:}"; break; }
  done
  if [[ -z "$unit" ]]; then
    echo "recreate-sidecars: $name runs $tag outside compose and its row names no unit: (condition '$cond') — recreate it by hand" >&2
    return 1
  fi
  if [[ "$DRY" = 1 ]]; then
    log "would recreate $name ($tag) with: systemctl --user restart $unit"
    return 0
  fi
  log "recreating $name ($tag): systemctl --user restart $unit"
  systemctl --user restart "$unit" || { echo "recreate-sidecars: FAILED to restart $unit for $name ($tag)" >&2; return 1; }
}

failed=0 total=0
while IFS=$'\t' read -r tag _containerfile _target _context condition; do
  [[ -n "$tag" ]] || continue
  matched=0
  while IFS=$'\t' read -r name image files workdir service; do
    [[ -n "$name" && "$image" == "$tag" ]] || continue
    matched=$((matched + 1)); total=$((total + 1))
    if [[ -n "$service" ]]; then
      recreate_compose "$name" "$tag" "$files" "$workdir" "$service" || failed=$((failed + 1))
    else
      recreate_unit "$name" "$tag" "$condition" || failed=$((failed + 1))
    fi
  done <<<"$CONTAINERS"
  if [[ "$matched" -eq 0 ]]; then log "$tag — no container created from it, nothing to recreate"; fi
done

if [[ "$failed" -gt 0 ]]; then
  echo "recreate-sidecars: $failed of $total container(s) could not be recreated" >&2
  exit 1
fi
if [[ "$DRY" = 1 ]]; then
  log "dry run: $total container(s) would be recreated"
else
  log "$total container(s) recreated"
fi
exit 0

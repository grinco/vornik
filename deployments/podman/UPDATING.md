# Updating a Community-Edition install

If you installed Vornik with the [quickstart](quickstart.sh)
(`curl -fsSL https://get.vornik.io | bash`), `vornik-update.sh` upgrades it
**in place** — same topology, no data loss, with a full backup and one-command
rollback. It is the counterpart to the quickstart: the quickstart installs,
this updates.

> **Don't re-run the quickstart one-liner to "update".** The quickstart is
> idempotent and will update the binaries, but `vornik-update.sh` additionally
> takes a DB dump + binary/config backup first and prints exact rollback
> commands — use it for upgrades so you always have a way back.

## What it assumes (the quickstart layout — all env-overridable)

| Thing | Default | Override |
|---|---|---|
| Source checkout | `~/vornik` | `VORNIK_DIR` |
| Binaries | `~/.local/bin/{vornik,vornikctl}` | `VORNIK_BIN_DIR` |
| Service | `vornik.service` (rootless `systemctl --user`) | `VORNIK_SERVICE` |
| Config | `~/.config/vornik/config.yaml` | `VORNIK_CONFIG` |
| Database | rootless podman container `vornik-postgres`, db/user `vornik` | `VORNIK_PG_CONTAINER` / `VORNIK_PG_DB` / `VORNIK_PG_USER` |
| UI/API port | `8080` | `VORNIK_HTTP_PORT` |
| Build image | `docker.io/library/golang:1.25` | `VORNIK_GO_IMAGE` |

Like the quickstart, it rebuilds `vornik` + `vornikctl` in an **ephemeral
golang container** — no host Go toolchain required.

## Upgrade

```bash
cd ~/vornik/deployments/podman

./vornik-update.sh              # newest fetched tag (asks to confirm)
./vornik-update.sh --check      # report current vs. available; change nothing
./vornik-update.sh --ref 2026.8.0     # pin a specific tag or commit
./vornik-update.sh --ref origin/main  # track the tip of main
./vornik-update.sh --yes        # skip the confirmation prompt (automation)
./vornik-update.sh --rebuild-agent    # also rebuild localhost/vornik-agent:latest
./vornik-update.sh --force      # rebuild+reinstall even if the checkout already matches
./vornik-update.sh --help
```

### What it does, step by step

1. **Preflight** — verifies podman/git/curl/systemctl, the checkout, config, the
   user unit, and that the `vornik-postgres` container is up.
2. **Backup** → `~/vornik-upgrade-backup-<UTC>/`: a full `pg_dump` (custom
   format), the current binaries (`*.prev`), `config.yaml`, and `STATE.txt`
   (pre-upgrade commit + DB migration version).
3. **Checkout + rebuild** — checks out the target ref and rebuilds CE binaries
   in the golang container, version-stamped via `-ldflags`.
4. **Smoke check** — runs the new binary's `-version` before touching the
   service; a binary that can't start is caught before any swap.
5. **Cutover** — stops the service, installs the new binaries, starts it.
   Because the unit is `Type=notify`, systemd reports "ready" only after DB
   migrations applied and health checks passed.
6. **Verify** — polls `/readyz`, prints the DB migration version bump, runs
   `doctor`.

If a running/leased task is in flight, the script warns before the cutover
(the restart interrupts it) — pass `--yes` to proceed anyway in automation.

### Why this is safe for the DB

Every Vornik migration to date is **additive** — `CREATE TABLE IF NOT EXISTS`,
`ADD COLUMN IF NOT EXISTS`. Upgrading does not drop or rewrite existing data.
The script still takes a full dump every run as a safety net.

> **Caveat:** the automation assumes migrations stay additive. If a future
> release ships a destructive migration, read its release notes before running
> unattended (`--yes`). The pre-upgrade dump lets you restore regardless.

## Rollback

The script prints exact rollback commands at the end of every run:

```bash
BK=~/vornik-upgrade-backup-<UTC>              # the dir the run printed
systemctl --user stop vornik.service
install -m0755 "$BK/vornik.prev"    ~/.local/bin/vornik
install -m0755 "$BK/vornikctl.prev" ~/.local/bin/vornikctl
git -C ~/vornik checkout <pre_upgrade_commit> # from $BK/STATE.txt
systemctl --user start vornik.service

# Full DB restore — only if a migration ever misbehaves (additive ones don't need it):
podman exec -i vornik-postgres pg_restore -U vornik -d vornik --clean < "$BK/vornik-vornik-<date>.dump"
```

## Optional: a daily "update available" check

`vornik-check-update.sh` is a silent, unattended check for a newer tag. It never
changes anything — it just writes `~/.cache/vornik/update-available` and logs a
journal NOTICE when an upgrade is waiting. Wire it to a systemd user timer:

```ini
# ~/.config/systemd/user/vornik-update-check.service
[Unit]
Description=Check for a newer Vornik release
[Service]
Type=oneshot
ExecStart=%h/vornik/deployments/podman/vornik-check-update.sh
```

```ini
# ~/.config/systemd/user/vornik-update-check.timer
[Unit]
Description=Daily Vornik update check
[Timer]
OnCalendar=daily
RandomizedDelaySec=1h
Persistent=true
[Install]
WantedBy=timers.target
```

```bash
systemctl --user daemon-reload
systemctl --user enable --now vornik-update-check.timer
loginctl enable-linger "$(id -un)"     # so it runs while logged out
```

Surface the notice on shell login (optional — add to your `~/.bashrc` yourself):

```bash
[ -f "$HOME/.cache/vornik/update-available" ] && \
  printf '\033[1;33m%s\033[0m\n' "$(cat "$HOME/.cache/vornik/update-available")"
```

Disable the check: `systemctl --user disable --now vornik-update-check.timer`.

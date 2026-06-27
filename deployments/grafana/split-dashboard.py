#!/usr/bin/env python3
"""split-dashboard.py — split vornik.json monolith into per-surface deep-dive dashboards.

Run from the repo root or any directory:
    python3 deployments/grafana/split-dashboard.py

Inputs:  deployments/grafana/dashboards/vornik.json
Outputs: deployments/grafana/dashboards/<slug>.json  (one per row/surface)
         vornik.json is left intact (all-in-one remains as the reference copy).

The script is kept in the repo so the split is reproducible when vornik.json
changes.  It is NOT required at deploy time — Grafana provisions every *.json
under the dashboards/ directory; the per-surface files stand on their own.

Algorithm
---------
1. Walk panels[] sequentially.
2. When a "row" panel is encountered, start a new group.
3. Non-row panels that follow belong to the most-recently-seen row.
4. For each group: re-lay-out panels from y=0 (subtract the row panel's y+1
   from each child's gridPos.y), emit a self-contained dashboard JSON.
5. The "cluster" dashboard (cluster.json) is generated separately — it is
   entirely synthesised, not extracted from vornik.json.
"""

import json
import os
import pathlib
import re
import sys

SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
DASHBOARDS_DIR = SCRIPT_DIR / "dashboards"
SOURCE = DASHBOARDS_DIR / "vornik.json"


def slugify(title: str) -> str:
    """Convert a row title to a filesystem/uid-safe slug."""
    title = title.lower()
    # Replace common separators and unicode dashes
    title = re.sub(r"[\s/\-–—]+", "-", title)
    # Strip anything that is not alphanumeric or hyphen
    title = re.sub(r"[^a-z0-9\-]", "", title)
    # Collapse multiple hyphens
    title = re.sub(r"-{2,}", "-", title)
    return title.strip("-")


# Manual slug overrides so filenames / uids are stable and human-friendly.
SLUG_OVERRIDES: dict[str, str] = {
    "tasks-executions": "tasks",
    "cost-observability": "cost",
    "model-performance": "model-performance",
    "scheduler-executor": "scheduler",
    "http-api": "http-api",
    "telegram": "telegram",
    "autonomy": "autonomy",
    "process": "process",
    "storage": "storage",
    "chat-llm": "chat-llm",
    "database": "database",
    "runtime-podman": "runtime-podman",
    "memory-rag": "memory-rag",
    "memory-ingest-pipeline-gates-quarantine": "memory-ingest",
    "memory-knowledge-graph-extraction-pipeline": "memory-kg",
    "trading-safety-envelope": "trading",
    "hallucination-llm-as-judge-phase-3": "hallucination-judge",
    "hallucination-phase-1-detector-signals": "hallucination-detector",
    "document-extraction": "doc-extraction",
    "instinct-continuous-learning": "instinct",
}


def make_base(source: dict, surface_title: str, uid: str) -> dict:
    """Construct a dashboard skeleton copying top-level fields from source."""
    return {
        "annotations": source.get("annotations", {"list": []}),
        "editable": True,
        "fiscalYearStartMonth": source.get("fiscalYearStartMonth", 0),
        "graphTooltip": source.get("graphTooltip", 1),
        "id": None,
        "links": [],
        "liveNow": False,
        "panels": [],
        "refresh": source.get("refresh", "30s"),
        "schemaVersion": source.get("schemaVersion", 39),
        "style": source.get("style", "dark"),
        "tags": ["vornik", "deep-dive"],
        "templating": source.get("templating", {"list": []}),
        "time": source.get("time", {"from": "now-30m", "to": "now"}),
        "timepicker": source.get("timepicker", {}),
        "timezone": source.get("timezone", ""),
        "title": f"vornik — {surface_title}",
        "uid": uid,
        "version": 1,
        "weekStart": source.get("weekStart", ""),
    }


def relayout(panels: list[dict], base_y: int) -> list[dict]:
    """Shift every panel's gridPos.y down so the surface starts at y=0."""
    result = []
    for p in panels:
        panel = dict(p)
        gp = dict(panel.get("gridPos", {}))
        gp["y"] = gp.get("y", 0) - base_y
        panel["gridPos"] = gp
        result.append(panel)
    return result


def build_cluster_dashboard(source: dict) -> dict:
    """
    Build the vornik-cluster dashboard from metrics that provably exist in
    the codebase (verified against internal/scheduler/metrics.go and
    internal/queue/metrics.go).

    Metrics used:
      up{job="vornik"}                                 — standard Prometheus liveness
      vornik_scheduler_leases_acquired_total           — scheduler/metrics.go
      vornik_scheduler_leases_expired_total            — scheduler/metrics.go
      vornik_scheduler_lease_duration_seconds_bucket   — scheduler/metrics.go
      vornik_queue_lease_total                         — queue/metrics.go

    NOTE: fleet membership, heartbeat freshness, relay health, and version
    skew are NOT in Prometheus today — they are surfaced via
    `vornikctl cluster status` and GET /api/v1/cluster.  Prometheus
    instrumentation for those signals is pending (deferred hardening per the
    cluster LLD).  This dashboard intentionally covers only the lease/liveness
    signals that ARE in Prometheus.
    """
    ds = {"type": "prometheus", "uid": "$datasource"}

    def ts_panel(pid, title, targets, gp, description=""):
        p = {
            "datasource": ds,
            "fieldConfig": {
                "defaults": {
                    "color": {"mode": "palette-classic"},
                    "custom": {
                        "drawStyle": "line",
                        "fillOpacity": 10,
                        "lineInterpolation": "smooth",
                        "lineWidth": 2,
                        "showPoints": "never",
                        "spanNulls": False,
                    },
                    "noValue": "0",
                },
                "overrides": [],
            },
            "gridPos": gp,
            "id": pid,
            "options": {
                "legend": {"displayMode": "list", "placement": "bottom", "showLegend": True},
                "tooltip": {"mode": "multi", "sort": "desc"},
            },
            "targets": targets,
            "title": title,
            "type": "timeseries",
        }
        if description:
            p["description"] = description
        return p

    def stat_panel(pid, title, targets, gp, description=""):
        p = {
            "datasource": ds,
            "fieldConfig": {
                "defaults": {
                    "color": {"mode": "thresholds"},
                    "thresholds": {
                        "mode": "absolute",
                        "steps": [
                            {"color": "red", "value": None},
                            {"color": "green", "value": 1},
                        ],
                    },
                    "mappings": [
                        {
                            "options": {
                                "0": {"color": "red", "text": "DOWN"},
                                "1": {"color": "green", "text": "UP"},
                            },
                            "type": "value",
                        }
                    ],
                    "noValue": "0",
                },
                "overrides": [],
            },
            "gridPos": gp,
            "id": pid,
            "options": {
                "colorMode": "background",
                "graphMode": "none",
                "reduceOptions": {"calcs": ["lastNotNull"]},
                "textMode": "value_and_name",
            },
            "targets": targets,
            "title": title,
            "type": "stat",
        }
        if description:
            p["description"] = description
        return p

    panels = []

    # ------------------------------------------------------------------ #
    # Row 1: Fleet Liveness
    # ------------------------------------------------------------------ #
    panels.append({
        "collapsed": False,
        "gridPos": {"h": 1, "w": 24, "x": 0, "y": 0},
        "id": 901,
        "title": "Fleet Liveness",
        "type": "row",
    })

    # Stat: daemon instances up
    panels.append(stat_panel(
        pid=902,
        title="Daemon Instances Up",
        targets=[{
            "expr": 'up{job="vornik"}',
            "legendFormat": "{{instance}}",
            "refId": "A",
        }],
        gp={"h": 6, "w": 24, "x": 0, "y": 1},
        description=(
            "Prometheus up{job=\"vornik\"} — 1=reachable, 0=scrape failed. "
            "Shows per-instance liveness. In a single-daemon deployment this "
            "is one series; in future multi-daemon deployments each instance "
            "appears separately."
        ),
    ))

    # Timeseries: liveness over time
    panels.append(ts_panel(
        pid=903,
        title="Daemon Liveness Over Time",
        targets=[{
            "expr": 'up{job="vornik"}',
            "legendFormat": "{{instance}}",
            "refId": "A",
        }],
        gp={"h": 8, "w": 24, "x": 0, "y": 7},
        description=(
            "Tracks scrape liveness over the selected time window. "
            "Drops to 0 indicate daemon restarts or network partitions."
        ),
    ))

    # ------------------------------------------------------------------ #
    # Row 2: Scheduler Lease Health
    # ------------------------------------------------------------------ #
    panels.append({
        "collapsed": False,
        "gridPos": {"h": 1, "w": 24, "x": 0, "y": 15},
        "id": 910,
        "title": "Scheduler Lease Health",
        "type": "row",
    })

    # Lease acquisition rate
    panels.append(ts_panel(
        pid=911,
        title="Lease Acquisition Rate",
        targets=[{
            "expr": "sum(rate(vornik_scheduler_leases_acquired_total[5m]))",
            "legendFormat": "acquired/s",
            "refId": "A",
        }],
        gp={"h": 8, "w": 12, "x": 0, "y": 16},
        description=(
            "Rate of task-lease acquisitions by the scheduler. "
            "vornik_scheduler_leases_acquired_total (internal/scheduler/metrics.go). "
            "Rising rate = healthy throughput; flat line while tasks are queued = "
            "scheduler may be wedged."
        ),
    ))

    # Lease expiry rate
    panels.append(ts_panel(
        pid=912,
        title="Lease Expiry Rate",
        targets=[{
            "expr": "sum(rate(vornik_scheduler_leases_expired_total[5m]))",
            "legendFormat": "expired/s",
            "refId": "A",
        }],
        gp={"h": 8, "w": 12, "x": 12, "y": 16},
        description=(
            "Rate of scheduler lease expirations. "
            "vornik_scheduler_leases_expired_total (internal/scheduler/metrics.go). "
            "Non-zero sustained rate indicates executors are dying before completing "
            "tasks — likely a sign of contention or OOM."
        ),
    ))

    # Lease acquisition latency
    panels.append({
        "datasource": ds,
        "fieldConfig": {
            "defaults": {
                "color": {"mode": "palette-classic"},
                "custom": {
                    "drawStyle": "line",
                    "fillOpacity": 10,
                    "lineInterpolation": "smooth",
                    "lineWidth": 2,
                    "showPoints": "never",
                    "spanNulls": False,
                },
                "unit": "s",
                "noValue": "0",
            },
            "overrides": [],
        },
        "gridPos": {"h": 8, "w": 12, "x": 0, "y": 24},
        "id": 913,
        "options": {
            "legend": {"displayMode": "list", "placement": "bottom", "showLegend": True},
            "tooltip": {"mode": "multi", "sort": "desc"},
        },
        "targets": [
            {
                "expr": "histogram_quantile(0.50, sum by (le) (rate(vornik_scheduler_lease_duration_seconds_bucket[5m])))",
                "legendFormat": "p50",
                "refId": "A",
            },
            {
                "expr": "histogram_quantile(0.95, sum by (le) (rate(vornik_scheduler_lease_duration_seconds_bucket[5m])))",
                "legendFormat": "p95",
                "refId": "B",
            },
            {
                "expr": "histogram_quantile(0.99, sum by (le) (rate(vornik_scheduler_lease_duration_seconds_bucket[5m])))",
                "legendFormat": "p99",
                "refId": "C",
            },
        ],
        "title": "Lease Acquisition Latency (p50/p95/p99)",
        "type": "timeseries",
        "description": (
            "Time to acquire a scheduler lease. "
            "vornik_scheduler_lease_duration_seconds_bucket (internal/scheduler/metrics.go). "
            "p99 spikes indicate DB contention under concurrent scheduler instances."
        ),
    })

    # Queue lease rate
    panels.append(ts_panel(
        pid=914,
        title="Queue Lease Rate",
        targets=[{
            "expr": "sum(rate(vornik_queue_lease_total[5m]))",
            "legendFormat": "queue leases/s",
            "refId": "A",
        }],
        gp={"h": 8, "w": 12, "x": 12, "y": 24},
        description=(
            "Rate of queue-level task leases. "
            "vornik_queue_lease_total (internal/queue/metrics.go). "
            "Cross-reference with scheduler lease acquisition rate — a divergence "
            "between the two surfaces dropped or double-leased tasks."
        ),
    ))

    # ------------------------------------------------------------------ #
    # Informational text panel
    # ------------------------------------------------------------------ #
    panels.append({
        "gridPos": {"h": 6, "w": 24, "x": 0, "y": 32},
        "id": 920,
        "options": {
            "content": (
                "## Cluster signals not yet in Prometheus\n\n"
                "The following are **not** instrumented in Prometheus today and are "
                "therefore intentionally absent from this dashboard:\n\n"
                "- **Fleet membership** (node count, peer list)\n"
                "- **Heartbeat freshness** (last-seen per daemon)\n"
                "- **Relay / MCP-broker health**\n"
                "- **Version skew** across daemon instances\n\n"
                "These signals are available via `vornikctl cluster status` and "
                "`GET /api/v1/cluster`. Prometheus instrumentation is pending "
                "(deferred hardening per the cluster LLD). This dashboard covers "
                "only the lease/liveness signals that **are** in Prometheus today."
            ),
            "mode": "markdown",
        },
        "title": "Cluster Observability Scope",
        "type": "text",
    })

    return {
        "annotations": source.get("annotations", {"list": []}),
        "editable": True,
        "fiscalYearStartMonth": source.get("fiscalYearStartMonth", 0),
        "graphTooltip": source.get("graphTooltip", 1),
        "id": None,
        "links": [],
        "liveNow": False,
        "panels": panels,
        "refresh": source.get("refresh", "30s"),
        "schemaVersion": source.get("schemaVersion", 39),
        "style": source.get("style", "dark"),
        "tags": ["vornik", "cluster", "leases"],
        "templating": source.get("templating", {"list": []}),
        "time": source.get("time", {"from": "now-30m", "to": "now"}),
        "timepicker": source.get("timepicker", {}),
        "timezone": source.get("timezone", ""),
        "title": "vornik — Cluster & Leases",
        "uid": "vornik-cluster",
        "version": 1,
        "weekStart": source.get("weekStart", ""),
    }


def main() -> None:
    with open(SOURCE) as f:
        source = json.load(f)

    all_panels: list[dict] = source["panels"]

    # ------------------------------------------------------------------ #
    # Group panels by row
    # ------------------------------------------------------------------ #
    # groups: list of (row_panel, child_panels)
    # "ungrouped" panels before the first row are collected but not emitted
    # (the monolith has none; guard anyway).
    groups: list[tuple[dict, list[dict]]] = []
    current_row: dict | None = None
    current_children: list[dict] = []

    for panel in all_panels:
        if panel.get("type") == "row":
            if current_row is not None:
                groups.append((current_row, current_children))
            current_row = panel
            current_children = []
        else:
            if current_row is not None:
                current_children.append(panel)
            # else: panels before first row — skip (none in this file)

    if current_row is not None:
        groups.append((current_row, current_children))

    # ------------------------------------------------------------------ #
    # Emit per-surface dashboards
    # ------------------------------------------------------------------ #
    total_child_panels = 0
    emitted: list[dict] = []  # for accounting report

    for row_panel, children in groups:
        row_title = row_panel.get("title", "Unknown")
        raw_slug = slugify(row_title)
        slug = SLUG_OVERRIDES.get(raw_slug, raw_slug)
        uid = f"vornik-{slug}"

        # base_y = y of the row panel itself; children start at row_y + 1
        row_y = row_panel.get("gridPos", {}).get("y", 0)
        # The first child's y is row_y + 1 at minimum; we shift so that
        # the first child lands at y=0.
        if children:
            min_child_y = min(p.get("gridPos", {}).get("y", row_y + 1) for p in children)
        else:
            min_child_y = row_y + 1

        relaid = relayout(children, min_child_y)
        total_child_panels += len(children)

        dash = make_base(source, row_title, uid)
        dash["panels"] = relaid

        out_path = DASHBOARDS_DIR / f"{slug}.json"
        with open(out_path, "w") as f:
            json.dump(dash, f, indent=2)
            f.write("\n")

        emitted.append({
            "title": row_title,
            "slug": slug,
            "uid": uid,
            "panel_count": len(children),
            "file": str(out_path),
        })
        print(f"  wrote {out_path.name}  ({len(children)} panels,  uid={uid})")

    # ------------------------------------------------------------------ #
    # Cluster & Leases dashboard
    # ------------------------------------------------------------------ #
    cluster = build_cluster_dashboard(source)
    cluster_path = DASHBOARDS_DIR / "cluster.json"
    with open(cluster_path, "w") as f:
        json.dump(cluster, f, indent=2)
        f.write("\n")
    cluster_panel_count = len(cluster["panels"])
    print(f"  wrote cluster.json  ({cluster_panel_count} panels,  uid=vornik-cluster)")

    # ------------------------------------------------------------------ #
    # Accounting report
    # ------------------------------------------------------------------ #
    total_source = len(all_panels)
    total_row_panels = sum(1 for p in all_panels if p.get("type") == "row")

    print()
    print("=" * 60)
    print("Panel accounting")
    print("=" * 60)
    print(f"  Source panels (total incl. rows): {total_source}")
    print(f"  Row panels dropped:               {total_row_panels}")
    print(f"  Non-row child panels:             {total_source - total_row_panels}")
    print(f"  Panels emitted to surface files:  {total_child_panels}")
    delta = (total_source - total_row_panels) - total_child_panels
    if delta == 0:
        print("  Conservation: OK — no panels lost")
    else:
        print(f"  Conservation: WARNING — delta={delta} (check script)")

    print()
    print("Per-surface dashboards:")
    for e in emitted:
        print(f"  {e['uid']:40s}  {e['panel_count']:3d} panels  -> {pathlib.Path(e['file']).name}")

    # UID uniqueness check
    all_uids = [e["uid"] for e in emitted] + ["vornik-cluster"]
    if len(all_uids) == len(set(all_uids)):
        print()
        print("UID uniqueness: OK — all uids are unique")
    else:
        from collections import Counter
        dupes = [u for u, c in Counter(all_uids).items() if c > 1]
        print(f"\nUID uniqueness: FAIL — duplicates: {dupes}")
        sys.exit(1)


if __name__ == "__main__":
    main()

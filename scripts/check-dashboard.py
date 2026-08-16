#!/usr/bin/env python3
"""Check the Grafana dashboard against the metrics Sluice actually exports.

A dashboard panel that queries a metric which no longer exists does not fail —
it renders an empty graph, which looks identical to "nothing is happening".
That is the worst possible failure for an observability asset: it is silent
precisely when someone is relying on it during an incident.

This walks every PromQL expression in the dashboard, extracts the metric names,
and compares them against the registrations in internal/app/metrics.go. Run it
in CI so renaming a metric breaks the build rather than the dashboard.

    python3 scripts/check-dashboard.py [dashboard.json]
    python3 scripts/check-dashboard.py --live http://localhost:3000

With --live, every panel expression is additionally executed through Grafana's
own datasource proxy — the same path the panels take — and any query returning
no series is reported. That catches the cases a static check cannot: a label
that never existed, a rate() window shorter than the scrape interval, a unit
mismatch that yields an empty vector.
"""

from __future__ import annotations

import json
import pathlib
import re
import sys
import urllib.error
import urllib.parse
import urllib.request

REPO = pathlib.Path(__file__).resolve().parent.parent
DASHBOARD = REPO / "deploy" / "grafana" / "dashboards" / "sluice.json"
METRICS_SRC = REPO / "internal" / "app" / "metrics.go"

# PromQL functions, keywords and aggregation operators that look like metric
# names to a naive regex.
PROMQL_RESERVED = {
    "sum", "avg", "min", "max", "count", "count_values", "stddev", "stdvar",
    "topk", "bottomk", "quantile", "group",
    "rate", "irate", "increase", "delta", "idelta", "deriv", "predict_linear",
    "changes", "resets", "abs", "ceil", "floor", "round", "clamp", "clamp_min",
    "clamp_max", "exp", "ln", "log2", "log10", "sqrt", "sgn", "timestamp",
    "histogram_quantile", "label_replace", "label_join", "absent",
    "absent_over_time", "vector", "scalar", "time", "day_of_week", "hour",
    "avg_over_time", "sum_over_time", "min_over_time", "max_over_time",
    "count_over_time", "quantile_over_time", "stddev_over_time", "last_over_time",
    "by", "without", "on", "ignoring", "group_left", "group_right", "offset",
    "and", "or", "unless", "bool", "le", "inf", "nan",
}

# Suffixes Prometheus derives from a histogram registration.
HISTOGRAM_SUFFIXES = ("_bucket", "_sum", "_count")


def registered_metrics(src: pathlib.Path) -> tuple[set[str], set[str]]:
    """Return (all exported names, histogram base names) from the Go source."""
    text = src.read_text(encoding="utf-8")
    names: set[str] = set()
    histograms: set[str] = set()

    for kind, name in re.findall(r"reg\.(Counter|Gauge|Histogram)\(\s*\"([a-z0-9_]+)\"", text):
        names.add(name)
        if kind == "Histogram":
            histograms.add(name)
            for suffix in HISTOGRAM_SUFFIXES:
                names.add(name + suffix)

    if not names:
        sys.exit(f"check-dashboard: found no metric registrations in {src} — has it moved?")
    return names, histograms


def expressions(node: object) -> list[str]:
    """Collect every `expr` string anywhere in the dashboard tree."""
    found: list[str] = []
    if isinstance(node, dict):
        for key, value in node.items():
            if key == "expr" and isinstance(value, str):
                found.append(value)
            else:
                found.extend(expressions(value))
    elif isinstance(node, list):
        for item in node:
            found.extend(expressions(item))
    return found


def metrics_in(expr: str) -> set[str]:
    """Extract candidate metric names from a PromQL expression."""
    # Strip label selectors and string literals first so their contents are not
    # mistaken for identifiers.
    stripped = re.sub(r"\{[^}]*\}", " ", expr)
    stripped = re.sub(r"\"[^\"]*\"", " ", stripped)

    candidates = set(re.findall(r"\b[a-z_][a-z0-9_]*\b", stripped))
    return {
        c for c in candidates
        if c not in PROMQL_RESERVED and c.startswith(("sluice_", "envoy_"))
    }


def resolve(expr: str) -> str:
    """Substitute dashboard template variables with match-everything values."""
    for var in ("route", "cloud", "backend", "region"):
        expr = expr.replace(f"${{{var}}}", ".*").replace(f"${var}", ".*")
    return expr


def run_live(base: str, dashboard: dict) -> int:
    """Execute every panel expression through Grafana's datasource proxy."""
    base = base.rstrip("/")

    def api(path: str, payload: dict | None = None) -> dict:
        url = f"{base}{path}"
        data = json.dumps(payload).encode() if payload is not None else None
        req = urllib.request.Request(
            url, data=data,
            headers={"Content-Type": "application/json", "Accept": "application/json"},
        )
        with urllib.request.urlopen(req, timeout=20) as resp:
            return json.loads(resp.read().decode())

    try:
        sources = api("/api/datasources")
    except urllib.error.URLError as exc:
        sys.exit(f"check-dashboard: cannot reach Grafana at {base}: {exc}")

    prom = next((s for s in sources if s["type"] == "prometheus"), None)
    if prom is None:
        sys.exit("check-dashboard: no Prometheus datasource is provisioned")
    uid = prom["uid"]
    print(f"\nlive check via {base} -> datasource {uid}")

    empty: list[tuple[str, str]] = []
    failed: list[tuple[str, str, str]] = []
    checked = 0

    for panel in dashboard["panels"]:
        if panel["type"] == "row":
            continue
        for target in panel.get("targets", []):
            expr = target.get("expr")
            if not expr:
                continue
            checked += 1
            query = urllib.parse.quote(resolve(expr), safe="")
            try:
                out = api(f"/api/datasources/proxy/uid/{uid}/api/v1/query?query={query}")
            except Exception as exc:  # noqa: BLE001 - report, do not abort the sweep
                failed.append((panel["title"], expr, str(exc)))
                continue
            if out.get("status") != "success":
                failed.append((panel["title"], expr, out.get("error", "unknown error")))
            elif not out["data"]["result"]:
                empty.append((panel["title"], expr))

    print(f"executed  : {checked} queries")

    if failed:
        print("\nqueries Prometheus rejected:")
        for title, expr, err in failed:
            print(f"  {title}\n      {expr}\n      {err}")

    if empty:
        # Not always a defect — a counter that has never incremented is
        # legitimately empty on a quiet system — but it is always worth a look,
        # because it is indistinguishable from a broken query on the panel.
        print("\nqueries returning no series (verify these are genuinely idle):")
        for title, expr in empty:
            print(f"  {title}\n      {expr}")

    return 1 if failed else 0


def main() -> int:
    argv = sys.argv[1:]
    live_base = None
    if "--live" in argv:
        i = argv.index("--live")
        live_base = argv[i + 1] if len(argv) > i + 1 else "http://localhost:3000"
        del argv[i:i + 2]

    path = pathlib.Path(argv[0]) if argv else DASHBOARD

    try:
        raw = path.read_bytes().decode("utf-8")
    except UnicodeDecodeError as exc:
        sys.exit(f"check-dashboard: {path} is not valid UTF-8: {exc}")
    try:
        dashboard = json.loads(raw)
    except json.JSONDecodeError as exc:
        sys.exit(f"check-dashboard: {path} is not valid JSON: {exc}")

    known, histograms = registered_metrics(METRICS_SRC)

    exprs = expressions(dashboard)
    if not exprs:
        sys.exit("check-dashboard: the dashboard contains no queries")

    unknown: dict[str, list[str]] = {}
    used: set[str] = set()

    for expr in exprs:
        for metric in metrics_in(expr):
            # Envoy's own series are not Sluice's to verify.
            if metric.startswith("envoy_"):
                continue
            used.add(metric)
            if metric not in known:
                unknown.setdefault(metric, []).append(expr.strip())

    # A histogram queried without a suffix returns nothing, silently.
    misuse = [
        m for m in used
        if m in histograms and not any(
            f"{m}{s}" in " ".join(exprs) for s in HISTOGRAM_SUFFIXES
        )
    ]

    print(f"dashboard : {path.relative_to(REPO)}")
    print(f"queries   : {len(exprs)}")
    print(f"metrics   : {len(used)} referenced, {len(known)} exported")

    ok = True

    if unknown:
        ok = False
        print("\nqueries reference metrics that are not exported:")
        for metric, where in sorted(unknown.items()):
            print(f"  {metric}")
            for expr in where[:2]:
                print(f"      {expr}")

    if misuse:
        ok = False
        print("\nhistograms queried without a _bucket/_sum/_count suffix "
              "(these return no data):")
        for metric in sorted(misuse):
            print(f"  {metric}")

    # Exported metrics with no panel are not an error, but they are worth
    # knowing about — usually it means a dashboard was not updated alongside
    # new instrumentation.
    #
    # A histogram counts as used when any of its derived series is queried;
    # nothing ever queries the bare base name, so comparing against that
    # directly would report every histogram as unused.
    def is_used(metric: str) -> bool:
        if metric in used:
            return True
        if metric in histograms:
            return any(metric + suffix in used for suffix in HISTOGRAM_SUFFIXES)
        return False

    unused = sorted(
        m for m in known
        if not is_used(m)
        and not m.endswith(HISTOGRAM_SUFFIXES)
        and m != "sluice_build_info"
    )
    if unused:
        print(f"\nexported but not on the dashboard ({len(unused)}):")
        for metric in unused:
            print(f"  {metric}")

    if live_base:
        if run_live(live_base, dashboard) != 0:
            ok = False

    print("\nOK" if ok else "\nFAILED")
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())

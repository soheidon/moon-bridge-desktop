#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.10"
# dependencies = ["httpx", "rich"]
# ///
"""Analyze MoonBridge metrics with cache usage breakdown.

Fetches per-request records from GET /v1/admin/metrics, aggregates per-model
stats, and renders three focused tables: Usage, Cache Analysis, SLB.

Usage:
  BASE_URL=http://127.0.0.1:38440 API_KEY=sk-... python3 scripts/dev/metrics_analyze.py
  # or with uv:
  BASE_URL=http://127.0.0.1:38440 uv run scripts/dev/metrics_analyze.py

Environment variables:
  BASE_URL   MoonBridge server base URL (required)
  API_KEY    Bearer token for authentication (optional if auth is disabled)
  LIMIT      Max records per page (default: 1000)
  MODEL      Filter by model name (optional)
  SINCE      Filter by start time, RFC3339 (optional)
  UNTIL      Filter by end time, RFC3339 (optional)
"""

from __future__ import annotations

import os
import sys
from dataclasses import dataclass

import httpx
from rich.console import Console
from rich.table import Table
from rich.text import Text


@dataclass
class ModelStats:
    requests: int = 0
    input_tokens: int = 0
    output_tokens: int = 0
    cache_read: int = 0
    cache_creation: int = 0
    cost: float = 0.0
    total_response_time_ms: int = 0
    errors: int = 0

    @property
    def fresh_input(self) -> int:
        v = self.input_tokens - self.cache_read - self.cache_creation
        return v if v > 0 else 0

    @property
    def total_tokens(self) -> int:
        return self.input_tokens + self.output_tokens


# ── Data fetching ──

def fetch_all_records(
    base_url: str,
    api_key: str | None,
    *,
    limit: int = 1000,
    model: str | None = None,
    since: str | None = None,
    until: str | None = None,
) -> list[dict]:
    url = f"{base_url.rstrip('/')}/v1/admin/metrics"
    headers: dict[str, str] = {}
    if api_key:
        headers["Authorization"] = f"Bearer {api_key}"

    all_records: list[dict] = []
    offset = 0
    while True:
        params: dict[str, str | int] = {"limit": limit, "offset": offset}
        if model:
            params["model"] = model
        if since:
            params["since"] = since
        if until:
            params["until"] = until

        resp = httpx.get(url, params=params, headers=headers, timeout=30)
        resp.raise_for_status()
        data = resp.json()
        records = data.get("records", [])
        all_records.extend(records)
        if len(records) < limit:
            break
        offset += limit
    return all_records


def aggregate(records: list[dict]) -> tuple[dict[str, ModelStats], ModelStats]:
    by_model: dict[str, ModelStats] = {}
    total = ModelStats()
    for r in records:
        model = r.get("actual_model") or r.get("model", "?")
        ms = by_model.setdefault(model, ModelStats())
        _update(ms, r)
        _update(total, r)
    return by_model, total


def _update(ms: ModelStats, r: dict) -> None:
    ms.requests += 1
    ms.input_tokens += r.get("input_tokens", 0)
    ms.output_tokens += r.get("output_tokens", 0)
    ms.cache_read += r.get("cache_read", 0)
    ms.cache_creation += r.get("cache_creation", 0)
    ms.cost += r.get("cost", 0.0)
    ms.total_response_time_ms += r.get("response_time", 0) // 1_000_000  # Go time.Duration ns→ms
    if r.get("status") != "success":
        ms.errors += 1


# ── Formatting ──

def fmt_tok(n: int) -> str:
    if n >= 1_000_000_000:
        return f"{n / 1_000_000_000:.2f}B"
    if n >= 1_000_000:
        return f"{n / 1_000_000:.2f}M"
    if n >= 1_000:
        return f"{n / 1_000:.1f}K"
    return str(n)


def fmt_cost(v: float) -> str:
    return f"¥{v:.4f}" if v else "¥0"


def fmt_pct(v: float, *, invert: bool = False) -> Text:
    """Format percentage. By default green=high; set invert=True for metrics
    where lower is better (like fresh% = wasted cache potential)."""
    s = f"{v:.1f}%"
    if invert:
        if v <= 20:
            return Text(s, style="bold green")
        if v <= 50:
            return Text(s, style="yellow")
        return Text(s, style="red")
    if v >= 80:
        return Text(s, style="bold green")
    if v >= 40:
        return Text(s, style="yellow")
    return Text(s, style="red")


def fmt_ratio(v: float | None) -> Text:
    if v is None:
        return Text("N/A", style="dim")
    s = f"{v:.2f}"
    if v >= 4:
        return Text(s, style="bold green")
    if v >= 1:
        return Text(s, style="green")
    return Text(s, style="yellow")


def fmt_unit_cost(v: float | None) -> Text:
    if v is None:
        return Text("N/A", style="dim")
    s = f"¥{v:.2f}"
    if v <= 1:
        return Text(s, style="bold green")
    if v <= 5:
        return Text(s, style="yellow")
    return Text(s, style="red")


def fmt_latency(ms: ModelStats) -> Text:
    avg = ms.total_response_time_ms / ms.requests if ms.requests > 0 else 0
    if avg <= 0:
        return Text("N/A", style="dim")
    if avg >= 1000:
        s = f"{avg / 1000:.1f}s"
    else:
        s = f"{avg:.0f}ms"
    if avg < 2000:
        return Text(s, style="green")
    if avg < 10000:
        return Text(s, style="yellow")
    return Text(s, style="red")


# ── Table builders ──

def _sorted(by_model: dict[str, ModelStats]) -> list[tuple[str, ModelStats]]:
    return sorted(by_model.items(), key=lambda x: x[1].cost, reverse=True)


def build_usage_table(by_model: dict[str, ModelStats], total: ModelStats) -> Table:
    """Table 1: token volume and cost per model."""
    t = Table(title="Usage", expand=True, show_lines=False)
    t.add_column("Model", style="cyan", no_wrap=True)
    t.add_column("Req", justify="right")
    t.add_column("Fresh", justify="right")
    t.add_column("Cache R", justify="right", style="green")
    t.add_column("Cache W", justify="right", style="dim")
    t.add_column("Output", justify="right")
    t.add_column("Cost", justify="right", style="yellow")

    for model, ms in _sorted(by_model):
        t.add_row(
            model, str(ms.requests),
            fmt_tok(ms.fresh_input), fmt_tok(ms.cache_read),
            fmt_tok(ms.cache_creation), fmt_tok(ms.output_tokens),
            fmt_cost(ms.cost),
        )

    if len(by_model) > 1:
        t.add_section()
        t.add_row(
            Text("TOTAL", style="bold"),
            Text(str(total.requests), style="bold"),
            fmt_tok(total.fresh_input), fmt_tok(total.cache_read),
            fmt_tok(total.cache_creation), fmt_tok(total.output_tokens),
            fmt_cost(total.cost),
        )
    return t


def build_cache_table(by_model: dict[str, ModelStats], total: ModelStats) -> Table:
    """Table 2: cache efficiency metrics."""
    t = Table(title="Cache Analysis", expand=True, show_lines=False)
    t.add_column("Model", style="cyan", no_wrap=True)
    t.add_column("Hit%", justify="right")
    t.add_column("R/W", justify="right")
    t.add_column("Fresh%", justify="right")
    t.add_column("Cache R%", justify="right")
    t.add_column("Cache W%", justify="right")

    for model, ms in _sorted(by_model):
        inp = ms.input_tokens
        fresh = ms.fresh_input
        hit = (ms.cache_read / inp * 100) if inp > 0 else 0
        rw = (ms.cache_read / ms.cache_creation) if ms.cache_creation > 0 else None
        fresh_pct = (fresh / inp * 100) if inp > 0 else 0
        cr_pct = (ms.cache_read / inp * 100) if inp > 0 else 0
        cw_pct = (ms.cache_creation / inp * 100) if inp > 0 else 0

        t.add_row(
            model,
            fmt_pct(hit),
            fmt_ratio(rw),
            fmt_pct(fresh_pct, invert=True),
            fmt_pct(cr_pct),
            fmt_pct(cw_pct, invert=True),
        )

    if len(by_model) > 1:
        inp = total.input_tokens
        fresh = total.fresh_input
        hit = (total.cache_read / inp * 100) if inp > 0 else 0
        rw = (total.cache_read / total.cache_creation) if total.cache_creation > 0 else None
        fresh_pct = (fresh / inp * 100) if inp > 0 else 0
        cr_pct = (total.cache_read / inp * 100) if inp > 0 else 0
        cw_pct = (total.cache_creation / inp * 100) if inp > 0 else 0

        t.add_section()
        t.add_row(
            Text("TOTAL", style="bold"),
            fmt_pct(hit),
            fmt_ratio(rw),
            fmt_pct(fresh_pct, invert=True),
            fmt_pct(cr_pct),
            fmt_pct(cw_pct, invert=True),
        )
    return t


def build_sla_table(by_model: dict[str, ModelStats], total: ModelStats) -> Table:
    """Table 3: service-level / cost-efficiency metrics."""
    t = Table(title="SLA", expand=True, show_lines=False)
    t.add_column("Model", style="cyan", no_wrap=True)
    t.add_column("Req", justify="right")
    t.add_column("¥/MTok", justify="right")
    t.add_column("Lat.", justify="right")
    t.add_column("Err", justify="right", style="red")
    t.add_column("Err%", justify="right")

    for model, ms in _sorted(by_model):
        unit = (ms.cost / ms.total_tokens * 1_000_000) if ms.total_tokens > 0 else None
        err_rate = ms.errors / ms.requests * 100 if ms.requests > 0 else 0
        t.add_row(
            model,
            str(ms.requests),
            fmt_unit_cost(unit),
            fmt_latency(ms),
            Text(str(ms.errors), style="red") if ms.errors > 0 else Text("0", style="dim"),
            fmt_pct(err_rate, invert=True),
        )

    if len(by_model) > 1:
        unit = (total.cost / total.total_tokens * 1_000_000) if total.total_tokens > 0 else None
        err_rate = total.errors / total.requests * 100 if total.requests > 0 else 0
        t.add_section()
        t.add_row(
            Text("TOTAL", style="bold"),
            Text(str(total.requests), style="bold"),
            fmt_unit_cost(unit),
            fmt_latency(total),
            Text(str(total.errors), style="bold red") if total.errors > 0 else Text("0", style="bold dim"),
            fmt_pct(err_rate, invert=True),
        )
    return t


def main() -> None:
    base_url = os.environ.get("BASE_URL", "")
    if not base_url:
        print("Error: set BASE_URL environment variable.", file=sys.stderr)
        sys.exit(1)

    api_key = os.environ.get("API_KEY") or None
    limit = int(os.environ.get("LIMIT", "1000"))
    model = os.environ.get("MODEL") or None
    since = os.environ.get("SINCE") or None
    until = os.environ.get("UNTIL") or None

    console = Console()

    try:
        records = fetch_all_records(
            base_url, api_key,
            limit=limit, model=model, since=since, until=until,
        )
    except httpx.HTTPStatusError as e:
        console.print(f"[red]HTTP {e.response.status_code}:[/] {e.response.text}")
        sys.exit(1)
    except httpx.ConnectError as e:
        console.print(f"[red]Connection failed:[/] {e}")
        sys.exit(1)

    if not records:
        console.print("[dim]No metrics records found.[/]")
        return

    by_model, total = aggregate(records)

    console.print(build_usage_table(by_model, total))
    console.print()
    console.print(build_cache_table(by_model, total))
    console.print()
    console.print(build_sla_table(by_model, total))
    console.print(f"\n[dim]{len(records)} records from {base_url}[/]")


if __name__ == "__main__":
    main()

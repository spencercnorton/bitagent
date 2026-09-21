"""Pure Prometheus text-format parsing + numeric validation helpers.

Extracted verbatim from app.py so the stats/telemetry plumbing is easier to
navigate. No app state, no I/O — just parsing and validation. The telemetry-
envelope helper (_metric_from_probe) and the history-touching rate helpers stay
in app.py because they depend on app-level types / module globals.
"""
from __future__ import annotations

import math
import re

# Matches `label="value"` pairs inside a Prometheus series' `{...}` label block.
_PROM_LABEL_RE = re.compile(r'(\w+)="([^"]*)"')


def _parse_prometheus_snapshot(raw: str) -> dict[str, object]:
    """Parse a Prometheus text snapshot without inventing absent series."""
    metric_sum: dict[str, float] = {}
    metric_lines: dict[str, float] = {}
    for line in raw.splitlines():
        if not line or line.startswith("#"):
            continue
        parts = line.split()
        if len(parts) < 2:
            continue
        key, raw_value = parts[0], parts[1]
        bare = key.split("{", 1)[0]
        try:
            value = float(raw_value)
        except ValueError:
            continue
        if not math.isfinite(value):
            continue
        metric_sum[bare] = metric_sum.get(bare, 0.0) + value
        metric_lines[key] = value
    return {"raw": raw, "sum": metric_sum, "lines": metric_lines}


def _nonnegative_int(value) -> int | None:
    if value is None or isinstance(value, bool):
        return None
    try:
        number = float(value)
    except (TypeError, ValueError):
        return None
    if not math.isfinite(number) or number < 0 or not number.is_integer():
        return None
    return int(number)


def _nonnegative_rate(value) -> float | None:
    """Preserve a finite non-negative rate, including sub-unit activity."""
    if value is None or isinstance(value, bool):
        return None
    try:
        number = float(value)
    except (TypeError, ValueError):
        return None
    if not math.isfinite(number) or number < 0:
        return None
    return number


def _metric_family_present(metric_lines: dict[str, float], name: str) -> bool:
    return any(key == name or key.startswith(name + "{") for key in metric_lines)


def _labeled_metric_sum(
    metric_lines: dict[str, float],
    name: str,
    **wanted_labels: str,
) -> float | None:
    if not _metric_family_present(metric_lines, name):
        return None
    total = 0.0
    matched = False
    for key, value in metric_lines.items():
        bare, separator, label_blob = key.partition("{")
        if bare != name:
            continue
        labels = {
            match.group(1): match.group(2)
            for match in _PROM_LABEL_RE.finditer(label_blob if separator else "")
        }
        if all(labels.get(label) == expected for label, expected in wanted_labels.items()):
            total += value
            matched = True
    return total if matched else None


# ── LLM spend ─────────────────────────────────────────────────────────
#
# The core emits provider-reported token counts and says so in its own help
# text: "This is the ONLY token accounting bitagent emits; cost is derived
# downstream where the per-model price table lives." Downstream is here.

# Provider list price in USD per million tokens, as (input, output).
#
# OpenRouter /models prices verified 2026-09-14. Direct Nano pricing:
# https://developers.openai.com/api/docs/models/gpt-5.4-nano
# Upgrade path when it drifts: fetch https://openrouter.ai/api/v1/models on a
# daily timer and cache it. Deliberately NOT a dict-with-default — a model that
# is not listed is reported as unpriced, never as $0, because a changed model
# pin must not make the bill read as free.
LLM_PRICES_USD_PER_MTOK: dict[str, tuple[float, float]] = {
    "google/gemma-3-12b-it": (0.05, 0.15),
    "meta-llama/llama-3.3-70b-instruct": (0.10, 0.32),
    "gpt-5.4-nano": (0.20, 1.25),
    # Matcher route pins azure/us with no fallback. This is the regional
    # endpoint price, not OpenRouter's cheaper unpinned model-list minimum.
    "openai/gpt-5.4-nano-20260317": (0.22, 1.375),
}
LLM_CACHED_INPUT_USD_PER_MTOK = {"gpt-5.4-nano": 0.02, "openai/gpt-5.4-nano-20260317": 0.022}
LLM_PRICES_AS_OF = "2026-09-14"

# (family, stage id, label carrying the direction, input value, output value).
# The two emitting stages disagree on label names — the content filter says
# kind=prompt|completion, junk purge says type=input|output — so each family
# carries its own mapping instead of the caller guessing. junk purge also
# labels cached_input/reasoning, which its help text warns are SUBSETS of
# input/output; matching the exact input/output values excludes them, so
# nothing is double-counted.
_LLM_TOKEN_FAMILIES: tuple[tuple[str, str, str, str, str], ...] = (
    ("bitagent_contentfilter_llm_tokens_total", "contentfilter", "kind", "prompt", "completion"),
    ("bitagent_junkpurge_llm_tokens_total", "junkpurge", "type", "input", "output"),
    ("bitagent_classifier_llm_match_tokens_total", "matcher", "kind", "input", "output"),
)


def _iter_labeled(metric_lines: dict[str, float], name: str):
    """Yield (labels, value) for every series in one metric family."""
    for key, value in metric_lines.items():
        bare, separator, label_blob = key.partition("{")
        if bare != name:
            continue
        yield {
            match.group(1): match.group(2)
            for match in _PROM_LABEL_RE.finditer(label_blob if separator else "")
        }, value


def _llm_spend(metric_lines: dict[str, float]) -> dict[str, object]:
    """Derive USD spend per model from the provider-reported token counters.

    Counters are cumulative since core boot, so `totalUsd` is spend-to-date,
    not a rate; the caller measures a rate by sampling this over time.

    A model with tokens but no price entry contributes its tokens and no
    dollars, and is named in `unpricedModels` with `totalIsPartial` set — the
    total is then a floor, and the UI must say so rather than present it as
    the bill.
    """
    by_model: dict[tuple[str, str], dict] = {}
    for family, stage, direction_label, input_value, output_value in _LLM_TOKEN_FAMILIES:
        if not _metric_family_present(metric_lines, family):
            continue
        for labels, value in _iter_labeled(metric_lines, family):
            model = labels.get("model") or ""
            direction = labels.get(direction_label)
            if direction not in (input_value, output_value, "cached_input"):
                continue  # cached_input / reasoning are subsets — see above
            row = by_model.setdefault(
                (model, stage),
                {"model": model, "stage": stage, "inputTokens": 0.0, "outputTokens": 0.0, "cachedInputTokens": 0.0},
            )
            key = ("cachedInputTokens" if direction == "cached_input" else
                   "inputTokens" if direction == input_value else "outputTokens")
            row[key] += value

    for labels, value in _iter_labeled(metric_lines, "bitagent_classifier_llm_match_usage_missing_total"):
        if value <= 0:
            continue
        model = labels.get("model") or ""
        row = by_model.setdefault((model, "matcher"), {
            "model": model, "stage": "matcher", "inputTokens": 0.0,
            "outputTokens": 0.0, "cachedInputTokens": 0.0,
        })
        row["missingUsageResponses"] = row.get("missingUsageResponses", 0) + int(value)

    rows: list[dict] = []
    total = 0.0
    unpriced: list[str] = []
    for row in by_model.values():
        price = LLM_PRICES_USD_PER_MTOK.get(row["model"]) if row["model"] else None
        if price is None:
            row["usd"] = None
            row["priced"] = False
            # A series with no `model` label is unpriceable for a different
            # reason than an unrecognised model, but it fails the same way:
            # real tokens, no dollars. Naming it keeps the total honest —
            # dropping it because the label is empty would let the priced
            # subtotal render as the complete bill.
            label = row["model"] or "unlabelled"
            if label not in unpriced:
                unpriced.append(label)
        else:
            usd = (row["inputTokens"] * price[0] + row["outputTokens"] * price[1]) / 1_000_000
            cached_price = LLM_CACHED_INPUT_USD_PER_MTOK.get(row["model"])
            if cached_price is not None:
                cached = min(row["inputTokens"], row["cachedInputTokens"])
                usd -= cached * (price[0] - cached_price) / 1_000_000
            row["usd"] = usd
            row["priced"] = True
            total += usd
        row["inputTokens"] = int(row["inputTokens"])
        row["outputTokens"] = int(row["outputTokens"])
        row["cachedInputTokens"] = int(row["cachedInputTokens"])
        row["usageIncomplete"] = bool(row.get("missingUsageResponses"))
        rows.append(row)

    rows.sort(key=lambda r: (r["usd"] is None, -(r["usd"] or 0.0), r["model"]))
    return {
        "available": bool(rows),
        "totalUsd": total if rows else None,
        # Derived from the rows themselves, not from the name list — the two
        # can only agree by accident, and this one is what the UI's "≥" floor
        # actually depends on.
        "totalIsPartial": any(not r["priced"] or r["usageIncomplete"] for r in rows),
        "byModel": rows,
        "unpricedModels": sorted(unpriced),
        "pricesAsOf": LLM_PRICES_AS_OF,
    }

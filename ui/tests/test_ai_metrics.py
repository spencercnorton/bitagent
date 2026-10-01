"""AI contracts against synthetic versions of the actual emitted families.

Ratios describe operations, not labelled accuracy. Source absence, missing
usage, and unknown prices must never look like measured zero or free calls.
"""
from __future__ import annotations

import httpx
import pytest

import app as app_module
import config
import graphql_client as gql
from prom_metrics import _llm_spend, _parse_prometheus_snapshot


# Schemas copied from internal/classifier/{llmmatch,llmstage}/metrics.go,
# internal/classifier/contentfilter/metrics.go and internal/junkpurge/metrics.go.
# All activity, models, and counters here are synthetic.
EMITTED_AI_METRICS = '''
bitmagnet_process_start_time_seconds 1700000000
bitagent_classifier_llm_match_config{setting="enabled"} 1
bitagent_classifier_llm_match_config{setting="live"} 1
bitagent_classifier_llm_match_config{setting="require_source_title"} 1
bitagent_classifier_llm_match_config{setting="daily_call_limit"} 20
bitagent_classifier_llm_match_config{setting="monthly_call_limit"} 600
bitagent_classifier_llm_match_info{model="gpt-5.4-nano",prompt_version="synthetic"} 1
bitagent_classifier_llm_match_calls_total{model="gpt-5.4-nano",stage="extract"} 8
bitagent_classifier_llm_match_calls_total{model="gpt-5.4-nano",stage="rerank"} 4
bitagent_classifier_llm_match_extract_total{result="ok"} 10
bitagent_classifier_llm_match_rerank_total{result="match"} 4
bitagent_classifier_llm_match_matches_total{mode="live",media_type="movie"} 3
bitagent_classifier_llm_match_matches_total{mode="shadow",media_type="tv"} 1
bitagent_classifier_llm_match_gate_rejects_total{gate="candidate_year"} 1
bitagent_classifier_llm_match_cache_hits_total 2
bitagent_classifier_llm_match_cache_misses_total 12
bitagent_classifier_llm_match_tokens_total{model="gpt-5.4-nano",stage="extract",kind="input"} 10000
bitagent_classifier_llm_match_tokens_total{model="gpt-5.4-nano",stage="extract",kind="output"} 1000
bitagent_classifier_llm_match_call_duration_seconds_sum{stage="extract"} 12
bitagent_classifier_llm_match_call_duration_seconds_count{stage="extract"} 8
bitagent_classifier_llm_match_call_duration_seconds_sum{stage="rerank"} 8
bitagent_classifier_llm_match_call_duration_seconds_count{stage="rerank"} 4
bitagent_contentfilter_examined_total 50
bitagent_contentfilter_llm_calls_total{ok="true"} 6
bitagent_contentfilter_llm_calls_total{ok="false"} 1
bitagent_contentfilter_llm_tokens_total{model="gpt-5.4-nano",kind="input"} 20000
bitagent_contentfilter_llm_tokens_total{model="gpt-5.4-nano",kind="output"} 2000
bitagent_contentfilter_llm_tokens_total{model="gpt-5.4-nano",kind="cached_input"} 10000
bitagent_contentfilter_llm_tokens_total{model="gpt-5.4-nano",kind="reasoning"} 500
bitagent_contentfilter_llm_usage_missing_total{model="gpt-5.4-nano"} 1
bitagent_contentfilter_llm_cache_hits_total 3
bitagent_contentfilter_llm_cache_misses_total 7
bitagent_contentfilter_llm_call_duration_seconds_sum 14
bitagent_contentfilter_llm_call_duration_seconds_count 7
bitagent_contentfilter_llm_deferred_total 0
bitagent_contentfilter_llm_budget_exhausted_total 1
bitagent_contentfilter_drop_total{reason="llm_non_english"} 2
bitagent_contentfilter_drop_total{reason="blocked_extension"} 4
bitagent_contentfilter_would_drop_total{reason="blocked_extension"} 8
bitagent_junkpurge_llm_requests_total{processing="standard",model="synthetic/unpriced-judge",outcome="ok"} 3
bitagent_junkpurge_llm_tokens_total{processing="standard",model="synthetic/unpriced-judge",type="input"} 3000
bitagent_junkpurge_llm_tokens_total{processing="standard",model="synthetic/unpriced-judge",type="output"} 300
bitagent_junkpurge_judged_total{verdict="junk"} 2
bitagent_junkpurge_judged_total{verdict="unsure"} 1
bitagent_junkpurge_quarantined_total 0
bitagent_junkpurge_would_delete_total 2
bitagent_junkpurge_cycles_total 1
bitagent_junkpurge_cycle_duration_seconds_sum 5
bitagent_junkpurge_cycle_duration_seconds_count 1
bitagent_classifier_llm_config{setting="enabled"} 1
bitagent_classifier_llm_config{setting="live"} 0
bitagent_classifier_llm_config{setting="min_confidence"} 0.9
bitagent_classifier_llm_config{setting="daily_call_limit"} 20
bitagent_classifier_llm_config{setting="monthly_call_limit"} 600
bitagent_classifier_llm_calls_total{model="gpt-5.4-nano"} 2
bitagent_classifier_llm_tokens_total{model="gpt-5.4-nano",kind="input"} 5000
bitagent_classifier_llm_tokens_total{model="gpt-5.4-nano",kind="output"} 500
bitagent_classifier_llm_tokens_total{model="gpt-5.4-nano",kind="cached_input"} 0
bitagent_classifier_llm_tokens_total{model="gpt-5.4-nano",kind="reasoning"} 100
bitagent_classifier_llm_usage_missing_total{model="gpt-5.4-nano"} 0
bitagent_classifier_llm_decisions_total{media_type="movie"} 2
bitagent_classifier_llm_live_applied_total{media_type="movie"} 0
bitagent_classifier_llm_shadow_skipped_total 2
bitagent_classifier_llm_gate_rejects_total{reason="budget_exhausted"} 1
bitagent_classifier_llm_call_errors_total{class="schema"} 0
bitagent_classifier_llm_cache_hits_total 1
bitagent_classifier_llm_cache_misses_total 2
bitagent_classifier_llm_call_duration_seconds_sum 3
bitagent_classifier_llm_call_duration_seconds_count 2
'''


@pytest.fixture(autouse=True)
def isolated_ai(monkeypatch):
    monkeypatch.setattr(config.settings, "require_auth", False)
    monkeypatch.setattr(app_module, "_SPEND_CORE_STARTED_AT", None)
    app_module._SPEND_HISTORY.clear()
    yield
    app_module._SPEND_HISTORY.clear()


def metrics(monkeypatch, text):
    async def read():
        return text
    monkeypatch.setattr(gql, "fetch_metrics", read)


def stage(body, name):
    return next(item for item in body["stages"] if item["id"] == name)


def metric(item, name):
    return next(value for value in item["metrics"] if value["label"] == name)


def spend(text):
    return _llm_spend(_parse_prometheus_snapshot(text)["lines"])


def test_actual_four_stage_schemas_and_provider_token_directions(client, monkeypatch):
    metrics(monkeypatch, EMITTED_AI_METRICS)
    body = client.get("/api/ai/summary").json()
    assert [item["id"] for item in body["stages"]] == ["matcher", "contentfilter", "junkpurge", "typeclassifier"]
    assert body["telemetry"]["status"] == "ok"
    assert body["telemetry"]["observedAt"] and body["telemetry"]["stale"] is False
    assert metric(stage(body, "matcher"), "Calls")["value"] == 12
    cf = stage(body, "contentfilter")
    assert cf["tokens"] == {"input": 20000, "output": 2000}
    assert cf["spend"]["usd"] == pytest.approx(0.0047)
    assert cf["spend"]["totalIsPartial"] and cf["spend"]["monthlyIsPartial"]
    assert "incomplete" in cf["tokensNote"]
    tc = stage(body, "typeclassifier")
    assert tc["status"]["label"] == "Shadow configured"
    assert tc["status"]["basis"] == "resolved configuration"
    assert tc["config"]["min_confidence"] == 0.9
    assert metric(tc, "Calls")["value"] == 2
    assert tc["tokens"] == {"input": 5000, "output": 500}
    assert tc["spend"]["usd"] == pytest.approx(0.001625)
    assert tc["gateRejects"] == {"budget_exhausted": 1}
    quarantine = metric(stage(body, "junkpurge"), "Quarantined")
    assert quarantine["value"] == 0
    assert quarantine["detail"] == "quarantines since core boot; expiry counter not instrumented"
    for item in body["stages"]:
        assert item["quality"]["accuracy"] is None
        assert item["quality"]["calibrated"] is None
        assert item["quality"]["groundTruthAvailable"] is False
        assert "0.43%" not in str(item) and "half of these" not in str(item)


@pytest.mark.parametrize("text,status", [("", "unavailable"), ("# HELP x description\n", "unavailable"),
    ("not a metric\n", "error"), ("bitagent_classifier_llm_match_extract_total{result=\"ok\"} NaN\n", "error"),
    ("bitagent_classifier_llm_match_extract_total{result=\"ok\"} -1\n", "error"),
    ("bitagent_classifier_llm_match_extract_total{result=\"ok\"} 1.5\n", "error")])
def test_source_failure_never_invents_counts_or_retains_previous_values(client, monkeypatch, text, status):
    metrics(monkeypatch, EMITTED_AI_METRICS)
    assert client.get("/api/ai/summary").json()["spend"]["available"]
    metrics(monkeypatch, text)
    body = client.get("/api/ai/summary").json()
    assert body["telemetry"]["status"] == status
    assert body["telemetry"]["observedAt"] is None and body["telemetry"]["attemptedAt"]
    assert not any(body["familyPresence"].values())
    assert body["extract"]["total"] is None
    assert body["matches"]["live"]["total"] is None
    assert body["cache"]["hitRatio"] is None
    assert body["latency"]["extract"] == {"avgSeconds": None, "count": None}
    assert body["spend"]["totalUsd"] is None and body["spend"]["monthlyUsd"] is None
    assert all(row["value"] is None and row["detail"] == "not instrumented" for item in body["stages"] for row in item["metrics"])


def test_thrown_source_error_is_sanitized(client, monkeypatch):
    async def fail():
        raise httpx.ConnectError("http://synthetic:secret@upstream.invalid")
    monkeypatch.setattr(gql, "fetch_metrics", fail)
    body = client.get("/api/ai/summary").json()
    assert body["telemetry"]["status"] == "error"
    assert body["telemetry"]["error"] == "Core metrics request failed."
    assert "secret" not in str(body)


def test_healthy_source_without_ai_is_distinct_from_zero_observations(client, monkeypatch):
    metrics(monkeypatch, "bitagent_unrelated_total 1\n")
    body = client.get("/api/ai/summary").json()
    assert body["telemetry"]["status"] == "ok"
    assert not any(body["familyPresence"].values())
    assert stage(body, "typeclassifier")["status"]["detail"] == "The core emitted no type-classifier metric family, so enabled state and activity are unknown."
    metrics(monkeypatch, '''
bitagent_classifier_llm_match_cache_hits_total 0
bitagent_classifier_llm_match_cache_misses_total 0
bitagent_classifier_llm_match_rerank_total{result="match"} 0
bitagent_classifier_llm_match_call_duration_seconds_sum{stage="extract"} 0
bitagent_classifier_llm_match_call_duration_seconds_count{stage="extract"} 0
''')
    body = client.get("/api/ai/summary").json()
    assert body["cache"] == {"hits": 0, "misses": 0, "hitRatio": None}
    assert body["rerank"]["total"] == 0 and body["rerank"]["matchRate"] is None
    assert body["latency"]["extract"] == {"avgSeconds": None, "count": 0}
    assert body["familyPresence"]["latencyExtract"]


def test_optional_prometheus_timestamp_and_missing_histogram_sum(client, monkeypatch):
    metrics(monkeypatch, '''
bitagent_classifier_llm_match_extract_total{result="ok"} 5 1700000000000
bitagent_classifier_llm_match_call_duration_seconds_count{stage="extract"} 5
''')
    body = client.get("/api/ai/summary").json()
    assert body["extract"]["ok"] == 5
    assert body["latency"]["extract"] == {"avgSeconds": None, "count": 5}
    assert not body["familyPresence"]["latencyExtract"]
    assert metric(stage(body, "matcher"), "Calls")["value"] is None
    assert stage(body, "matcher")["status"]["label"] == "Outcomes observed"
    assert metric(stage(body, "matcher"), "Avg latency")["value"] is None


def test_partial_configuration_gauge_does_not_invent_disabled_or_shadow(client, monkeypatch):
    metrics(monkeypatch, 'bitagent_classifier_llm_match_config{setting="enabled"} 1\n')
    item = stage(client.get("/api/ai/summary").json(), "matcher")
    assert item["config"]["live"] is None and item["config"]["daily_call_limit"] is None
    assert item["status"]["label"] == "Enabled; mode unknown"
    assert "0 outbound" not in item["status"]["detail"] and "0/day" not in item["status"]["detail"]


def test_canonical_content_filter_tokens_win_over_legacy_aliases_per_model():
    result = spend('''
bitagent_contentfilter_llm_tokens_total{model="gpt-5.4-nano",kind="input"} 100
bitagent_contentfilter_llm_tokens_total{model="gpt-5.4-nano",kind="prompt"} 900
bitagent_contentfilter_llm_tokens_total{model="gpt-5.4-nano",kind="output"} 10
bitagent_contentfilter_llm_tokens_total{model="gpt-5.4-nano",kind="completion"} 90
bitagent_contentfilter_llm_tokens_total{model="gpt-5.4-nano",kind="reasoning"} 4
bitagent_contentfilter_llm_tokens_total{model="google/gemma-3-12b-it",kind="prompt"} 200
bitagent_contentfilter_llm_tokens_total{model="google/gemma-3-12b-it",kind="completion"} 20
''')
    rows = {row["model"]: row for row in result["byModel"]}
    assert (rows["gpt-5.4-nano"]["inputTokens"], rows["gpt-5.4-nano"]["outputTokens"]) == (100, 10)
    assert (rows["google/gemma-3-12b-it"]["inputTokens"], rows["google/gemma-3-12b-it"]["outputTokens"]) == (200, 20)


def test_only_unknown_prices_are_unknown_not_zero(client, monkeypatch):
    metrics(monkeypatch, '''
bitagent_junkpurge_llm_tokens_total{model="synthetic/unpriced",type="input"} 100
bitagent_junkpurge_llm_tokens_total{model="synthetic/unpriced",type="output"} 10
''')
    result = client.get("/api/ai/summary").json()
    assert result["spend"]["totalUsd"] is None and result["spend"]["monthlyUsd"] is None
    assert result["spend"]["totalIsPartial"]
    assert stage(result, "junkpurge")["spend"]["usd"] is None


def test_missing_token_kind_and_usage_only_are_partial_unknowns():
    result = spend('''
bitagent_contentfilter_llm_tokens_total{model="gpt-5.4-nano",kind="input"} 100
bitagent_classifier_llm_usage_missing_total{model="gpt-5.4-nano"} 1
''')
    rows = {row["stage"]: row for row in result["byModel"]}
    assert rows["contentfilter"]["outputTokens"] is None
    assert rows["contentfilter"]["usageIncomplete"]
    assert rows["typeclassifier"]["inputTokens"] is None and rows["typeclassifier"]["usd"] is None
    assert result["totalIsPartial"]


def test_called_stage_without_tokens_makes_global_cost_partial(client, monkeypatch):
    metrics(monkeypatch, '''
bitagent_contentfilter_llm_tokens_total{model="gpt-5.4-nano",kind="input"} 100
bitagent_contentfilter_llm_tokens_total{model="gpt-5.4-nano",kind="output"} 10
bitagent_classifier_llm_calls_total{model="synthetic/unmetered"} 1
''')
    result = client.get("/api/ai/summary").json()["spend"]
    assert result["totalUsd"] > 0 and result["totalIsPartial"] and result["monthlyIsPartial"]
    assert result["unmeteredStages"] == ["typeclassifier"]


def test_all_models_in_one_stage_contribute_to_cost(client, monkeypatch):
    text = '''
bitagent_contentfilter_llm_tokens_total{model="gpt-5.4-nano",kind="input"} 1000000
bitagent_contentfilter_llm_tokens_total{model="gpt-5.4-nano",kind="output"} 0
bitagent_contentfilter_llm_tokens_total{model="google/gemma-3-12b-it",kind="input"} 1000000
bitagent_contentfilter_llm_tokens_total{model="google/gemma-3-12b-it",kind="output"} 0
bitagent_contentfilter_drop_total{reason="llm_non_english"} 5
'''
    metrics(monkeypatch, text)
    item = stage(client.get("/api/ai/summary").json(), "contentfilter")
    assert item["spend"]["usd"] == pytest.approx(0.25)
    assert item["spend"]["usdPerUnit"] == pytest.approx(0.05)
    assert not item["spend"]["totalIsPartial"]


def test_same_model_in_two_stages_keeps_independent_projection(monkeypatch):
    def snapshot(cf, tc):
        return f'''
bitagent_contentfilter_llm_tokens_total{{model="gpt-5.4-nano",kind="input"}} {cf}
bitagent_contentfilter_llm_tokens_total{{model="gpt-5.4-nano",kind="output"}} 0
bitagent_classifier_llm_tokens_total{{model="gpt-5.4-nano",kind="input"}} {tc}
bitagent_classifier_llm_tokens_total{{model="gpt-5.4-nano",kind="output"}} 0
'''
    monkeypatch.setattr(app_module, "_metric_history_now", lambda: 100.0)
    app_module._spend_snapshot(snapshot(1000000, 2000000))
    monkeypatch.setattr(app_module, "_metric_history_now", lambda: 160.0)
    result = app_module._spend_snapshot(snapshot(1100000, 2200000))
    rows = {row["stage"]: row for row in result["byModel"]}
    assert rows["contentfilter"]["monthlyUsd"] == pytest.approx(864)
    assert rows["typeclassifier"]["monthlyUsd"] == pytest.approx(1728)
    assert result["monthlyUsd"] == pytest.approx(2592)


@pytest.mark.parametrize("boundary", ["failure", "restart", "counter_reset"])
def test_projection_requires_new_baseline_after_observation_boundary(monkeypatch, boundary):
    def snapshot(tokens, boot=1700000000):
        return f'''
bitmagnet_process_start_time_seconds {boot}
bitagent_classifier_llm_tokens_total{{model="gpt-5.4-nano",kind="input"}} {tokens}
bitagent_classifier_llm_tokens_total{{model="gpt-5.4-nano",kind="output"}} 0
'''
    monkeypatch.setattr(app_module, "_metric_history_now", lambda: 100.0)
    app_module._spend_snapshot(snapshot(1000000))
    monkeypatch.setattr(app_module, "_metric_history_now", lambda: 130.0)
    if boundary == "failure":
        app_module._spend_snapshot("")
    monkeypatch.setattr(app_module, "_metric_history_now", lambda: 160.0)
    result = app_module._spend_snapshot(snapshot(1200000 if boundary != "counter_reset" else 100,
                                                1700000100 if boundary == "restart" else 1700000000))
    assert result["monthlyUsd"] is None and result["monthlyStatus"] == "measuring"
    assert result["monthlyWindowSeconds"] == 0


def test_cached_subset_alone_does_not_claim_measured_zero_total(client, monkeypatch):
    metrics(monkeypatch, 'bitagent_contentfilter_llm_tokens_total{model="gpt-5.4-nano",kind="cached_input"} 100\n')
    body = client.get("/api/ai/summary").json()
    row = body["spend"]["byModel"][0]
    assert row["inputTokens"] is None and row["outputTokens"] is None
    assert row["priceAvailable"] is True and row["priced"] is False
    assert row["usd"] is None and body["spend"]["totalUsd"] is None
    item = stage(body, "contentfilter")
    assert item["spend"]["priceAvailable"] is True
    assert item["spend"]["allPricesAvailable"] is True
    assert item["spend"]["usageIncomplete"] is True
    assert item["spend"]["unpricedModels"] == []
    assert item["tokens"] == {"input": None, "output": None}
    assert metric(item, "Tokens")["value"] is None


def test_absent_effect_counters_do_not_claim_zero_enforcement(client, monkeypatch):
    metrics(monkeypatch, '''
bitagent_contentfilter_would_drop_total{reason="blocked_extension"} 3
bitagent_junkpurge_would_delete_total 2
''')
    body = client.get("/api/ai/summary").json()
    cf = stage(body, "contentfilter")
    jp = stage(body, "junkpurge")
    assert "Enforced-drop counter not instrumented" in cf["status"]["detail"]
    assert "Quarantine counter not instrumented" in jp["status"]["detail"]
    assert metric(jp, "Quarantined")["value"] is None

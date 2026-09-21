"""GraphQL/metrics client: evidence reshaping + soft-fail contract."""
import asyncio

import httpx

import graphql_client as gql


def test_parse_iso8601_to_unix():
    assert gql._parse_iso8601_to_unix("") == 0.0
    assert gql._parse_iso8601_to_unix("not-a-date") == 0.0
    assert gql._parse_iso8601_to_unix("2026-06-22T00:00:00Z") > 0


def test_media_type_mapping_table():
    m = gql._MEDIA_TYPE_TO_CONTENT_TYPE
    assert m["tv"] == "tv_show"
    assert m["book"] == "ebook"
    assert m["unknown"] == ""


def test_fetch_evidence_list_reshapes_to_dashboard_contract(monkeypatch):
    async def fake_query(q, variables=None, **kwargs):
        return {"data": {"evidence": {"list": {
            "totalCount": 1,
            "items": [{
                "id": "42", "source": "sonarr", "kind": "webhook_grab",
                "infoHash": "abc123", "title": "Show S01E01",
                "mediaType": "tv", "observedAt": "2026-06-22T00:00:00Z",
            }],
        }}}}

    monkeypatch.setattr(gql, "query", fake_query)
    out = asyncio.run(gql.fetch_evidence_list(10, 0))
    assert out["totalCount"] == 1
    it = out["items"][0]
    assert it["id"] == 42                  # coerced str -> int
    assert it["eventType"] == "webhook_grab"   # kind -> eventType
    assert it["contentType"] == "tv_show"      # mediaType mapped
    # No fabricated `result` field — the UI colours the real eventType instead
    # of a hardcoded "success" that made every row show a green pill.
    assert "result" not in it
    assert it["timestamp"] > 0


def test_query_and_metrics_soft_fail(monkeypatch):
    class FakeClient:
        async def post(self, *a, **k):
            raise httpx.HTTPError("boom")

        async def get(self, *a, **k):
            raise httpx.HTTPError("boom")

    monkeypatch.setattr(gql, "_get_client", lambda: FakeClient())
    res = asyncio.run(gql.query("{ x }"))
    assert res["data"] is None and res["errors"]
    assert asyncio.run(gql.fetch_metrics()) == ""

# BitAgent Dashboard API Reference

This document describes the REST API exposed by the BitAgent dashboard —
the `ui` worker inside the BitAgent image, on port 8080 by default. All
endpoints are served from the dashboard's base URL under an operator host
(`OPERATOR_HOSTS`); operator APIs are `404` on a public-library host.
Responses use JSON unless otherwise noted. Authenticated endpoints resolve
identity through the tiers in [ui/README.md](../../ui/README.md); requests
that resolve to nobody receive `401 Unauthorized`. The pages below cover the
endpoints the console's tabs use; `ui/app.py` is the complete list (it also
serves `/api/quarantine`, `/api/ai/summary`, `/api/block-phrases`,
`/api/account` and `/api/indexer-stats`, undocumented here).

---

## Health

### GET /healthz

Returns the current health status of the dashboard service.

**Authentication:** None required.

**Response:**

```json
{
  "status": "ok",
  "ts": 1789989357.809
}
```

| Field    | Type   | Description                            |
|----------|--------|----------------------------------------|
| `status` | string | Service status. `"ok"` when healthy.   |
| `ts`     | number | Unix timestamp (seconds) of the check. |

`/healthz` is the only host-agnostic path — it answers on any `Host`, so container healthchecks work — and it never calls the core.

---

## Auth

### GET /api/me

Returns the identity of the currently authenticated user.

**Response:**

```json
{
  "id": "usr_abc123",
  "method": "api_key",
  "display": "operator"
}
```

| Field     | Type   | Description                                    |
|-----------|--------|------------------------------------------------|
| `id`      | string | Unique identifier for the authenticated user.  |
| `method`  | string | Authentication method (e.g. `api_key`, `oidc`).|
| `display` | string | Human-readable display name.                   |

---

## Stats

### GET /api/stats

Returns aggregate system statistics from the BitAgent core.

**Response:**

```json
{
  "totalTorrents": 142857,
  "totalReleases": 98412,
  "totalEvidence": 310044,
  "dhtPeerCount": 2048,
  "indexerThroughput": 320.5,
  "cacheHitRatio": 0.87,
  "uptimeSeconds": 604800,
  "lastCrawlAt": "2026-04-27T12:00:00.000Z",
  "categoryBreakdown": {
    "movie": 42000,
    "tv": 35000,
    "music": 21412
  }
}
```

| Field                 | Type   | Description                                       |
|-----------------------|--------|---------------------------------------------------|
| `totalTorrents`       | number | Total number of tracked torrents.                 |
| `totalReleases`       | number | Total number of identified releases.              |
| `totalEvidence`       | number | Total evidence records collected.                 |
| `dhtPeerCount`        | number | Current number of DHT peers.                      |
| `indexerThroughput`   | number | Indexer processing rate (items per second).        |
| `cacheHitRatio`       | number | Cache hit ratio between 0 and 1.                  |
| `uptimeSeconds`       | number | Seconds since the BitAgent core last started.     |
| `lastCrawlAt`         | string | ISO-8601 timestamp of the most recent crawl.      |
| `categoryBreakdown`   | object | Map of content category to torrent count.         |

### GET /api/metrics

Proxies Prometheus-format metrics from the BitAgent core.

**Response:** `text/plain` — standard Prometheus exposition format.

---

## Library

### GET /api/torrents

Search and paginate the torrent library.

**Query Parameters:**

| Parameter      | Type   | Default | Description                              |
|----------------|--------|---------|------------------------------------------|
| `q`            | string | `""`    | Free-text search query.                  |
| `content_type` | string | `""`    | Filter by content type (e.g. `movie`).   |
| `limit`        | number | `50`    | Maximum items to return (1--500).        |
| `offset`       | number | `0`     | Number of items to skip for pagination.  |

**Response:**

```json
{
  "totalCount": 1420,
  "items": [
    {
      "info_hash": "a1b2c3d4e5f6...",
      "title": "Example Release",
      "content_type": "movie",
      "size_bytes": 1073741824,
      "created_at": "2026-04-20T08:00:00.000Z"
    }
  ]
}
```

| Field        | Type   | Description                              |
|--------------|--------|------------------------------------------|
| `totalCount` | number | Total matching results (before paging).  |
| `items`      | array  | Array of torrent summary objects.        |

### GET /api/torrents/{info_hash}

Returns full detail for a single torrent, including its file list,
associated evidence records, and a pre-built magnet URI.

**Path Parameters:**

| Parameter   | Type   | Description                            |
|-------------|--------|----------------------------------------|
| `info_hash` | string | The torrent's unique info hash (hex).  |

**Response:**

```json
{
  "info_hash": "a1b2c3d4e5f6...",
  "title": "Example Release",
  "content_type": "movie",
  "size_bytes": 1073741824,
  "created_at": "2026-04-20T08:00:00.000Z",
  "magnet_uri": "magnet:?xt=urn:btih:a1b2c3d4e5f6...",
  "files": [
    {
      "path": "Example.Release.2026.mkv",
      "size_bytes": 1073741824
    }
  ],
  "evidence": [
    {
      "id": "ev_001",
      "source": "dht",
      "recorded_at": "2026-04-21T10:15:00.000Z"
    }
  ]
}
```

**Error Responses:**

| Status | Description                                      |
|--------|--------------------------------------------------|
| `404`  | No torrent found with the specified `info_hash`. |

---

## Evidence

### GET /api/evidence

Returns a paginated list of webhook evidence events.

**Query Parameters:**

| Parameter | Type   | Default | Description                             |
|-----------|--------|---------|-----------------------------------------|
| `limit`   | number | `50`    | Maximum items to return (1--500).       |
| `offset`  | number | `0`     | Number of items to skip for pagination. |

**Response:**

```json
{
  "items": [
    {
      "id": "ev_001",
      "info_hash": "a1b2c3d4e5f6...",
      "source": "dht",
      "recorded_at": "2026-04-21T10:15:00.000Z",
      "payload": {}
    }
  ]
}
```

---

## Wants

`GET /api/wantbridge` (operator only) reports the core's wantbridge as observations: wanted titles per \*arr, exact matches found, fingerprint keys and poll health. Wants are not created from the dashboard — they come from the \*arrs the core polls (see [Wantbridge](../concepts/wantbridge.md)). There is no `/api/wants` CRUD.

---

## Settings

These endpoints require operator authority. Runtime overrides are limited to
`MUTABLE_FIELDS` in `ui/config.py`: console `log_level`, TMDB and Torznab keys,
and the Sonarr, Radarr and Lidarr base URLs and keys. Host routing, proxy trust,
operator roles and core endpoint URLs are startup-only. Every save and reset
writes an audit row; secret values are stored as `[redacted]` in that log.

### GET /api/settings

Returns a `fields` object keyed by setting name and a `mutable_keys` array.
For non-sensitive fields, `default` is the resolved startup value and `current`
is the effective value; both are strings. Sensitive fields return only
configured/source and override state, never values or lengths.

**Synthetic response example:**

```json
{
  "fields": {
    "lidarr_api_key": {"sensitive": true, "configured": false, "source": "unset", "overridden": false},
    "lidarr_base_url": {"sensitive": false, "default": "", "current": "", "source": "startup", "overridden": false},
    "log_level": {"sensitive": false, "default": "info", "current": "warning", "source": "override", "overridden": true},
    "radarr_api_key": {"sensitive": true, "configured": false, "source": "unset", "overridden": false},
    "radarr_base_url": {"sensitive": false, "default": "", "current": "", "source": "startup", "overridden": false},
    "sonarr_api_key": {"sensitive": true, "configured": true, "source": "override", "overridden": true},
    "sonarr_base_url": {"sensitive": false, "default": "", "current": "https://sonarr.example.com", "source": "override", "overridden": true},
    "tmdb_api_key": {"sensitive": true, "configured": false, "source": "unset", "overridden": false},
    "torznab_api_key": {"sensitive": true, "configured": true, "source": "startup", "overridden": false}
  },
  "mutable_keys": ["lidarr_api_key", "lidarr_base_url", "log_level", "radarr_api_key", "radarr_base_url", "sonarr_api_key", "sonarr_base_url", "tmdb_api_key", "torznab_api_key"]
}
```

| Field | Type | Description |
|---|---|---|
| `sensitive` | boolean | Whether the field contains a credential. |
| `default` / `current` | string | Startup/effective values; absent for sensitive fields. |
| `configured` | boolean | Whether a sensitive field has a value; absent for non-sensitive fields. |
| `source` | string | `startup` or `override`; an unset sensitive field reports `unset`. |
| `overridden` | boolean | Whether a saved runtime override exists. |

### PUT /api/settings/overrides/{key}

Saves a string override for a mutable setting. For example,
`PUT /api/settings/overrides/log_level` accepts:

```json
{
  "value": "warning"
}
```

Logging accepts `debug`, `info`, `warning`, `error` or `critical` (`warn` is
normalized to `warning`). It changes only the console's application logger;
core and web-server logging remain deployment-managed. Non-empty *arr URLs
must pass HTTP/HTTPS and resolved-address validation before saving.

**Response:** `200 OK`. Non-sensitive fields return `key`, `old`, `new`,
`actor` and `at` (Unix seconds). Sensitive fields return `key`, `sensitive`,
`configured`, `source`, `overridden`, `actor` and `at`, without credential
material.

| Status | Description |
|---|---|
| `400` | Invalid logging level or rejected *arr base URL. |
| `403` | The key is not mutable. |
| `422` | Request body is missing a string `value` or fails schema validation. |

### DELETE /api/settings/overrides/{key}

Removes an existing runtime override and restores the resolved startup value.
A logging reset also reapplies the startup console logging level.

**Response:** `200 OK` with `{"status": "deleted"}`.

| Status | Description |
|---|---|
| `403` | The key is not mutable. |
| `404` | No saved override exists for the key. |

### GET /api/settings/audit

Returns an array of change records, newest first. `limit` defaults to `100`
and accepts `1` through `1000`. Secret `old` and `new` values are redacted.
A reset has `new: null`.

**Synthetic response example:**

```json
[
  {
    "id": 1,
    "key": "log_level",
    "old": "info",
    "new": "warning",
    "actor": "demo-operator",
    "at": 1790841600.0
  }
]
```

---

## Notifications

### GET /api/notifications

Returns the list of notifications for the current user.

**Response:**

```json
{
  "items": [
    {
      "id": "n_001",
      "type": "want_fulfilled",
      "message": "Want 'Some Movie 2026' has been fulfilled.",
      "read": false,
      "created_at": "2026-04-27T13:00:00.000Z"
    }
  ]
}
```

### PUT /api/notifications/{id}/read

Marks a notification as read.

**Path Parameters:**

| Parameter | Type   | Description                       |
|-----------|--------|-----------------------------------|
| `id`      | string | The notification's unique ID.     |

**Response:** `200 OK` with the updated notification object.

**Error Responses:**

| Status | Description                                         |
|--------|-----------------------------------------------------|
| `404`  | No notification found with the specified `id`.      |

---

## TMDB

### GET /api/poster/{tmdb_id}

Returns a cached poster image URL for the given TMDB identifier.
Results are cached server-side to avoid repeated TMDB API calls.

**Path Parameters:**

| Parameter | Type   | Description             |
|-----------|--------|-------------------------|
| `tmdb_id` | string | The TMDB identifier.    |

**Query Parameters:**

| Parameter    | Type   | Default   | Description                              |
|--------------|--------|-----------|------------------------------------------|
| `media_type` | string | `"movie"` | Media type: `movie` or `tv`.             |

**Response:**

```json
{
  "tmdb_id": "12345",
  "media_type": "movie",
  "poster_url": "https://image.tmdb.org/t/p/w500/example.jpg"
}
```

---

## GraphQL

### POST /api/graphql

Proxies a GraphQL request to the BitAgent core engine. This endpoint
allows the dashboard to execute arbitrary queries and mutations
supported by the core's GraphQL schema.

**Request Body:**

```json
{
  "query": "query { torrents(limit: 10) { info_hash title } }",
  "variables": {}
}
```

| Field       | Type   | Required | Description                        |
|-------------|--------|----------|------------------------------------|
| `query`     | string | Yes      | The GraphQL query or mutation.     |
| `variables` | object | No       | Variables referenced by the query. |

**Response:** Standard GraphQL response envelope:

```json
{
  "data": {
    "torrents": [
      {
        "info_hash": "a1b2c3d4e5f6...",
        "title": "Example Release"
      }
    ]
  },
  "errors": null
}
```

---

## Common Error Responses

All endpoints may return the following standard errors:

| Status | Description                                                   |
|--------|---------------------------------------------------------------|
| `400`  | Bad request. The request body or parameters are malformed.    |
| `401`  | Unauthorized. Authentication is required or has expired.      |
| `404`  | Not found. The requested resource does not exist.             |
| `500`  | Internal server error. An unexpected condition was encountered.|

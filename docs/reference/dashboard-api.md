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

Settings are managed through a defaults-plus-overrides model. Each
setting has a default value defined by the system. Users can override
individual settings, and every change is recorded in the audit log.

### GET /api/settings

Returns all mutable settings with their default values, current
effective values, and override status.

**Response:**

```json
{
  "items": [
    {
      "key": "crawl_interval_seconds",
      "default": 3600,
      "value": 1800,
      "overridden": true
    },
    {
      "key": "max_concurrent_downloads",
      "default": 5,
      "value": 5,
      "overridden": false
    }
  ]
}
```

| Field        | Type    | Description                                       |
|--------------|---------|---------------------------------------------------|
| `key`        | string  | Unique setting identifier.                        |
| `default`    | any     | System-defined default value.                     |
| `value`      | any     | Current effective value (default or override).    |
| `overridden` | boolean | `true` if the user has set a custom override.     |

### PUT /api/settings/overrides/{key}

Sets a user override for a specific setting.

**Path Parameters:**

| Parameter | Type   | Description                   |
|-----------|--------|-------------------------------|
| `key`     | string | The setting key to override.  |

**Request Body:**

```json
{
  "value": 1800
}
```

| Field   | Type | Required | Description            |
|---------|------|----------|------------------------|
| `value` | any  | Yes      | The new override value. |

**Response:** `200 OK` with the updated setting object.

**Error Responses:**

| Status | Description                                    |
|--------|------------------------------------------------|
| `400`  | Invalid value type or out-of-range value.      |
| `404`  | No setting found with the specified `key`.     |

### DELETE /api/settings/overrides/{key}

Removes a user override, reverting the setting to its default value.

**Path Parameters:**

| Parameter | Type   | Description                          |
|-----------|--------|--------------------------------------|
| `key`     | string | The setting key to reset to default. |

**Response:** `200 OK` with the setting restored to its default value.

**Error Responses:**

| Status | Description                                |
|--------|--------------------------------------------|
| `404`  | No setting found with the specified `key`. |

### GET /api/settings/audit

Returns an audit log of setting changes.

**Query Parameters:**

| Parameter | Type   | Default | Description                          |
|-----------|--------|---------|--------------------------------------|
| `limit`   | number | `100`   | Maximum entries to return (1--1000). |

**Response:**

```json
{
  "items": [
    {
      "key": "crawl_interval_seconds",
      "action": "override_set",
      "old_value": 3600,
      "new_value": 1800,
      "changed_by": "operator",
      "changed_at": "2026-04-27T11:00:00.000Z"
    }
  ]
}
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

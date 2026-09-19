package attribution

import (
	"context"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// prowlarrClient pulls grab events from Prowlarr's /api/v1/history.
//
// Prowlarr's history shape (verified against live API 2026-04-26):
//
//	{
//	  "id": 438506,
//	  "indexerId": 1,            // number; name needs lookup
//	  "date": "2026-04-26T01:17:48Z",
//	  "eventType": "releaseGrabbed",
//	  "successful": true,
//	  "data": {
//	    "source": "Sonarr",      // requesting *arr
//	    "grabTitle": "...",      // NOT "title"
//	    "grabMethod": "Proxy",
//	    "host": "localhost",
//	    "url": "magnet:?xt=urn:btih:HEX..."  // NOT "magnetUrl"
//	  }
//	}
//
// We do NOT get an `indexer` (string name) at the top level — that
// must be resolved via /api/v1/indexer keyed on indexerId. The
// resolution is cached for the lifetime of the prowlarrClient.
type prowlarrClient struct {
	baseURL string
	apiKey  string
	http    *http.Client

	// indexerNames is the cached id → name map populated on first
	// FetchGrabs call. Prowlarr's indexer config rarely changes
	// during a recon session; refreshing per-call is wasteful.
	indexerNames map[int]string
}

func newProwlarrClient(baseURL, apiKey string, timeout time.Duration) *prowlarrClient {
	return &prowlarrClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: timeout},
	}
}

// prowlarrHistoryRecord is the projection of /api/v1/history that
// drives the join. Field names match Prowlarr's actual JSON
// (verified 2026-04-26); see the type-doc on prowlarrClient for
// the full shape rationale.
type prowlarrHistoryRecord struct {
	ID         int64  `json:"id"`
	IndexerID  int    `json:"indexerId"` // resolved to name via cache
	Date       string `json:"date"`      // ISO 8601
	EventType  string `json:"eventType"`
	Successful bool   `json:"successful"`
	Data       struct {
		Source     string `json:"source"`     // requesting *arr
		GrabTitle  string `json:"grabTitle"`  // the actual release title
		GrabMethod string `json:"grabMethod"` // "Proxy" / "RedirectUrl"
		URL        string `json:"url"`        // magnet:?xt=urn:btih:HEX or http(s)://
		// Older / fallback fields. Tolerated for forward-compat
		// across Prowlarr versions but the canonical fields above
		// take precedence.
		Title       string `json:"title"`
		MagnetURL   string `json:"magnetUrl"`
		DownloadURL string `json:"downloadUrl"`
		InfoHash    string `json:"infoHash"`
		Guid        string `json:"guid"`
		Size        string `json:"size"`
	} `json:"data"`
}

// prowlarrIndexerEntry is the minimal projection of /api/v1/indexer
// for the id→name cache.
type prowlarrIndexerEntry struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// btihRE finds an info_hash in xt=urn:btih:HEX inside any URL string.
// Captures both 40-char hex (SHA-1) and 32-char base32 forms.
// Callers MUST run the captured value through normaliseBTIH so qB
// lookups (which always use lowercase hex) work consistently —
// without normalisation a base32 magnet would silently miss the
// join.
var btihRE = regexp.MustCompile(`(?i)urn:btih:([0-9a-zA-Z]+)`)

// normaliseBTIH coerces a btih value to lowercase 40-char hex.
// Inputs accepted:
//   - 40-char hex (any case) → lowercased
//   - 32-char base32 (RFC 4648 alphabet, A-Z2-7, no padding) → decoded
//     to 20 bytes, hex-encoded
//   - anything else → "" (caller treats as "no info_hash")
//
// qB's /api/v2/torrents/info?hashes=... expects lowercase hex; the
// *arr history endpoints similarly use hex in `data.torrentInfoHash`.
// Review caught the missing normalisation
// — base32 magnets are rare in modern Prowlarr indexers but exist
// on some (e.g. The Pirate Bay sometimes emits them); fixing here
// closes the silent-miss path.
func normaliseBTIH(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) == 40 && isHexAll(s) {
		return strings.ToLower(s)
	}
	if len(s) == 32 {
		decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).
			DecodeString(strings.ToUpper(s))
		if err == nil && len(decoded) == 20 {
			return hex.EncodeToString(decoded)
		}
	}
	return ""
}

// isHexAll reports whether every rune is a hex digit. Avoids a regex
// for the hot path in case attribution-recon is ever called on a
// large grab list.
func isHexAll(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// FetchGrabs returns grab events from Prowlarr in the time window
// (now - sinceHours, now]. Prowlarr's API doesn't have a clean
// "since timestamp" filter, so we page through descending and stop
// at the first record older than the cutoff.
//
// On first call, also fetches the indexer-name lookup table so the
// returned GrabEvent.Indexer is populated with the human-readable
// name (e.g. "BitMagnet (Local DHT)") rather than the bare numeric
// id. Cached for subsequent calls.
func (c *prowlarrClient) FetchGrabs(ctx context.Context, sinceHours, limit int) ([]GrabEvent, error) {
	if sinceHours <= 0 {
		sinceHours = 24
	}
	cutoff := time.Now().Add(-time.Duration(sinceHours) * time.Hour)

	if c.indexerNames == nil {
		if err := c.refreshIndexerNames(ctx); err != nil {
			// Indexer-name lookup failure is non-fatal: callers
			// can still match on partial data, and the empty cache
			// falls through to the empty-string indexer name (the
			// substring filter just sees "" and treats it as no
			// match). Log via the caller's typical surfaces; here
			// we just init to an empty map so subsequent calls
			// don't retry on every page.
			c.indexerNames = map[int]string{}
		}
	}

	var out []GrabEvent
	page := 1
	pageSize := 100

	for {
		recs, err := c.fetchPage(ctx, page, pageSize)
		if err != nil {
			return nil, err
		}
		if len(recs) == 0 {
			break
		}
		stopPaging := false
		for _, r := range recs {
			grabbedAt, err := time.Parse(time.RFC3339, r.Date)
			if err != nil {
				// Some Prowlarr versions add fractional seconds;
				// tolerate by truncating.
				grabbedAt, err = time.Parse("2006-01-02T15:04:05.999Z", r.Date)
				if err != nil {
					// Skip records we can't parse the date on
					// rather than abort the whole fetch.
					continue
				}
			}
			if grabbedAt.Before(cutoff) {
				stopPaging = true
				break
			}
			if !strings.EqualFold(r.EventType, "releaseGrabbed") {
				continue
			}
			ev := c.buildGrabEvent(r, grabbedAt)
			out = append(out, ev)
			if limit > 0 && len(out) >= limit {
				return out, nil
			}
		}
		if stopPaging || len(recs) < pageSize {
			break
		}
		page++
	}
	return out, nil
}

// refreshIndexerNames pulls /api/v1/indexer once and builds the
// id→name map.
func (c *prowlarrClient) refreshIndexerNames(ctx context.Context) error {
	url := c.baseURL + "/api/v1/indexer"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("prowlarr indexers: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("prowlarr indexers: read: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		snip := string(body)
		if len(snip) > 200 {
			snip = snip[:200] + "...[truncated]"
		}
		return fmt.Errorf("prowlarr indexers: http %d: %s", resp.StatusCode, snip)
	}
	var entries []prowlarrIndexerEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return fmt.Errorf("prowlarr indexers: parse: %w", err)
	}
	c.indexerNames = make(map[int]string, len(entries))
	for _, e := range entries {
		c.indexerNames[e.ID] = e.Name
	}
	return nil
}

func (c *prowlarrClient) fetchPage(ctx context.Context, page, pageSize int) ([]prowlarrHistoryRecord, error) {
	q := url.Values{}
	q.Set("page", strconv.Itoa(page))
	q.Set("pageSize", strconv.Itoa(pageSize))
	q.Set("sortKey", "date")
	q.Set("sortDirection", "descending")
	q.Set("eventType", "1") // 1 = releaseGrabbed in Prowlarr's enum
	endpoint := c.baseURL + "/api/v1/history?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prowlarr: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("prowlarr: read: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		snip := string(body)
		if len(snip) > 200 {
			snip = snip[:200] + "...[truncated]"
		}
		return nil, fmt.Errorf("prowlarr: http %d: %s", resp.StatusCode, snip)
	}
	var paged struct {
		Records []prowlarrHistoryRecord `json:"records"`
	}
	if err := json.Unmarshal(body, &paged); err != nil {
		return nil, fmt.Errorf("prowlarr: parse: %w", err)
	}
	return paged.Records, nil
}

// buildGrabEvent reconciles Prowlarr's record into the canonical
// GrabEvent shape. Per the live-API verification on 2026-04-26:
//
//   - Indexer NAME is not on the record; resolve via the cached
//     indexerId → name lookup. If lookup misses, the indexer label
//     stays empty and the substring filter at the orchestrator
//     level will skip the row.
//   - Title comes from data.grabTitle (Prowlarr's canonical),
//     with data.title and the legacy top-level title as fallbacks
//     for older Prowlarr versions.
//   - info_hash is extracted from data.url (the magnet URI), with
//     fallbacks to data.magnetUrl / data.downloadUrl / data.guid /
//     data.infoHash for forward-compat across Prowlarr versions.
//   - All info_hash candidates run through normaliseBTIH so
//     base32 magnets land as 40-char lowercase hex (the form qB
//     and *arr APIs expect).
func (c *prowlarrClient) buildGrabEvent(r prowlarrHistoryRecord, grabbedAt time.Time) GrabEvent {
	// Source: Prowlarr puts the requesting *arr in data.source.
	source := normaliseSource(r.Data.Source)

	// info_hash search: data.url first (the canonical magnet),
	// then the legacy fields for forward-compat.
	infoHash := normaliseBTIH(r.Data.InfoHash)
	if infoHash == "" {
		for _, candidate := range []string{r.Data.URL, r.Data.MagnetURL, r.Data.DownloadURL, r.Data.Guid} {
			if candidate == "" {
				continue
			}
			if m := btihRE.FindStringSubmatch(candidate); m != nil {
				infoHash = normaliseBTIH(m[1])
				if infoHash != "" {
					break
				}
			}
		}
	}

	// size is sometimes int, sometimes string in legacy versions.
	size := int64(0)
	if v, err := strconv.ParseInt(r.Data.Size, 10, 64); err == nil {
		size = v
	}

	// Title: prefer Prowlarr's canonical grabTitle.
	title := r.Data.GrabTitle
	if title == "" {
		title = r.Data.Title
	}

	// Resolve indexer name via the cache. If absent, leave empty —
	// the orchestrator's filter will skip records with empty
	// indexer when a non-empty filter is configured.
	indexer := ""
	if c.indexerNames != nil {
		indexer = c.indexerNames[r.IndexerID]
	}

	return GrabEvent{
		GrabbedAt:  grabbedAt,
		Indexer:    indexer,
		Source:     source,
		Title:      title,
		InfoHash:   infoHash,
		Size:       size,
		ProwlarrID: r.ID,
	}
}

// normaliseSource maps the case-varying Prowlarr "source" field to
// our canonical Source enum.
func normaliseSource(s string) Source {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "sonarr":
		return SourceSonarr
	case "radarr":
		return SourceRadarr
	case "lidarr":
		return SourceLidarr
	}
	return Source("")
}

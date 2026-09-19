package attribution

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// arrHistoryClient queries Sonarr/Radarr/Lidarr's history endpoint
// for the import outcome of a given info_hash. The three *arrs share
// almost the same shape; this client abstracts the version + path
// differences.
type arrHistoryClient struct {
	baseURL string
	apiKey  string
	source  Source
	http    *http.Client
}

func newArrHistoryClient(source Source, baseURL, apiKey string, timeout time.Duration) *arrHistoryClient {
	return &arrHistoryClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		source:  source,
		http:    &http.Client{Timeout: timeout},
	}
}

// pathForSource returns the version-prefixed history endpoint.
// Sonarr / Radarr are /api/v3/history; Lidarr is /api/v1/history.
func (c *arrHistoryClient) historyPath() string {
	if c.source == SourceLidarr {
		return "/api/v1/history"
	}
	return "/api/v3/history"
}

// arrHistoryRecord is the shared projection. Field naming is the
// same across Sonarr/Radarr/Lidarr v3+ for the fields we use.
type arrHistoryRecord struct {
	ID         int64  `json:"id"`
	Date       string `json:"date"`
	EventType  string `json:"eventType"`
	SourceTitle string `json:"sourceTitle"`
	Data       struct {
		// info_hash isn't always exposed at top-level; data.downloadId
		// is populated by the *arr from the indexer's GUID and is the
		// most reliable join field.
		DownloadID  string `json:"downloadId"`
		DownloadURL string `json:"downloadUrl"`
		TorrentInfoHash string `json:"torrentInfoHash"`
		Reason      string `json:"reason"`
	} `json:"data"`
}

// LookupRecent fetches the *arr's recent history (last `pageSize`
// records) and returns a map[lower-hex-info-hash → ArrImport].
//
// Why a recent-window fetch rather than a per-hash query: Sonarr /
// Radarr / Lidarr don't have a "lookup by info_hash" endpoint on
// the history controller. Pulling the last 200 records is cheap and
// covers the typical attribution window comfortably.
func (c *arrHistoryClient) LookupRecent(ctx context.Context, pageSize int) (map[string]ArrImport, error) {
	if pageSize <= 0 {
		pageSize = 200
	}
	q := url.Values{}
	q.Set("pageSize", fmt.Sprintf("%d", pageSize))
	q.Set("sortKey", "date")
	q.Set("sortDirection", "descending")
	endpoint := c.baseURL + c.historyPath() + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s history: %w", c.source, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		snip := string(body)
		if len(snip) > 200 {
			snip = snip[:200] + "...[truncated]"
		}
		return nil, fmt.Errorf("%s history http %d: %s", c.source, resp.StatusCode, snip)
	}
	var paged struct {
		Records []arrHistoryRecord `json:"records"`
	}
	if err := json.Unmarshal(body, &paged); err != nil {
		return nil, fmt.Errorf("%s history parse: %w", c.source, err)
	}

	// Index by info_hash. Only the LATEST event per hash wins —
	// newer events typically supersede (e.g. an "imported" event
	// after a "grabbed" event for the same hash). Records are in
	// descending-date order so the first encounter is the latest.
	out := make(map[string]ArrImport)
	for _, r := range paged.Records {
		h := strings.ToLower(strings.TrimSpace(r.Data.TorrentInfoHash))
		if h == "" {
			h = strings.ToLower(strings.TrimSpace(r.Data.DownloadID))
		}
		if h == "" {
			continue
		}
		if _, seen := out[h]; seen {
			continue
		}
		dt, _ := time.Parse(time.RFC3339, r.Date)
		out[h] = ArrImport{
			Found:      true,
			Source:     c.source,
			EventType:  r.EventType,
			OccurredAt: dt,
			Reason:     r.Data.Reason,
		}
	}
	return out, nil
}

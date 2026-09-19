package wantbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// arrClient is the minimal interface every *arr client implements.
// One Fetch call returns the current full wantlist as a slice of
// canonical entries (already normalised). Errors are operational —
// the poller logs + counts them and falls through to the previous
// snapshot rather than blanking the wantlist.
type arrClient interface {
	Source() Source
	Fetch(ctx context.Context) ([]wantEntry, error)
}

// wantEntry is one item the operator has told their *arr to track.
// One entry can produce multiple canonical keys (e.g. a TV series
// expands to one key per monitored season).
type wantEntry struct {
	Source Source

	// Canonical is the normalised shape used for matching. Multiple
	// canonicals per *arr entry get expanded by the poller before
	// reaching this point — so by the time wantEntry exists, each
	// represents exactly one indexable canonical key.
	Canonical Canonical

	// PushTarget carries the upstream IDs needed by the D5 push
	// pipeline. The poller fills this in so consumers don't need
	// a separate lookup.
	PushTarget PushTarget
}

// arrHTTPDo wraps an HTTP request with the *arr's API key header
// and a deadline. Returns the body bytes on 2xx; errors otherwise.
// All *arr APIs use the same X-Api-Key header convention.
func arrHTTPDo(ctx context.Context, client *http.Client, baseURL, apiKey, path string) ([]byte, error) {
	url := strings.TrimRight(baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("arr: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("arr: read: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		// Truncate response body in error so we don't dump huge
		// HTML error pages into logs.
		snippet := string(body)
		if len(snippet) > 200 {
			snippet = snippet[:200] + "...[truncated]"
		}
		return nil, fmt.Errorf("arr: http %d: %s", resp.StatusCode, snippet)
	}
	return body, nil
}

// ----- Sonarr -----

type sonarrClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func newSonarrClient(baseURL, apiKey string, timeout time.Duration) arrClient {
	return &sonarrClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: timeout},
	}
}

func (c *sonarrClient) Source() Source { return SourceSonarr }

// sonarrSeries is the projection of /api/v3/series we care about.
// Sonarr returns much more; we deserialise only what's needed for
// the canonical fingerprint + the push target.
type sonarrSeries struct {
	ID        int    `json:"id"`
	Title     string `json:"title"`
	Year      int    `json:"year"`
	Monitored bool   `json:"monitored"`
	Status    string `json:"status"`
	Seasons   []struct {
		SeasonNumber int  `json:"seasonNumber"`
		Monitored    bool `json:"monitored"`
	} `json:"seasons"`
}

func (c *sonarrClient) Fetch(ctx context.Context) ([]wantEntry, error) {
	body, err := arrHTTPDo(ctx, c.http, c.baseURL, c.apiKey, "/api/v3/series")
	if err != nil {
		return nil, err
	}
	var series []sonarrSeries
	if err := json.Unmarshal(body, &series); err != nil {
		return nil, fmt.Errorf("sonarr: parse series: %w", err)
	}
	var out []wantEntry
	for _, s := range series {
		if !s.Monitored {
			continue
		}
		// One canonical key per monitored season. Season 0
		// (specials) is included if monitored — operator's
		// choice.
		title := strings.ToLower(strings.TrimSpace(s.Title))
		if title == "" {
			continue
		}
		hasMonitoredSeason := false
		for _, season := range s.Seasons {
			if !season.Monitored {
				continue
			}
			hasMonitoredSeason = true
			out = append(out, wantEntry{
				Source: SourceSonarr,
				Canonical: Canonical{
					Kind:   KindTV,
					Title:  title,
					Year:   s.Year,
					Season: season.SeasonNumber,
				},
				PushTarget: PushTarget{
					Source:         SourceSonarr,
					SonarrSeriesID: s.ID,
					Title:          s.Title,
					Year:           s.Year,
					Season:         season.SeasonNumber,
				},
			})
		}
		// Series-level fallback so a torrent with no season marker
		// can still match. Mirrors the canonicalKeys() expansion.
		if !hasMonitoredSeason {
			// Series monitored but no monitored seasons listed —
			// rare but possible. Skip (no actionable target).
			continue
		}
	}
	return out, nil
}

// ----- Radarr -----

type radarrClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func newRadarrClient(baseURL, apiKey string, timeout time.Duration) arrClient {
	return &radarrClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: timeout},
	}
}

func (c *radarrClient) Source() Source { return SourceRadarr }

type radarrMovie struct {
	ID        int    `json:"id"`
	Title     string `json:"title"`
	Year      int    `json:"year"`
	Monitored bool   `json:"monitored"`
	Status    string `json:"status"`
	HasFile   bool   `json:"hasFile"`
}

func (c *radarrClient) Fetch(ctx context.Context) ([]wantEntry, error) {
	body, err := arrHTTPDo(ctx, c.http, c.baseURL, c.apiKey, "/api/v3/movie")
	if err != nil {
		return nil, err
	}
	var movies []radarrMovie
	if err := json.Unmarshal(body, &movies); err != nil {
		return nil, fmt.Errorf("radarr: parse movies: %w", err)
	}
	var out []wantEntry
	for _, m := range movies {
		if !m.Monitored {
			continue
		}
		// Skip already-grabbed movies — Radarr handles upgrades on
		// its own search schedule, no need to flood the bridge.
		if m.HasFile {
			continue
		}
		title := strings.ToLower(strings.TrimSpace(m.Title))
		if title == "" {
			continue
		}
		out = append(out, wantEntry{
			Source: SourceRadarr,
			Canonical: Canonical{
				Kind:   KindMovie,
				Title:  title,
				Year:   m.Year,
				Season: -1,
			},
			PushTarget: PushTarget{
				Source:        SourceRadarr,
				RadarrMovieID: m.ID,
				Title:         m.Title,
				Year:          m.Year,
				Season:        -1,
			},
		})
	}
	return out, nil
}

// ----- Lidarr -----

type lidarrClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func newLidarrClient(baseURL, apiKey string, timeout time.Duration) arrClient {
	return &lidarrClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: timeout},
	}
}

func (c *lidarrClient) Source() Source { return SourceLidarr }

type lidarrArtist struct {
	ID        int    `json:"id"`
	ArtistName string `json:"artistName"`
	Monitored bool   `json:"monitored"`
	Status    string `json:"status"`
}

func (c *lidarrClient) Fetch(ctx context.Context) ([]wantEntry, error) {
	body, err := arrHTTPDo(ctx, c.http, c.baseURL, c.apiKey, "/api/v1/artist")
	if err != nil {
		return nil, err
	}
	var artists []lidarrArtist
	if err := json.Unmarshal(body, &artists); err != nil {
		return nil, fmt.Errorf("lidarr: parse artists: %w", err)
	}
	var out []wantEntry
	for _, a := range artists {
		if !a.Monitored {
			continue
		}
		title := strings.ToLower(strings.TrimSpace(a.ArtistName))
		if title == "" {
			continue
		}
		out = append(out, wantEntry{
			Source: SourceLidarr,
			Canonical: Canonical{
				Kind:   KindMusic,
				Title:  title,
				Season: -1,
			},
			PushTarget: PushTarget{
				Source:         SourceLidarr,
				LidarrArtistID: a.ID,
				Title:          a.ArtistName,
				Season:         -1,
			},
		})
	}
	return out, nil
}

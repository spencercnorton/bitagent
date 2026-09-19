package animedb

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"
)

// maxDownloadBytes caps a single source download. The real files are a few MB;
// this is a sanity ceiling against a misconfigured URL, not a tight bound.
const maxDownloadBytes = 128 << 20 // 128 MiB

// RefreshStats reports one refresh cycle's outcome.
type RefreshStats struct {
	Mappings int  // TMDB-mapped anime entries parsed
	Titles   int  // alias lines parsed (after language filter)
	Aliases  int  // deduplicated alias rows built
	Wrote    bool // whether the build was persisted
	Rows     int  // rows written (0 in dry-run)
}

// Runner performs one anime-titles refresh: download both source files
// (conditional GET, cached, offline-fallback), parse, join+dedup into the alias
// set, and — in write mode — replace the table and hot-swap the resolver.
type Runner struct {
	cfg      Config
	store    *Store
	resolver *Resolver // may be nil (CLI without a live resolver)
	metrics  *Metrics
	logger   *zap.SugaredLogger
	http     *http.Client
}

func NewRunner(cfg Config, store *Store, resolver *Resolver, metrics *Metrics, logger *zap.SugaredLogger) *Runner {
	return &Runner{
		cfg:      cfg,
		store:    store,
		resolver: resolver,
		metrics:  metrics,
		logger:   logger,
		http:     &http.Client{Timeout: cfg.HttpTimeout},
	}
}

// Refresh runs one cycle. In dry-run (write=false) it downloads, parses and
// builds but persists nothing. A non-nil error is a genuine failure of one
// stage; the stage is recorded on the error metric.
func (r *Runner) Refresh(ctx context.Context, write bool) (RefreshStats, error) {
	start := time.Now()

	xmlBytes, err := r.fetch(ctx, r.cfg.AnimeListUrl, "anime-list-full.xml")
	if err != nil {
		r.recordErr("download")
		return RefreshStats{}, fmt.Errorf("animedb: fetch anime-list: %w", err)
	}
	datBytes, err := r.fetch(ctx, r.cfg.AnidbTitlesUrl, "anime-titles.dat")
	if err != nil {
		r.recordErr("download")
		return RefreshStats{}, fmt.Errorf("animedb: fetch anime-titles: %w", err)
	}

	mappings, err := ParseAnimeList(bytes.NewReader(xmlBytes))
	if err != nil {
		r.recordErr("parse")
		return RefreshStats{}, err
	}
	datReader, err := maybeGunzip(datBytes)
	if err != nil {
		r.recordErr("parse")
		return RefreshStats{}, err
	}
	titles, err := ParseAnimeTitles(datReader, languageSet(r.cfg.Languages))
	if err != nil {
		r.recordErr("parse")
		return RefreshStats{}, err
	}

	aliases := BuildAliases(mappings, titles)
	stats := RefreshStats{Mappings: len(mappings), Titles: len(titles), Aliases: len(aliases)}

	if r.metrics != nil {
		r.metrics.mappingsBuilt.Set(float64(len(mappings)))
		r.metrics.titlesBuilt.Set(float64(len(titles)))
		r.metrics.aliasesBuilt.Set(float64(len(aliases)))
	}

	if write {
		rows, err := r.store.ReplaceAll(ctx, aliases)
		if err != nil {
			r.recordErr("persist")
			return stats, err
		}
		stats.Wrote = true
		stats.Rows = rows
		if r.resolver != nil {
			r.resolver.Swap(aliases)
		}
		if r.metrics != nil {
			r.metrics.tableRows.Set(float64(rows))
		}
	}

	if r.metrics != nil {
		r.metrics.refreshTotal.Inc()
		r.metrics.refreshDuration.Observe(time.Since(start).Seconds())
	}
	return stats, nil
}

// fetch downloads url with a conditional GET against the cached copy, writing a
// fresh cache on 200 and falling back to the cache on 304, transport error, or
// non-OK status (offline resilience). The cache file's mtime drives
// If-Modified-Since, so a good upstream citizen re-downloads only on change.
func (r *Runner) fetch(ctx context.Context, url, cacheName string) ([]byte, error) {
	cachePath := filepath.Join(r.cacheDir(), cacheName)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if r.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", r.cfg.UserAgent)
	}
	if fi, statErr := os.Stat(cachePath); statErr == nil {
		req.Header.Set("If-Modified-Since", fi.ModTime().UTC().Format(http.TimeFormat))
	}

	resp, err := r.http.Do(req)
	if err != nil {
		if b, rerr := os.ReadFile(cachePath); rerr == nil {
			r.logf("animedb: %s download failed (%v); using cache", cacheName, err)
			return b, nil
		}
		return nil, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		return os.ReadFile(cachePath)
	case http.StatusOK:
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes))
		if err != nil {
			return nil, err
		}
		r.writeCache(cachePath, b)
		return b, nil
	default:
		if b, rerr := os.ReadFile(cachePath); rerr == nil {
			r.logf("animedb: %s returned http %d; using cache", cacheName, resp.StatusCode)
			return b, nil
		}
		return nil, fmt.Errorf("http %d fetching %s", resp.StatusCode, url)
	}
}

func (r *Runner) cacheDir() string {
	if r.cfg.CacheDir != "" {
		return r.cfg.CacheDir
	}
	return filepath.Join(os.TempDir(), "bitagent-animedb")
}

func (r *Runner) writeCache(path string, b []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.logf("animedb: cache mkdir failed: %v", err)
		return
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		r.logf("animedb: cache write failed: %v", err)
	}
}

func (r *Runner) recordErr(stage string) {
	if r.metrics != nil {
		r.metrics.refreshErrors.WithLabelValues(stage).Inc()
	}
}

func (r *Runner) logf(format string, args ...any) {
	if r.logger != nil {
		r.logger.Warnf(format, args...)
	}
}

// maybeGunzip transparently decompresses a gzip payload (the AniDB dump ships
// as .gz), detected by the gzip magic bytes so it works regardless of the URL
// suffix or a cached-decompressed copy.
func maybeGunzip(b []byte) (io.Reader, error) {
	if len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("animedb: gunzip: %w", err)
		}
		return zr, nil
	}
	return bytes.NewReader(b), nil
}

// languageSet builds a membership set from the configured language whitelist.
// An empty slice yields a nil set, which ParseAnimeTitles treats as "keep all".
func languageSet(langs []string) map[string]bool {
	if len(langs) == 0 {
		return nil
	}
	m := make(map[string]bool, len(langs))
	for _, l := range langs {
		m[l] = true
	}
	return m
}

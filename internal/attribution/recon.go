package attribution

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Logger is the minimal interface the orchestrator needs. Callers
// pass a *zap.SugaredLogger or a noop in tests.
type Logger interface {
	Infow(msg string, kv ...interface{})
	Warnw(msg string, kv ...interface{})
}

// noopLogger satisfies Logger without doing anything; used when
// the caller passes nil.
type noopLogger struct{}

func (noopLogger) Infow(string, ...interface{}) {}
func (noopLogger) Warnw(string, ...interface{}) {}

// ReconReport is the full output of one Run() call. The CLI
// renders this to a table; tests assert against the structured
// fields.
type ReconReport struct {
	Filters Filters
	Rows    []Row
	Summary Summary
	// Warnings collects per-source non-fatal failures so the CLI
	// can surface them without aborting the whole run.
	Warnings []string
}

// Run is the orchestrator. Pulls grabs from Prowlarr, qB state for
// each grab, and *arr import outcomes. Joins by info_hash and
// returns a ReconReport.
//
// Fail-open semantics: any single source can fail; the report
// proceeds with the remaining data. Only Prowlarr being absent is
// fatal because we have no chain anchor without it.
func Run(ctx context.Context, cfg Config, filters Filters, logger Logger) (ReconReport, error) {
	if logger == nil {
		logger = noopLogger{}
	}
	if !cfg.Enabled {
		return ReconReport{}, errors.New("attribution: ATTRIBUTION_ENABLED=false; the recon CLI refuses to run until the operator opts in")
	}
	if !cfg.HasProwlarr() {
		return ReconReport{}, errors.New("attribution: Prowlarr URL+API key required; without them the chain has no anchor")
	}

	timeout, err := time.ParseDuration(cfg.HTTPTimeout)
	if err != nil || timeout <= 0 {
		timeout = 30 * time.Second
	}

	report := ReconReport{Filters: filters}

	// 1. Pull grab events from Prowlarr.
	prowl := newProwlarrClient(cfg.ProwlarrBaseURL, cfg.ProwlarrAPIKey, timeout)
	logger.Infow("attribution: fetching prowlarr history",
		"since_hours", filters.SinceHours,
		"indexer_filter", filters.IndexerSubstring)
	grabs, err := prowl.FetchGrabs(ctx, filters.SinceHours, filters.Limit*4 /* over-fetch for filtering */)
	if err != nil {
		return report, err
	}
	logger.Infow("attribution: prowlarr returned grabs", "n", len(grabs))

	// Apply indexer filter post-hoc.
	if filters.IndexerSubstring != "" {
		grabs = filterByIndexer(grabs, filters.IndexerSubstring)
	}
	if filters.Limit > 0 && len(grabs) > filters.Limit {
		grabs = grabs[:filters.Limit]
	}

	// Collect info_hashes for the qB lookup.
	hashes := make([]string, 0, len(grabs))
	seen := map[string]bool{}
	for _, g := range grabs {
		if g.InfoHash == "" || seen[g.InfoHash] {
			continue
		}
		seen[g.InfoHash] = true
		hashes = append(hashes, g.InfoHash)
	}

	// 2. qB state lookup (best-effort).
	qbStates := map[string]QBState{}
	if cfg.HasQB() {
		qb, qbErr := newQBTClient(cfg.QBTBaseURL, cfg.QBTUsername, cfg.QBTPassword, timeout)
		if qbErr != nil {
			report.Warnings = append(report.Warnings, "qBittorrent: client init: "+qbErr.Error())
			logger.Warnw("attribution: qb client init failed", "err", qbErr.Error())
		} else if loginErr := qb.Login(ctx); loginErr != nil {
			report.Warnings = append(report.Warnings, "qBittorrent: login: "+loginErr.Error())
			logger.Warnw("attribution: qb login failed", "err", loginErr.Error())
		} else {
			states, lookupErr := qb.LookupHashes(ctx, hashes)
			if lookupErr != nil {
				report.Warnings = append(report.Warnings, "qBittorrent: lookup: "+lookupErr.Error())
				logger.Warnw("attribution: qb lookup failed", "err", lookupErr.Error())
			} else {
				qbStates = states
				logger.Infow("attribution: qb state collected", "torrents", len(states))
			}
		}
	} else {
		report.Warnings = append(report.Warnings, "qBittorrent: not configured; skipping download-state column")
	}

	// 3. *arr history lookup, one per source.
	importByHash := map[string]ArrImport{}
	for _, src := range []Source{SourceSonarr, SourceRadarr, SourceLidarr} {
		baseURL, apiKey := cfg.ArrEndpoint(src)
		if baseURL == "" || apiKey == "" {
			continue
		}
		ac := newArrHistoryClient(src, baseURL, apiKey, timeout)
		hist, err := ac.LookupRecent(ctx, 200)
		if err != nil {
			report.Warnings = append(report.Warnings,
				string(src)+": history: "+err.Error())
			logger.Warnw("attribution: arr history failed",
				"source", string(src), "err", err.Error())
			continue
		}
		// Merge: later sources don't overwrite earlier — the
		// matching source for each grab takes precedence anyway via
		// the join logic below (we only use importByHash for the
		// matching source per row).
		for h, imp := range hist {
			if _, exists := importByHash[h]; exists {
				continue
			}
			importByHash[h] = imp
		}
		logger.Infow("attribution: arr history collected",
			"source", string(src), "rows", len(hist))
	}

	// 4. Join.
	rows := make([]Row, 0, len(grabs))
	for _, g := range grabs {
		row := Row{Grab: g}
		if g.InfoHash != "" {
			if qb, ok := qbStates[g.InfoHash]; ok {
				row.QB = qb
			}
			if imp, ok := importByHash[g.InfoHash]; ok {
				// Only attach the import if its source matches the
				// grab source (or the source is unknown). Avoids
				// cross-source contamination on shared hashes (rare).
				if imp.Source == g.Source || g.Source == "" {
					row.Import = imp
				}
			}
		}
		rows = append(rows, row)
	}

	// 5. Summary.
	report.Rows = rows
	report.Summary = computeSummary(rows)
	return report, nil
}

func filterByIndexer(grabs []GrabEvent, sub string) []GrabEvent {
	subLower := strings.ToLower(sub)
	out := grabs[:0]
	for _, g := range grabs {
		if strings.Contains(strings.ToLower(g.Indexer), subLower) {
			out = append(out, g)
		}
	}
	return out
}

func computeSummary(rows []Row) Summary {
	s := Summary{
		ByVerdict: map[string]int{},
		BySource:  map[Source]int{},
	}
	s.GrabsTotal = len(rows)
	for _, r := range rows {
		s.ByVerdict[r.Verdict()]++
		if r.Grab.Source != "" {
			s.BySource[r.Grab.Source]++
		}
		idx := strings.ToLower(r.Grab.Indexer)
		if strings.Contains(idx, "bitagent") || strings.Contains(idx, "bitmagnet") {
			s.BitAgentN++
		}
	}
	return s
}

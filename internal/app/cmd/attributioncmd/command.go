// Package attributioncmd registers the `bitagent attribution`
// CLI subcommand. Today it only has one subcommand — `recon` — that
// runs a one-shot end-to-end correlation report (Prowlarr grab →
// qB state → *arr import).
//
// Why a CLI rather than a worker: the senior review (gpt-5.5-pro,
// 2026-04-25) flagged "build end-to-end attribution" as the new
// top priority because every "8.4% grab rate / 18% liveness miss"
// claim was untested. A CLI gives the operator immediate validation
// without the design overhead of a persistent table + worker. A
// later MR can promote this to a periodic worker once the report
// shape is settled.
package attributioncmd

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spencercnorton/bitagent/internal/attribution"
	"github.com/spencercnorton/bitagent/internal/wantbridge"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type Params struct {
	fx.In
	AttributionConfig attribution.Config
	WantbridgeConfig  wantbridge.Config // for *arr URL/key fallback
	Logger            *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Command *cli.Command `group:"commands"`
}

func New(p Params) (Result, error) {
	cmd := &cli.Command{
		Name:  "attribution",
		Usage: "End-to-end correlation between BitAgent torznab results, Prowlarr grabs, qBittorrent state, and *arr import outcomes",
		Subcommands: []*cli.Command{
			{
				Name:  "recon",
				Usage: "One-shot correlation report (no DB writes, prints a table)",
				Flags: []cli.Flag{
					&cli.IntFlag{
						Name:  "since-hours",
						Value: 24,
						Usage: "How far back to read Prowlarr's grab history",
					},
					&cli.StringFlag{
						Name:  "indexer",
						Value: "BitMagnet",
						Usage: "Substring filter on Prowlarr indexer name (case-insensitive). Empty for all indexers.",
					},
					&cli.IntFlag{
						Name:  "limit",
						Value: 100,
						Usage: "Cap the number of grab events shown. 0 = no cap.",
					},
				},
				Action: func(ctx *cli.Context) error {
					return runRecon(ctx, p)
				},
			},
		},
	}
	return Result{Command: cmd}, nil
}

func runRecon(ctx *cli.Context, p Params) error {
	cfg := mergeArrFallback(p.AttributionConfig, p.WantbridgeConfig)
	filters := attribution.Filters{
		SinceHours:       ctx.Int("since-hours"),
		IndexerSubstring: ctx.String("indexer"),
		Limit:            ctx.Int("limit"),
	}

	report, err := attribution.Run(ctx.Context, cfg, filters, p.Logger.Named("attribution"))
	if err != nil {
		return err
	}

	renderReport(ctx.App.Writer, report)
	if len(report.Warnings) > 0 {
		_, _ = fmt.Fprintln(os.Stderr)
		_, _ = fmt.Fprintln(os.Stderr, "warnings (non-fatal):")
		for _, w := range report.Warnings {
			_, _ = fmt.Fprintln(os.Stderr, "  -", w)
		}
	}
	return nil
}

// mergeArrFallback fills in missing *arr URL+key from the wantbridge
// config so the operator doesn't double-configure. ATTRIBUTION_*
// values take precedence; wantbridge fills the gaps.
func mergeArrFallback(a attribution.Config, w wantbridge.Config) attribution.Config {
	if a.SonarrBaseURL == "" {
		a.SonarrBaseURL = w.SonarrBaseURL
	}
	if a.SonarrAPIKey == "" {
		a.SonarrAPIKey = w.SonarrAPIKey
	}
	if a.RadarrBaseURL == "" {
		a.RadarrBaseURL = w.RadarrBaseURL
	}
	if a.RadarrAPIKey == "" {
		a.RadarrAPIKey = w.RadarrAPIKey
	}
	if a.LidarrBaseURL == "" {
		a.LidarrBaseURL = w.LidarrBaseURL
	}
	if a.LidarrAPIKey == "" {
		a.LidarrAPIKey = w.LidarrAPIKey
	}
	return a
}

func renderReport(w interface {
	Write(p []byte) (n int, err error)
}, report attribution.ReconReport) {
	// --- Summary block first ---
	fmt.Fprintf(w, "Attribution recon — last %d hours, indexer ~ %q\n",
		report.Filters.SinceHours,
		report.Filters.IndexerSubstring)
	fmt.Fprintf(w, "Total grabs: %d (BitMagnet: %d)\n",
		report.Summary.GrabsTotal, report.Summary.BitAgentN)
	if len(report.Summary.BySource) > 0 {
		var sources []Source
		for s := range report.Summary.BySource {
			sources = append(sources, Source(s))
		}
		sort.Slice(sources, func(i, j int) bool { return sources[i] < sources[j] })
		var parts []string
		for _, s := range sources {
			parts = append(parts, fmt.Sprintf("%s=%d", s, report.Summary.BySource[attribution.Source(s)]))
		}
		fmt.Fprintf(w, "By *arr source: %s\n", strings.Join(parts, " "))
	}
	if len(report.Summary.ByVerdict) > 0 {
		// Show in a stable, meaningful order.
		order := []string{
			"imported_ok",
			"completed_pending_import",
			"downloading",
			"stalled",
			"missing_in_qb",
			"import_failed",
			"unknown",
		}
		var parts []string
		for _, v := range order {
			if n, ok := report.Summary.ByVerdict[v]; ok {
				parts = append(parts, fmt.Sprintf("%s=%d", v, n))
			}
		}
		fmt.Fprintf(w, "By verdict:    %s\n", strings.Join(parts, " "))
	}
	fmt.Fprintln(w)

	// --- Per-row table ---
	tw := table.NewWriter()
	tw.SetOutputMirror(w)
	tw.AppendHeader(table.Row{
		"grabbed_at", "src", "indexer", "verdict",
		"qb_state", "qb_progress", "import", "title",
	})
	for _, r := range report.Rows {
		title := r.Grab.Title
		if len(title) > 60 {
			title = title[:57] + "..."
		}
		qbState := "-"
		qbProgress := ""
		if r.QB.Found {
			qbState = r.QB.State
			qbProgress = fmt.Sprintf("%.0f%%", r.QB.Progress*100)
		}
		importStr := "-"
		if r.Import.Found {
			importStr = r.Import.EventType
		}
		grabbedAt := r.Grab.GrabbedAt.Format(time.RFC3339)
		tw.AppendRow(table.Row{
			grabbedAt[:16], // YYYY-MM-DDTHH:MM
			string(r.Grab.Source),
			truncate(r.Grab.Indexer, 25),
			r.Verdict(),
			qbState,
			qbProgress,
			importStr,
			title,
		})
	}
	tw.Render()
}

// Source is locally aliased for the sort.Slice import readability.
type Source = attribution.Source

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

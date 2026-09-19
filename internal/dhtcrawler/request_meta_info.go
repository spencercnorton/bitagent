package dhtcrawler

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"

	anacrolixmetainfo "github.com/anacrolix/torrent/metainfo"

	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/protocol/metainfo/metainforequester"
)

// errCsamBlocklistBlocked is returned by doRequestMetaInfo when the
// CSAM blocklist's defense-in-depth IsBlocked check rejects a hash
// at the network-egress site. Distinct sentinel so peerrep / banning
// logic can recognise this rejection without parsing the message.
var errCsamBlocklistBlocked = errors.New("csam blocklist: hash blocked, BEP-9 fetch refused")

func (c *crawler) runRequestMetaInfo(ctx context.Context) {
	_ = c.requestMetaInfo.Run(ctx, func(req infoHashWithPeers) {
		mi, reqErr := c.doRequestMetaInfo(ctx, req.infoHash, req.peers)
		if reqErr != nil {
			return
		}

		// Content filter — operator's English-only / format / NSFW
		// curation gate, runs before the torrent is even queued for
		// persistence. The filter is enabled+enforced via env
		// (CONTENT_FILTER_ENABLED, CONTENT_FILTER_ENFORCE), so
		// disabled deploys are pure no-ops here.
		//
		// Two contracts to keep here:
		//
		//   1. Gating on Enabled() short-circuits BOTH Decide() and
		//      metrics.Observe() so a disabled filter contributes
		//      nothing — not even examined_total / keep_total. The
		//      config doc on Enabled is explicit: "no metrics are
		//      emitted." (review finding, fixed.)
		//
		//   2. This is a PRE-CLASSIFIER hook. The CEL classifier
		//      hasn't run yet, so Input.Languages and
		//      Input.ContentType are empty here. We call
		//      DecideDeterministic so the deterministic ladder
		//      (script / extension / NSFW keyword / mp3-only)
		//      fires while skipping the LLM tier — calling
		//      Decide here would key off "Languages empty" as
		//      residual-cohort and burn cache / budget on every
		//      Latin-script title regardless of whether it's
		//      actually English. The LLM tier belongs at a
		//      post-classifier hook (separate follow-up MR).
		//      (review finding, fixed.)
		if c.contentFilter != nil && c.contentFilter.Enabled() {
			fi := metaInfoToFilterInput(mi.Info.Name, mi.Info.Files)
			d := c.contentFilter.DecideDeterministic(fi)
			if c.contentFilterMetrics != nil {
				c.contentFilterMetrics.Observe(d)
			}
			if !d.Allow {
				// Live drop: skip the persist queue entirely.
				return
			}
		}

		// Wantbridge — *arr wantlist match against the just-fetched torrent
		// name. Match() internally fires the wantbridge metrics
		// (`bitagent_wantbridge_matches_total{tier,source}`) via the
		// ServiceCallbacks wired in wantbridgefx. The dhtcrawler does not
		// own its own counter here.
		//
		// `c.wantbridge` is always non-nil: wantbridgefx returns a NoOp
		// implementation when WANTBRIDGE_ENABLED=false, which short-
		// circuits internally and allocates nothing on the hot path.
		// Match takes only an RWMutex read lock; the poll-time write lock
		// only contends every PollInterval (default 5min) and holds for
		// sub-millisecond.
		//
		// Order is contentfilter first, then wantbridge: the cheap
		// deterministic ladder eliminates obvious junk before we spend a
		// wantlist lookup on torrents we'd drop anyway. The Match result
		// is consumed by metrics only at this MR; the dhtcrawler does
		// not yet act on the tier (priority queue / persist annotation
		// are deliberate follow-ups so this MR stays additive).
		_ = c.wantbridge.Match(mi.Info.Name)

		select {
		case <-ctx.Done():
		case c.persistTorrents.In() <- infoHashWithMetaInfo{
			nodeHasPeersForHash: req.nodeHasPeersForHash,
			metaInfo:            mi.Info,
		}:
		}
	})
}

// metaInfoToFilterInput projects the BEP-9 metainfo into the
// contentfilter.Input shape. Picks the "primary" file as the largest
// file by size (single-file torrents get that file directly).
//
// Why size, not first: a torrent like "Some Movie 2024 [1080p].mkv"
// might also include subtitles (.srt), info (.nfo), and a sample
// (.mp4) — the largest is the actual video. The filter cares about
// the largest-by-size file's extension as the "primary content
// type" signal.
func metaInfoToFilterInput(name string, files []anacrolixmetainfo.FileInfo) contentfilter.Input {
	allExtsSet := map[string]struct{}{}
	primaryExt := ""
	var primarySize int64

	addExt := func(path string) string {
		// path may be slash- or backslash-separated; tolerate both
		base := filepath.Base(strings.ReplaceAll(path, "\\", "/"))
		ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(base)), ".")
		if ext != "" {
			allExtsSet[ext] = struct{}{}
		}
		return ext
	}

	if len(files) == 0 {
		// Single-file torrent — name is the filename itself.
		primaryExt = addExt(name)
	} else {
		for _, f := range files {
			ext := addExt(filepath.Join(f.Path...))
			if ext == "" {
				continue
			}
			if f.Length > primarySize {
				primarySize = f.Length
				primaryExt = ext
			}
		}
	}

	allExts := make([]string, 0, len(allExtsSet))
	for e := range allExtsSet {
		allExts = append(allExts, e)
	}

	return contentfilter.Input{
		Title:            name,
		PrimaryExtension: primaryExt,
		AllExtensions:    allExts,
		// ContentType + Languages are intentionally empty here —
		// the classifier hasn't run yet. The post-classify hook (a
		// separate, future MR) populates those.
	}
}

func (c *crawler) doRequestMetaInfo(
	ctx context.Context,
	hash protocol.ID,
	peers []netip.AddrPort,
) (metainforequester.Response, error) {
	// Defense-in-depth: even though infohash_triage already ran the
	// CSAM blocklist Filter on this hash's batch, re-check at the
	// point of network egress. Triage and request_meta_info are
	// decoupled by channels — a feed refresh between them could
	// introduce a new hash to the blocklist after triage cleared it.
	// The IsBlocked call is a single bloom-test, O(1), no allocation.
	// Worth it to guarantee no BEP-9 fetch touches a community-known
	// CSAM swarm. See issue #494.
	if c.csamBlocklist.IsBlocked(hash) {
		// Phase-C: record the egress re-check reject (feed refresh added
		// this hash between triage and here). Recording-only.
		c.recordCsamReject(ctx, hash, "egress")
		return metainforequester.Response{}, errCsamBlocklistBlocked
	}

	var errs []error

	errsMutex := sync.Mutex{}
	addErr := func(err error) {
		errsMutex.Lock()
		errs = append(errs, err)
		errsMutex.Unlock()
	}

	for _, p := range peers {
		res, err := c.metainfoRequester.Request(ctx, hash, p)
		if err != nil {
			addErr(err)
			continue
		}

		if banErr := c.banningChecker.Check(res.Info); banErr != nil {
			_ = c.blockingManager.Block(ctx, []protocol.ID{hash}, false)
			// Phase-C: dual-write a blocking verdict for the ban-driven
			// block. Recording-only, after the block (never rolls it back).
			c.recordBlockingVerdict(ctx, hash)
			return metainforequester.Response{}, banErr
		}

		return res, nil
	}

	return metainforequester.Response{}, errors.Join(errs...)
}

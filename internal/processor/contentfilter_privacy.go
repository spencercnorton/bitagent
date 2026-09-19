package processor

import (
	"context"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
)

// PrivacyStore is the gate the post-classifier contentfilter hook
// uses to decide whether a torrent originated from a private tracker.
// Mirrors classifier/llmstage.PrivacyStore intentionally so we use
// the same evidence.Store implementation in production.
//
// Why this gate exists separately from llmstage's: llmstage's gate
// only fires on the ErrUnmatched path (CEL classifier couldn't
// decide). When the CEL classifier matches successfully — the COMMON
// case for *arr-imported private-tracker content with a populated
// ContentSource hint — llmstage short-circuits and never consults its
// privacy gate. Without the gate here, those private torrents would
// reach the contentfilter LLM and the title would be sent to OpenAI.
//
// The contentfilter LLM only fires on the residual cohort (Latin
// script + empty Languages tag), so the leak window is narrow but
// real: a private-tracker movie/show whose CEL classification didn't
// produce a language tag (TMDB miss, or *arr webhook arrived without
// language metadata) would be eligible.
type PrivacyStore interface {
	IsPrivateInfoHash(ctx context.Context, infoHash []byte) (bool, error)
}

const llmMatchedTagName = "llm-matched"

// shouldSkipContentFilter returns true when the content-filter
// post-classifier hook MUST be skipped because either the LLM matcher already
// accepted the torrent, or the torrent originated from a private tracker. The
// skip is total: neither the deterministic ladder nor the LLM tier runs, and
// no metric is emitted ("the filter never saw it").
//
// The LLM-match skip is deliberately before the privacy lookup. The matcher
// already ran its own privacy gate before any OpenAI call, and the post-filter
// has less context than the matcher. In particular, English-subbed anime is
// usually attached to TMDB content whose original language is "ja"; running the
// generic language filter after a successful anime English-track decision would
// delete the exact rows the matcher recovered.
//
// Fail-closed semantics on store error: if the privacy lookup errors
// (DB hiccup, transient pgpool issue), we treat the torrent as
// "potentially private" and skip the filter. Better an
// under-curated public torrent than a leaked private title.
//
// nil-safe: returns false (don't skip) when the store isn't wired,
// because in that environment the FX wiring promised no privacy
// requirement (e.g. a unit-test harness with no evidence store). Real
// production wiring always provides a store via factory.go.
func (c processor) shouldSkipContentFilter(
	ctx context.Context,
	t model.Torrent,
	cl classification.Result,
) bool {
	if _, ok := cl.Tags[llmMatchedTagName]; ok {
		return true
	}
	if c.privacy == nil {
		return false
	}
	isPriv, err := c.privacy.IsPrivateInfoHash(ctx, t.InfoHash.Bytes())
	if err != nil {
		// Fail closed.
		return true
	}
	return isPriv
}

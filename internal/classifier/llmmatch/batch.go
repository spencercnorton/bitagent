package llmmatch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spencercnorton/bitagent/internal/model"
)

// The batch extract path exists for backfill/reprocess runs over the unmatched
// backlog: one-call-per-torrent doesn't scale to millions of names on a local
// model, but the stage-1 prompt batches naturally (N names in, N extractions
// out). Grouped responses are reusable only for the exact same group request.
// They cannot warm ordinary Extract: its system/input/output contract differs,
// even when a group's individual name also appears in a single-item request.
const batchExtractSystemPrompt = `You extract the canonical media identity from EACH torrent release name in a numbered list. Output ONLY compact JSON: {"items":[{"id":int,"title":string,"year":int,"type":"movie"|"tv","season":int,"episode":int,"is_anime":bool,"english":"dub"|"sub"|"none"|"unknown","is_pack":bool,"is_adult":bool}]} with exactly one item per input line, echoing that line's id unchanged. Never skip, merge, or invent ids. If you do not recognise a title for a line, still emit its item with title "".

` + extractRules

// minBatchSplit: a failing batch is split in half (isolating a poison item)
// until it is smaller than this, then falls back to per-name Extract calls.
const minBatchSplit = 8

// ValidateBacklogExtraction rejects grouped warming for a workflow that later
// consumes the distinct single-item contract. Capture mode already sends
// singles; otherwise the operator must select a single-request batch size.
func (c *Client) ValidateBacklogExtraction(batchSize int) error {
	if batchSize < minBatchSplit || (c.capture != nil && c.capture.Enabled()) {
		return nil
	}
	return fmt.Errorf("grouped extraction cannot warm single-item workflow requests; set llmBatchSize below %d or enable evaluation capture", minBatchSplit)
}

// Batch calls scale the completion budget and request deadline with size — a
// 40-name batch legitimately needs longer than the single-call Timeout at
// local-model generation speeds (~60-70 output tokens per item).
const (
	batchBaseMaxTokens    = 100
	batchPerItemMaxTokens = 130
	batchPerItemTimeout   = 3 * time.Second
)

// BatchStats summarises an ExtractMany run.
type BatchStats struct {
	Requested int // torrents passed in
	Gated     int // skipped because the native private flag forbids model use
	Cached    int // skipped: already decided (or a duplicate name in the input)
	OK        int // extractions with a recognised title
	Empty     int // the model declined to name a title
	Failed    int // items that errored even after single-call fallback
	Batches   int // batch prompts issued (including split retries)
	Singles   int // per-name fallback calls
}

// Add accumulates another run's counters (for per-page totals in callers).
func (s *BatchStats) Add(o BatchStats) {
	s.Requested += o.Requested
	s.Gated += o.Gated
	s.Cached += o.Cached
	s.OK += o.OK
	s.Empty += o.Empty
	s.Failed += o.Failed
	s.Batches += o.Batches
	s.Singles += o.Singles
}

// ExtractMany evaluates the given torrents using grouped or single prompts.
// Genuine single decisions and identical group requests are reused within
// their own contracts. Duplicate exact single inputs are skipped. The
// caller remains responsible for the broader Allow gate, but the native
// private flag is enforced again here so no caller can batch a private name to
// the endpoint when evidence is absent or stale.
func (c *Client) ExtractMany(ctx context.Context, torrents []model.Torrent, batchSize int) BatchStats {
	st := BatchStats{Requested: len(torrents)}
	if batchSize < 1 {
		batchSize = 1
	}

	pending := make([]model.Torrent, 0, len(torrents))
	seen := make(map[string]struct{}, len(torrents))
	for _, t := range torrents {
		if !c.nameAllowed(ctx, t) || c.nativePrivateBlocked(t) {
			st.Gated++
			continue
		}
		key := c.extractKey(t)
		if _, dup := seen[key]; dup {
			st.Cached++
			continue
		}
		seen[key] = struct{}{}
		if _, ok := c.cache.Get(key); ok {
			st.Cached++
			continue
		}
		pending = append(pending, t)
	}

	for start := 0; start < len(pending) && ctx.Err() == nil; start += batchSize {
		end := min(start+batchSize, len(pending))
		c.extractChunk(ctx, pending[start:end], &st)
	}
	return st
}

func (c *Client) extractChunk(ctx context.Context, chunk []model.Torrent, st *BatchStats) {
	if len(chunk) == 0 || ctx.Err() != nil {
		return
	}
	// A multi-name prompt entangles otherwise independent corpus cases. While
	// prospective capture is enabled, use the ordinary production single-item
	// boundary so every actual extraction has an exact, independently
	// replayable input. Disabled deployments retain the existing batching.
	if c.namePolicy.Enabled() || (c.capture != nil && c.capture.Enabled()) {
		c.extractSingles(ctx, chunk, st)
		return
	}
	// Grouped extraction renders names only. Do not cache that poorer evidence
	// under a single-item request that also renders files (including subtitles).
	var namesOnly, withFiles []model.Torrent
	for _, t := range chunk {
		if len(extractionModelFiles(t)) > 0 {
			withFiles = append(withFiles, t)
		} else {
			namesOnly = append(namesOnly, t)
		}
	}
	if len(withFiles) > 0 {
		c.extractSingles(ctx, withFiles, st)
		c.extractChunk(ctx, namesOnly, st)
		return
	}
	if len(chunk) < minBatchSplit {
		c.extractSingles(ctx, chunk, st)
		return
	}

	key := c.batchExtractKey(chunk)
	var got map[int]Extraction
	cached := false
	var err error
	if value, ok := c.cache.Get(key); ok {
		got, cached = value.(map[int]Extraction), true
	} else {
		st.Batches++
		got, err = c.callBatchExtract(ctx, chunk)
	}
	if err != nil {
		// Whole call failed (timeout, HTTP error, undecodable JSON): split in
		// half and retry each side — halving isolates a poison name; sides
		// below minBatchSplit go per-name.
		c.logger.Warnw("batch extract failed; splitting", "size", len(chunk), "err", err)
		mid := len(chunk) / 2
		c.extractChunk(ctx, chunk[:mid], st)
		c.extractChunk(ctx, chunk[mid:], st)
		return
	}
	if !cached {
		c.cache.Put(key, got)
	}

	// Count-match guardrail: rows the model dropped (sent 40, got 38 — the
	// classic long-list failure mode) are retried as single calls, which is
	// strictly cheaper than re-paying the whole batch.
	var missing []model.Torrent
	for i, t := range chunk {
		ext, ok := got[i+1] // ids are the 1-based input line numbers
		if !ok {
			missing = append(missing, t)
			continue
		}
		if cached {
			st.Cached++
		} else {
			c.recordBatchExtraction(ext, st)
		}
	}
	if len(missing) > 0 {
		c.logger.Warnw("batch extract dropped rows; retrying as singles",
			"sent", len(chunk), "missing", len(missing))
		c.extractSingles(ctx, missing, st)
	}
}

func (c *Client) extractSingles(ctx context.Context, torrents []model.Torrent, st *BatchStats) {
	for i, t := range torrents {
		if ctx.Err() != nil {
			st.Failed += len(torrents) - i
			return
		}
		st.Singles++
		ext, err := c.Extract(ctx, t)
		switch {
		case err != nil:
			st.Failed++
		case ext.OK:
			st.OK++
		default:
			st.Empty++
		}
	}
}

func (c *Client) recordBatchExtraction(ext Extraction, st *BatchStats) {
	normalizeExtraction(&ext)
	if ext.OK {
		c.metrics.extractTotal.WithLabelValues("ok").Inc()
		st.OK++
	} else {
		c.metrics.extractTotal.WithLabelValues("empty").Inc()
		st.Empty++
	}
}

func batchExtractInput(chunk []model.Torrent) string {
	var sb strings.Builder
	sb.WriteString("release_names:\n")
	for i, t := range chunk {
		fmt.Fprintf(&sb, "%d) %s\n", i+1, t.Name)
	}
	return sb.String()
}

func (c *Client) batchExtractKey(chunk []model.Torrent) string {
	maxTokens := batchBaseMaxTokens + batchPerItemMaxTokens*len(chunk)
	return c.extractionRequestKey("batch_extract", "llmmatch-chat-batch-extract-v2",
		c.newChatRequest(batchExtractSystemPrompt, batchExtractInput(chunk), maxTokens))
}

// callBatchExtract issues one batched stage-1 prompt and returns extractions
// keyed by the echoed input id. Invented ids are dropped. Every item for a
// repeated known id is dropped so an ambiguous answer cannot choose which
// title to attach; the caller retries those missing lines as single requests.
func (c *Client) callBatchExtract(ctx context.Context, chunk []model.Torrent) (map[int]Extraction, error) {
	cctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout+time.Duration(len(chunk))*batchPerItemTimeout)
	defer cancel()
	maxTokens := batchBaseMaxTokens + batchPerItemMaxTokens*len(chunk)
	raw, err := c.callWith(cctx, c.httpLong, "batch_extract", batchExtractSystemPrompt, batchExtractInput(chunk), maxTokens)
	if err != nil {
		return nil, err
	}
	raw, err = requireJSONObject(raw)
	if err != nil {
		c.metrics.callErrors.WithLabelValues("batch_extract", "decode").Inc()
		return nil, fmt.Errorf("decode batch extract: %w", err)
	}

	var env struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		c.metrics.callErrors.WithLabelValues("batch_extract", "decode").Inc()
		return nil, fmt.Errorf("decode batch extract: %w", err)
	}

	out := make(map[int]Extraction, len(env.Items))
	seenIDs := make(map[int]bool, len(env.Items))
	for _, rawItem := range env.Items {
		var header struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal(rawItem, &header); err != nil ||
			header.ID < 1 || header.ID > len(chunk) {
			continue
		}
		if seenIDs[header.ID] {
			delete(out, header.ID)
			continue
		}
		seenIDs[header.ID] = true
		extraction, err := decodeExtraction(rawItem)
		if err != nil {
			// Treat an individual contract violation like a dropped row. The
			// caller retries only that name through the ordinary single-item
			// production boundary rather than caching partial safe defaults.
			continue
		}
		if err := normalizeAndValidateExtraction(&extraction); err != nil {
			continue
		}
		out[header.ID] = extraction
	}
	return out, nil
}

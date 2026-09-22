package processor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/spencercnorton/bitagent/internal/anime"
	"github.com/spencercnorton/bitagent/internal/blocking"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/classifier/llmsignal"
	"github.com/spencercnorton/bitagent/internal/csamblocklist"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/database/query"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/verdicts"
	"go.uber.org/zap"
	"gorm.io/gen/field"
	"gorm.io/gorm/clause"
)

type Processor interface {
	Process(ctx context.Context, params MessageParams) error
}

// contentFilterDeferRetryDelay is how long a content-filter-deferred
// torrent waits before re-processing. The filter defers (rather than
// keeps) a residual torrent only when its LLM endpoint is unreachable
// and LLMDeferOnUnavailable is set; a fixed backoff keeps a long
// outage from hot-looping while still draining promptly once the
// endpoint returns.
const contentFilterDeferRetryDelay = 15 * time.Minute

type processor struct {
	defaultWorkflow string
	search          search.Search
	runner          classifier.Runner
	dao             *dao.Query
	blockingManager blocking.Manager
	// csamExporter records every classifier-driven delete, double-
	// hashes the infohash, and (after re-checking the title against
	// the CSAM banned-keyword regex) appends to the local JSONL log.
	// Always non-nil — wired as an interface with a NoOp impl when
	// CSAM_BLOCKLIST_EXPORT_ENABLED=false. See internal/csamblocklist
	// for the export contract.
	csamExporter csamblocklist.Exporter
	// contentFilter + contentFilterMetrics are the post-classifier
	// hook for the content-filter LLM tier. The dhtcrawler runs a
	// PRE-classifier deterministic pass (DecideDeterministic) to drop
	// obvious junk early; here, after the classifier has populated
	// `cl.ContentType` and `cl.Languages`, we call Filter.Decide
	// which adds the LLM tier on top — the residual cohort
	// (Latin-script titles where TMDB couldn't language-tag) gets a
	// configured hosted-LLM verdict gated by daily budget + cache. Both fields
	// are nil-safe; absence skips the hook entirely.
	contentFilter        *contentfilter.Filter
	contentFilterMetrics *contentfilter.Metrics
	// privacy gates the post-classifier content-filter hook so
	// private-tracker torrents never reach the LLM tier (and therefore
	// its configured provider). Mirrors classifier/llmstage's wiring; both share the
	// same *evidence.Store. nil-safe — when unset (e.g. in narrow
	// tests) the gate is a no-op.
	privacy PrivacyStore
	// verdicts is the T3 phase-C dual-write hook (nil-safe): a
	// classifier ErrDeleteTorrent or a content-filter enforce drop is
	// recorded as a blacklisted verdict AFTER the delete commits. Both
	// cohorts are already added to the blocking bloom by persist -> Block,
	// so this recording is behavior-neutral — it only makes the durable
	// negative enumerable in the ledger.
	verdicts *verdicts.Store
	// deleteAuditBudget gates recording the torrent name in the
	// classifier_delete evidence for a 1-in-128 sample (never for CSAM
	// banned-keyword matches or private torrents). Off by default, and
	// self-limiting once on; see classifier.Config.DeleteAuditSample.
	deleteAuditBudget *deleteAuditBudget
	// deleteMetrics counts every classifier-driven delete by pre-delete
	// content type and rule path (nil-safe). See DeleteMetrics.
	deleteMetrics *DeleteMetrics
	logger        *zap.SugaredLogger
}

// deleteVerdict is a pending blacklisted-verdict write for one deleted
// torrent, accumulated during the classify loop and flushed after persist
// commits (so a rolled-back delete never leaves a false blacklist row).
type deleteVerdict struct {
	infoHash  protocol.ID
	mechanism string
	reason    string
	evidence  []byte
}

type MissingHashesError struct {
	InfoHashes []protocol.ID
}

func (e MissingHashesError) Error() string {
	return fmt.Sprintf("missing %d info hashes", len(e.InfoHashes))
}

func (c processor) Process(ctx context.Context, params MessageParams) error {
	workflowName := params.ClassifierWorkflow
	if workflowName == "" {
		workflowName = c.defaultWorkflow
	}

	searchResult, searchErr := c.search.TorrentsWithMissingInfoHashes(
		ctx,
		params.InfoHashes,
		query.Preload(func(q *dao.Query) []field.RelationField {
			return []field.RelationField{
				q.Torrent.Files.RelationField,
				q.Torrent.Hint.RelationField,
				q.Torrent.Sources.RelationField,
			}
		}),
	)
	if searchErr != nil {
		return searchErr
	}

	tcResult, tcErr := c.search.TorrentContent(
		ctx,
		query.Where(search.TorrentContentInfoHashCriteria(params.InfoHashes...)),
		search.HydrateTorrentContentContent(),
	)
	if tcErr != nil {
		return tcErr
	}

	for _, tc := range tcResult.Items {
		for ti, t := range searchResult.Torrents {
			if t.InfoHash == tc.InfoHash {
				searchResult.Torrents[ti].Contents = append(
					searchResult.Torrents[ti].Contents,
					tc.TorrentContent,
				)

				break
			}
		}
	}

	var (
		mtx                sync.Mutex
		wg                 sync.WaitGroup
		errs               []error
		idsToDelete        []string
		infoHashesToDelete []protocol.ID
		deferredHashes     []protocol.ID
		// deleteVerdicts pairs each queued delete with its blacklist
		// verdict; flushed after persist commits (recording-only).
		deleteVerdicts []deleteVerdict
	)

	tcs := make([]model.TorrentContent, 0, len(searchResult.Torrents))

	tagsToAdd := make(map[protocol.ID]map[string]struct{})

	failedHashes := make([]protocol.ID, 0, len(searchResult.MissingInfoHashes))
	failedHashes = append(failedHashes, searchResult.MissingInfoHashes...)

	if len(failedHashes) > 0 {
		errs = append(errs, MissingHashesError{InfoHashes: failedHashes})
	}

	for _, torrent := range searchResult.Torrents {
		wg.Add(1)

		go func(torrent model.Torrent) {
			defer wg.Done()

			thisDeleteIDs := make(map[string]struct{}, len(torrent.Contents))
			foundMatch := false

			for _, tc := range torrent.Contents {
				thisDeleteIDs[tc.ID] = struct{}{}

				if !foundMatch &&
					!torrent.Hint.ContentSource.Valid &&
					params.ClassifyMode != ClassifyModeRematch &&
					tc.ContentType.Valid &&
					tc.ContentSource.Valid &&
					(torrent.Hint.IsNil() || torrent.Hint.ContentType == tc.ContentType.ContentType) {
					torrent.Hint.ContentType = tc.ContentType.ContentType
					torrent.Hint.ContentSource = tc.ContentSource
					torrent.Hint.ContentID = tc.ContentID
					foundMatch = true
				}
			}

			// Per-run sideband for the LLM matcher's English-track read —
			// survives the find_match result discard on unmatched outcomes.
			runCtx := llmsignal.WithHolder(ctx)

			cl, classifyErr := c.runner.Run(runCtx, workflowName, params.ClassifierFlags, torrent)

			mtx.Lock()
			defer mtx.Unlock()

			if classifyErr != nil {
				if errors.Is(classifyErr, classification.ErrDeleteTorrent) {
					infoHashesToDelete = append(infoHashesToDelete, torrent.InfoHash)
					c.deleteMetrics.Observe(cl.ContentType, classifyErr)
					// Hoisted above both hooks: the ledger's sampled name
					// capture and the CSAM exporter run the same
					// banned-keyword test over these paths, and must never
					// see different input.
					filePaths := make([]string, 0, len(torrent.Files))
					for _, f := range torrent.Files {
						filePaths = append(filePaths, f.Path)
					}
					if c.verdicts != nil {
						// CSAM keyword deletes flow through this same
						// ErrDeleteTorrent path (design §1 row 1) and are
						// tagged classifier_delete — the dedicated `csam`
						// mechanism lands in a later phase-C MR. The
						// evidence rule_path preserves the CSAM-vs-flag
						// distinction, so when an operator-restore path
						// over the ledger is added it MUST refuse to
						// restore a classifier_delete whose rule_path is a
						// banned-keyword rule (design §3/§6-Q3: CSAM
						// blacklists are not operator-restorable).
						deleteVerdicts = append(deleteVerdicts, deleteVerdict{
							infoHash:  torrent.InfoHash,
							mechanism: verdicts.MechanismClassifierDelete,
							reason:    "classifier delete_torrent; content deleted + blocking-bloomed",
							evidence: classifierDeleteEvidence(
								workflowName, classifyErr, torrent,
								filePaths, c.deleteAuditBudget,
							),
						})
					}

					// CSAM self-export hook — exporter independently
					// re-checks the title + file paths against the
					// banned-keyword regex (so other classifier-driven
					// deletes like flags.delete_xxx don't get mistaken
					// for CSAM observations). When matched, the
					// infohash is double-hashed (SHA-256 of the SHA-1
					// infohash) and appended to the local JSONL log;
					// the raw infohash never leaves this hook.
					c.csamExporter.Record(ctx, torrent.InfoHash, torrent.Name, filePaths)
				} else {
					failedHashes = append(failedHashes, torrent.InfoHash)
					errs = append(errs, classifyErr)
				}
			} else {
				// Post-classifier content-filter hook. The classifier has
				// emitted ContentType + Languages, so Filter.Decide can
				// run the LLM tier on residual cases (Latin-script titles
				// with no TMDB-derived language tag). Pre-classifier
				// dhtcrawler hook used DecideDeterministic — that's the
				// cheap ladder; this is the language-aware second pass.
				//
				// Gating mirrors the pre-classifier hook: skip when the
				// filter is disabled (no metrics emitted, zero cost) or
				// not wired (nil — older deploys / partial test wiring).
				// On a "drop" decision in enforce mode we route the
				// torrent to infoHashesToDelete, treating it the same as
				// the classifier's ErrDeleteTorrent path: the persist
				// pass will purge the torrent + its TorrentContent rows
				// in one transaction. In shadow mode (Enforce=false) the
				// metric records the would-drop and we fall through to
				// the normal persist.
				if !params.SkipContentFilter &&
					c.contentFilter != nil &&
					c.contentFilter.Enabled() &&
					!c.shouldSkipContentFilter(ctx, torrent, cl) {
					fi := torrentToFilterInput(torrent, cl)
					groupKey := []byte(contentfilter.EvaluationGroupKey(fi.Title))
					if cl.Content != nil {
						groupKey = []byte(fmt.Sprintf(
							"%s\x00%s\x00%s", cl.Content.Type, cl.Content.Source, cl.Content.ID,
						))
					}
					d, filterErr := c.contentFilter.DecideAudited(ctx, fi, contentfilter.AuditSource{
						InfoHash: torrent.InfoHash.Bytes(), GroupKey: groupKey,
					})
					if filterErr != nil {
						failedHashes = append(failedHashes, torrent.InfoHash)
						errs = append(errs, fmt.Errorf("contentfilter audited decision: %w", filterErr))
						return
					}
					if c.contentFilterMetrics != nil {
						c.contentFilterMetrics.Observe(d)
					}
					if d.Defer {
						// LLM endpoint unreachable + defer-on-unavailable:
						// re-queue (with backoff) instead of keeping
						// (foreign leak) or dropping (false positive).
						deferredHashes = append(deferredHashes, torrent.InfoHash)
						return
					}
					if !d.Allow {
						infoHashesToDelete = append(infoHashesToDelete, torrent.InfoHash)
						if c.verdicts != nil {
							deleteVerdicts = append(deleteVerdicts, deleteVerdict{
								infoHash:  torrent.InfoHash,
								mechanism: verdicts.MechanismContentFilter,
								reason:    "content-filter enforce drop; content deleted + blocking-bloomed",
								evidence:  contentFilterEvidence(d),
							})
						}
						return
					}
				}

				torrentContent := newTorrentContent(torrent, cl, llmsignal.English(runCtx))

				tcID := torrentContent.InferID()
				for id := range thisDeleteIDs {
					if id != tcID {
						idsToDelete = append(idsToDelete, id)
					}
				}

				tcs = append(tcs, torrentContent)

				if len(cl.Tags) > 0 {
					tagsToAdd[torrent.InfoHash] = cl.Tags
				}
			}
		}(torrent)
	}

	wg.Wait()

	// Deletes are persistence work too: a batch can contain only
	// classifier/content-filter drops and no replacement TorrentContent rows.
	payload := persistPayload{
		torrentContents:  tcs,
		deleteIDs:        idsToDelete,
		deleteInfoHashes: infoHashesToDelete,
		addTags:          tagsToAdd,
	}

	if len(failedHashes) > 0 {
		if payload.isEmpty() {
			return errors.Join(errs...)
		}

		republishJob, republishJobErr := NewQueueJob(MessageParams{
			InfoHashes:         failedHashes,
			ClassifyMode:       params.ClassifyMode,
			ClassifierWorkflow: workflowName,
			ClassifierFlags:    params.ClassifierFlags,
			SkipContentFilter:  params.SkipContentFilter,
		})
		if republishJobErr != nil {
			return errors.Join(append(errs, republishJobErr)...)
		}

		if err := c.dao.QueueJob.WithContext(ctx).Clauses(clause.OnConflict{
			DoNothing: true,
		}).Create(&republishJob); err != nil {
			return errors.Join(append(errs, err)...)
		}
	}

	// Content-filter deferrals: torrents whose LLM classification
	// couldn't complete (endpoint unreachable, LLMDeferOnUnavailable
	// set). Re-publish with a backoff so they're retried once the LLM
	// is back — never kept (leak) or dropped. OnConflict DoNothing
	// dedups an already-pending defer for the same set.
	if len(deferredHashes) > 0 {
		deferJob, deferJobErr := NewQueueJob(MessageParams{
			InfoHashes:         deferredHashes,
			ClassifyMode:       params.ClassifyMode,
			ClassifierWorkflow: workflowName,
			ClassifierFlags:    params.ClassifierFlags,
			SkipContentFilter:  params.SkipContentFilter,
		}, model.QueueJobDelayBy(contentFilterDeferRetryDelay))
		if deferJobErr != nil {
			return errors.Join(append(errs, deferJobErr)...)
		}
		if err := c.dao.QueueJob.WithContext(ctx).Clauses(clause.OnConflict{
			DoNothing: true,
		}).Create(&deferJob); err != nil {
			return errors.Join(append(errs, err)...)
		}
	}

	if payload.isEmpty() {
		return nil
	}

	if err := c.persist(ctx, payload); err != nil {
		return err
	}

	// Deletes committed — dual-write the blacklist verdicts (phase C).
	// AFTER persist so a rolled-back delete never leaves a false negative;
	// recording-only + log-and-continue, so a ledger error never fails the
	// batch. deleteVerdicts is empty unless c.verdicts is wired.
	c.recordDeleteVerdicts(ctx, deleteVerdicts)
	return nil
}

// recordDeleteVerdicts writes one blacklisted verdict per committed delete.
// Best-effort: mirrors the phase-A junkpurge dual-writer — a Record error is
// logged, never propagated.
func (c processor) recordDeleteVerdicts(ctx context.Context, dv []deleteVerdict) {
	if c.verdicts == nil {
		return
	}
	for _, v := range dv {
		if err := c.verdicts.Record(ctx, verdicts.Event{
			InfoHash:  v.infoHash.Bytes(),
			Verdict:   verdicts.VerdictBlacklisted,
			Mechanism: v.mechanism,
			Reason:    v.reason,
			Evidence:  v.evidence,
		}); err != nil {
			if c.logger != nil {
				c.logger.Warnw("processor verdict record", "mechanism", v.mechanism, "err", err)
			}
		}
	}
}

// classifierDeleteEvidence records why the classifier condemned a torrent —
// the workflow name plus, when the error is a RuntimeError, the CEL rule
// path (e.g. ["keywords","banned"] vs ["flags","delete_xxx"]). The path
// distinguishes CSAM-keyword deletes from ordinary flag deletes WITHOUT
// storing any title or file path. Returns nil on marshal error (evidence is
// optional; a verdict with no evidence is still valid).
// deleteNameSampleShare is the reciprocal of the fraction of classifier
// deletes whose name is recorded. Info-hashes are SHA-1, so the leading byte
// is uniform and `< 2` is a clean 1-in-128 Bernoulli sample that is
// independent of workflow, rule, and content — which is what makes the
// resulting rows a valid basis for a proportion estimate.
//
// ponytail: sampled, not exhaustive. classifier_delete runs at ~160k/day, so
// capturing every name costs ~15 GB/year to answer a question that needs a few
// hundred adjudicated rows. At 1/128 this yields ~1,250 names/day — already
// ~4x the n=300 a Wilson bound wants. Raise the constant only if a per-rule
// breakdown needs its own n (norvi[4] is only ~26/day, so a rule-stratified
// estimate would).
const deleteNameSampleShare = 2

// classifierDeleteEvidence records which rule destroyed a torrent, and — for a
// small unbiased sample — what it destroyed.
//
// The name is the whole point: classifier_delete is the largest destructive
// surface in the estate (2.23M lifetime, ~160k/day, every one blacklisted so it
// cannot be re-acquired), and until now the ledger recorded only that a
// decision happened, never enough to review it. No false-delete rate for
// norvi[1] or the non_english rule can be estimated without this.
//
// Two exclusions, both mandatory:
//
//   - CSAM. A banned-keyword delete never records a name. The check is
//     csamblocklist.MatchesBannedKeyword — the same parity-pinned predicate the
//     exporter uses to keep other delete reasons out of the CSAM log, applied
//     here in the opposite direction. Not a rule_path test: rule indices move
//     when the operator edits the workflow, and a stale index would silently
//     start recording banned-keyword names.
//   - Private torrents. Mirrors llmmatch.nativePrivateBlocked and junkpurge's
//     `WHERE t.private = false`: a native private flag suppresses recording
//     regardless of anything else.
func classifierDeleteEvidence(
	workflow string,
	classifyErr error,
	torrent model.Torrent,
	filePaths []string,
	budget *deleteAuditBudget,
) []byte {
	ev := map[string]any{"workflow": workflow}
	var re classification.RuntimeError
	if errors.As(classifyErr, &re) && len(re.Path) > 0 {
		ev["rule_path"] = re.Path
	}
	if name, ok := sampledDeleteName(torrent, filePaths, budget); ok {
		ev["name"] = name
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return nil
	}
	return b
}

// sampledDeleteName reports the torrent name when the operator has enabled
// the audit sample, the collection budget is not spent, this delete is inside
// the sample, and neither exclusion applies.
//
// Order matters: the budget is consumed LAST, after every exclusion and the
// sample test have passed, so an excluded or out-of-sample delete never burns
// quota. That keeps the cap a bound on names actually recorded rather than on
// deletes seen.
func sampledDeleteName(
	t model.Torrent,
	filePaths []string,
	budget *deleteAuditBudget,
) (string, bool) {
	if !budget.enabled() {
		return "", false
	}
	if t.Private {
		return "", false
	}
	hash := t.InfoHash.Bytes()
	if len(hash) == 0 || hash[0] >= deleteNameSampleShare {
		return "", false
	}
	if csamblocklist.MatchesBannedKeyword(t.Name, filePaths) {
		return "", false
	}
	if !budget.consume() {
		return "", false
	}
	return t.Name, true
}

// contentFilterEvidence records the stable non-PII drop reason (the same
// label the content-filter metric uses) plus the blocked extension when the
// reason is a blocked-extension drop. No title or file path.
func contentFilterEvidence(d contentfilter.Decision) []byte {
	ev := map[string]any{"reason": d.Reason.String()}
	if d.BlockedExt != "" {
		ev["ext"] = d.BlockedExt
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return nil
	}
	return b
}

func newTorrentContent(t model.Torrent, c classification.Result, llmEnglish model.NullEnglishAudio) model.TorrentContent {
	var filesCount model.NullUint
	if t.FilesCount.Valid {
		filesCount = t.FilesCount
	} else if t.FilesStatus == model.FilesStatusSingle {
		filesCount = model.NewNullUint(1)
	}

	animeSignals := anime.Detect(t.Name)

	tc := model.TorrentContent{
		Torrent:         t,
		InfoHash:        t.InfoHash,
		ContentType:     c.ContentType,
		Languages:       c.Languages,
		Episodes:        c.Episodes,
		VideoResolution: c.VideoResolution,
		VideoSource:     c.VideoSource,
		VideoCodec:      c.VideoCodec,
		Video3D:         c.Video3D,
		VideoModifier:   c.VideoModifier,
		ReleaseGroup:    c.ReleaseGroup,
		Size:            t.Size,
		FilesCount:      filesCount,
		Seeders:         t.Seeders(),
		Leechers:        t.Leechers(),
		PublishedAt:     t.PublishedAt(),
		// Persist the full deterministic anime signal (fansub group anywhere,
		// absolute "Title - NNN" numbering, romaji season/English-track
		// markers) from the release name, so the server-side cat=5070 filter
		// and the Torznab 5070 emission read a stored flag instead of
		// re-detecting from torrents.name on every query.
		IsAnime: animeSignals.IsAnime(),
	}

	// Persist the absolute episode number alongside is_anime — previously
	// parsed on every classification and discarded into the bool. Gated on
	// anime-ness established INDEPENDENTLY of the absolute number (a plain
	// IsAnime gate is circular for unlisted groups and would stamp any
	// "[LatinTag] Anything - NN"), and skipped for batch-range starts
	// ("- 01 ~ 24"), which are not THE episode of the release.
	if animeSignals.IsAnimeIndependentOfAbsolute() &&
		animeSignals.AbsoluteEpisode > 0 && !animeSignals.AbsoluteRange {
		tc.AnimeAbsoluteEpisode = model.NewNullUint(uint(animeSignals.AbsoluteEpisode))
	}

	// English availability (anime only), merged with explicit precedence:
	// a deterministic name signal wins ('name'); otherwise a definite LLM
	// read from THIS run fills in ('llm' — the only path that can assign
	// 'none'/raw). The pair is always written together; persist() protects
	// existing 'llm' rows from cycles that carry no signal at all.
	tc.EnglishAudio = model.DeriveEnglishAudio(
		animeSignals.IsAnime(), animeSignals.EnglishAudioSignal, animeSignals.EnglishSubSignal)
	if tc.EnglishAudio.Valid {
		tc.EnglishAudioSource = model.NewNullString(model.EnglishAudioSourceName)
	} else if llmEnglish.Valid {
		tc.EnglishAudio = llmEnglish
		tc.EnglishAudioSource = model.NewNullString(model.EnglishAudioSourceLLM)
	}

	if c.Content != nil {
		content := *c.Content
		content.UpdateTsv()
		tc.ContentType = model.NewNullContentType(content.Type)
		tc.ContentSource = model.NewNullString(content.Source)
		tc.ContentID = model.NewNullString(content.ID)
		tc.Content = content
	}

	// Derived AFTER the content attach above so a hint/TMDB-corrected content
	// type (not the raw classifier guess) decides whether granularity applies.
	tc.ReleaseGranularity = model.DeriveReleaseGranularity(tc.ContentType, tc.Episodes, t.Name)

	// Persist the parsed air date (classifier parse_date action) — previously
	// computed and dropped, breaking daily-show date queries end to end. Only
	// a FULL date is stored: movie parses carry year-only Dates whose Value()
	// would fabricate a Jan 1 timestamp.
	if c.Date.IsValid() {
		tc.ReleaseDate = c.Date
	}

	tc.UpdateTsv()

	return tc
}

package processor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/spencercnorton/bitagent/internal/anime"
	"github.com/spencercnorton/bitagent/internal/blocking"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/csamblocklist"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/database/query"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"github.com/spencercnorton/bitagent/internal/processingstate"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/verdicts"
	"go.uber.org/zap"
	"gorm.io/gen/field"
	"gorm.io/gorm/clause"
)

type Processor interface {
	Process(ctx context.Context, params MessageParams) error
}

type processor struct {
	coreRecorder    processingstate.Recorder
	namePolicy      *namepolicy.Policy
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
	// Deterministic policy runs again after classification has supplied types
	// and languages. Missing optional processors do not affect this decision.
	contentFilter        *contentfilter.Filter
	contentFilterMetrics *contentfilter.Metrics
	// Privacy independently protects catalogue handling and sampled evidence.
	privacy PrivacyStore
	// Legacy audit helpers remain available for historical evidence. Removal
	// authority is now written atomically by the complete recovery transition;
	// an independent blacklist here would invalidate its restore receipt.
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

type MissingHashesError struct {
	InfoHashes []protocol.ID
}

func (e MissingHashesError) Error() string {
	return fmt.Sprintf("missing %d info hashes", len(e.InfoHashes))
}

func (c processor) Process(ctx context.Context, params MessageParams) error {
	if c.namePolicy.Enabled() {
		hashes, err := c.admitNames(ctx, params.InfoHashes)
		if err != nil {
			return err
		}
		params.InfoHashes = hashes
		if len(hashes) == 0 {
			return nil
		}
	}
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
		deleteObservations []deleteObservation
		deleteReasons      []string
	)

	tcs := make([]model.TorrentContent, 0, len(searchResult.Torrents))
	coreReceipts := []processingstate.Receipt{}

	preserved := map[protocol.ID]*classification.ApplicationPreservation{}
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

			d := c.namePolicy.Evaluate(torrent.InfoHash, torrent.Name)
			if !d.Eligible {
				c.namePolicy.Observe("processor", d)
				return
			}
			sourceTorrent := torrent
			thisDeleteIDs := make(map[string]struct{}, len(torrent.Contents))
			foundMatch := false

			for _, tc := range torrent.Contents {
				thisDeleteIDs[tc.ID] = struct{}{}

				if !foundMatch &&
					!torrent.Hint.ContentSource.Valid &&
					params.ClassifyMode != ClassifyModeRematch &&
					tc.ContentType.Valid &&
					(torrent.Hint.IsNil() || torrent.Hint.ContentType == tc.ContentType.ContentType) {
					torrent.Hint.ContentType = tc.ContentType.ContentType
					torrent.Hint.ContentSource = tc.ContentSource
					torrent.Hint.ContentID = tc.ContentID
					foundMatch = true
				}
			}

			// Keep source admission independent of optional processors.
			runCtx, matchTrace := processingstate.WithTrace(ctx)
			if c.namePolicy.Enabled() {
				runCtx = namepolicy.WithAdmission(runCtx, c.providerNameAdmission)
			}

			cl, classifyErr := c.runner.Run(runCtx, workflowName, params.ClassifierFlags, torrent)

			mtx.Lock()
			defer mtx.Unlock()

			if classifyErr != nil {
				for _, old := range sourceTorrent.Contents {
					coreReceipts = append(coreReceipts, processingstate.Receipt{Torrent: sourceTorrent, ContentID: old.ID, Outcome: "deferred", Processing: "deferred"})
				}
				if errors.Is(classifyErr, namepolicy.ErrExcluded) {
					return
				}
				if errors.Is(classifyErr, classification.ErrDeleteTorrent) {
					infoHashesToDelete = append(infoHashesToDelete, torrent.InfoHash)
					deleteObservations = append(deleteObservations, deleteObservation{cl.ContentType, classifyErr})
					filePathsForEvidence := make([]string, 0, len(torrent.Files))
					for _, f := range torrent.Files {
						filePathsForEvidence = append(filePathsForEvidence, f.Path)
					}
					evidenceBytes := classifierDeleteEvidence(workflowName, classifyErr, torrent, filePathsForEvidence, c.deleteAuditBudget)
					reason := string(evidenceBytes)
					if reason == "" {
						reason = "classifier_delete"
					}
					deleteReasons = append(deleteReasons, reason)
					// The independent CSAM observation exporter verifies the
					// name and paths even when destructive admission is held.
					filePaths := make([]string, 0, len(torrent.Files))
					for _, f := range torrent.Files {
						filePaths = append(filePaths, f.Path)
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
				kind := ""
				if cl.ContentType.Valid {
					kind = cl.ContentType.ContentType.String()
				}
				d := c.namePolicy.EvaluateClassified(torrent.InfoHash, torrent.Name, kind)
				if !d.Eligible {
					c.namePolicy.Observe("processor", d)
					return
				}
				// Apply the same deterministic filter after type/language enrichment.
				if !params.SkipContentFilter &&
					c.contentFilter != nil &&
					c.contentFilter.Enabled() &&
					!c.shouldSkipContentFilter(ctx, torrent, cl) {
					fi := torrentToFilterInput(torrent, cl)
					d := c.contentFilter.DecideDeterministic(fi)
					if c.contentFilterMetrics != nil {
						c.contentFilterMetrics.Observe(d)
					}
					if !d.Allow {
						infoHashesToDelete = append(infoHashesToDelete, torrent.InfoHash)
						deleteReasons = append(deleteReasons, "content_filter")
						return
					}

				}

				torrentContent := newTorrentContent(torrent, cl)

				tcID := torrentContent.InferID()
				for id := range thisDeleteIDs {
					if id != tcID {
						idsToDelete = append(idsToDelete, id)
					}
				}

				tcs = append(tcs, torrentContent)
				coreReceipts = append(coreReceipts, processingstate.Receipt{Torrent: sourceTorrent, ContentID: torrentContent.InferID(), Outcome: matchTrace.Outcome(cl.Content != nil), Processing: "completed"})
				if cl.Preserved != nil {
					preserved[torrent.InfoHash] = cl.Preserved
				}

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
		applications:     preserved,
		deleteIDs:        idsToDelete,
		deleteInfoHashes: infoHashesToDelete,
		deleteReasons:    deleteReasons,
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

	if payload.isEmpty() {
		if c.coreRecorder != nil {
			c.coreRecorder.RecordCore(ctx, coreReceipts)
		}
		return nil
	}

	if err := c.persist(ctx, payload); err != nil {
		return err
	}

	if c.coreRecorder != nil {
		c.coreRecorder.RecordCore(ctx, coreReceipts)
	}

	// Count fully successful removal calls, never held decisions. The
	// shared writer owns the bound verdict; do not overwrite it after commit.
	for _, observation := range deleteObservations {
		c.deleteMetrics.Observe(observation.contentType, observation.err)
	}
	return nil
}

type deleteObservation struct {
	contentType model.NullContentType
	err         error
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
// Sampled, not exhaustive. classifier_delete runs at ~160k/day, so
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

func newTorrentContent(t model.Torrent, c classification.Result) model.TorrentContent {
	var filesCount model.NullUint
	if t.FilesCount.Valid {
		filesCount = t.FilesCount
	} else if t.FilesStatus == model.FilesStatusSingle {
		filesCount = model.NewNullUint(1)
	}

	animeSignals := anime.Detect(t.Name)

	tc := model.TorrentContent{
		Torrent:           t,
		InfoHash:          t.InfoHash,
		ContentType:       c.ContentType,
		Languages:         c.Languages,
		Episodes:          c.Episodes,
		VideoResolution:   c.VideoResolution,
		VideoSource:       c.VideoSource,
		VideoCodec:        c.VideoCodec,
		Video3D:           c.Video3D,
		VideoModifier:     c.VideoModifier,
		ReleaseGroup:      c.ReleaseGroup,
		ReleaseAttributes: c.ReleaseAttributes,
		Size:              t.Size,
		FilesCount:        filesCount,
		Seeders:           t.Seeders(),
		Leechers:          t.Leechers(),
		PublishedAt:       t.PublishedAt(),
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

	// New audio claims come only from deterministic release-name evidence.
	// Persistence preserves attributed historical results when no new fact exists.
	tc.EnglishAudio = model.DeriveEnglishAudio(
		animeSignals.IsAnime(), animeSignals.EnglishAudioSignal, animeSignals.EnglishSubSignal)
	if tc.EnglishAudio.Valid {
		tc.EnglishAudioSource = model.NewNullString(model.EnglishAudioSourceName)
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

package processor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/classifier/llmstage"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/llmwork"
	"github.com/spencercnorton/bitagent/internal/model"
)

// DeferredApplyHandler runs only the specific source-bound stage. It never
// enters Processor.Process, a general workflow, deletion or redispatch.
type DeferredApplyHandler struct {
	Pool       lazy.Lazy[*pgxpool.Pool]
	Search     lazy.Lazy[search.Search]
	Runner     lazy.Lazy[classifier.Runner]
	Filter     *contentfilter.Filter
	Matcher    *llmmatch.Client
	Classifier classifier.Config
	Observer   classifier.MatchDecisionObserver
}

func decodeDeferredPayload(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return llmwork.ErrObsolete
	}
	return nil
}

func (h *DeferredApplyHandler) Handle(ctx context.Context, task llmwork.Task) error {
	e := llmwork.ExecutionFrom(ctx)
	if e == nil || e.Store == nil || !bytes.Equal(e.Lease.Task.Key, task.Key) {
		return llmwork.ErrLease
	}
	pool, err := h.Pool.Get()
	if err != nil {
		return err
	}
	var source model.Torrent
	var target model.TorrentContent
	load := func(c context.Context) error {
		tx, x := pool.Begin(c)
		if x != nil {
			return x
		}
		defer tx.Rollback(context.WithoutCancel(c))
		source, x = lockedDeferredSource(c, tx, task)
		if x != nil {
			return x
		}
		target, x = lockedDeferredTarget(c, tx, source, task.Kind == llmwork.Type, task.Kind == llmwork.Language)
		if x != nil {
			return x
		}
		return tx.Commit(c)
	}
	if err = load(ctx); err != nil {
		return err
	}
	recheckTx := func(c context.Context, tx pgx.Tx) error {
		current, x := lockedDeferredSource(c, tx, task)
		if x != nil {
			return x
		}
		nowTarget, x := lockedDeferredTarget(c, tx, current, task.Kind == llmwork.Type, task.Kind == llmwork.Language)
		if x != nil {
			return x
		}
		if !sameDeferredTarget(target, nowTarget) {
			return llmwork.ErrObsolete
		}
		return nil
	}
	if err = llmwork.SetSourceRecheck(ctx, func(c context.Context) error {
		tx, x := pool.Begin(c)
		if x != nil {
			return x
		}
		defer tx.Rollback(context.WithoutCancel(c))
		if x = recheckTx(c, tx); x != nil {
			return x
		}
		return tx.Commit(c)
	}); err != nil {
		return err
	}
	if err = llmwork.SetTransactionalSourceRecheck(ctx, recheckTx); err != nil {
		return err
	}
	ctx = llmwork.WithSourceTorrent(ctx, source)
	var policy any
	var receipt llmcapture.ResultReceipt
	var captureTask llmcapture.Task
	var result classification.Result
	var tags []string
	var validate func(json.RawMessage) bool
	switch task.Kind {
	case llmwork.Type:
		var p llmstage.WorkPayload
		if err = decodeDeferredPayload(task.Payload, &p); err != nil {
			return err
		}
		runner, x := h.Runner.Get()
		if x != nil {
			return x
		}
		stage, ok := runner.(*llmstage.Stage)
		if !ok {
			return llmwork.ErrHeld
		}
		if err = stage.ValidateWork(p); err != nil {
			return err
		}
		policy = stage.WorkPolicy(p)
		if !bytes.Equal(task.PolicyDigest, llmwork.Digest(policy)) || !bytes.Equal(task.InputDigest, stage.WorkInputDigest(source)) {
			return llmwork.ErrObsolete
		}
		prediction, x := stage.EvaluateDeferred(ctx, source, p)
		if x != nil {
			return x
		}
		if !prediction.Qualified {
			return llmwork.DeclinedError{Reason: "type_not_qualified"}
		}
		if !deferredContentTypeAllowed(prediction.Type, h.Classifier, p.Flags) {
			return llmwork.DeclinedError{Reason: "content_policy"}
		}
		result = llmwork.NewApplicationSnapshot(llmwork.Type, target, nil).Result()
		result.ContentType = prediction.Type
		backend, x := h.Search.Get()
		if x != nil {
			return x
		}
		local := classifier.NewLocalSearch(backend, h.Classifier)
		localEnabled := true
		if v, ok := h.Classifier.Flags["local_search_enabled"].(bool); ok {
			localEnabled = v
		}
		if v, ok := p.Flags["local_search_enabled"].(bool); ok {
			localEnabled = v
		}
		result, x = classifier.EnrichTypeLocally(ctx, source, result, local, classifier.LocalTypeEnrichmentOptions{ParseNoiseV2: h.Classifier.ParseNoiseV2, SingleEpisodeMaxBytes: h.Classifier.SingleEpisodeMaxBytes, LocalSearchEnabled: localEnabled, AltTitleMatch: h.Classifier.AltTitleMatch})
		if x != nil {
			return x
		}
		if !deferredContentTypeAllowed(result.ContentType, h.Classifier, p.Flags) {
			return llmwork.ErrObsolete
		}
		receipt, captureTask = prediction.Receipt, llmcapture.TaskClassifierType
		category := "movie"
		if prediction.Type.ContentType == model.ContentTypeTvShow {
			category = "tv"
		}
		validate = func(raw json.RawMessage) bool {
			return deferredTypeDecisionMatches(raw, category, prediction.Confidence)
		}
		for tag := range result.Tags {
			tags = append(tags, tag)
		}
	case llmwork.Language:
		if h.Filter == nil {
			return llmwork.ErrHeld
		}
		var p contentfilter.WorkPayload
		if err = decodeDeferredPayload(task.Payload, &p); err != nil {
			return err
		}
		result = llmwork.NewApplicationSnapshot(llmwork.Language, target, nil).Result()
		group := []byte(contentfilter.EvaluationGroupKey(source.Name))
		if result.Content != nil {
			group = []byte(fmt.Sprintf("%s\x00%s\x00%s", result.Content.Type, result.Content.Source, result.Content.ID))
		}
		if !bytes.Equal(llmwork.Digest(p.Input), llmwork.Digest(torrentToFilterInput(source, result))) || !bytes.Equal(p.Source.InfoHash, task.InfoHash) || !bytes.Equal(p.Source.GroupKey, group) {
			return llmwork.ErrObsolete
		}
		policy = h.Filter.WorkPolicy()
		if !bytes.Equal(task.PolicyDigest, llmwork.Digest(policy)) {
			return llmwork.ErrObsolete
		}
		contract, x := h.Filter.EvaluationCapture(p.Input)
		if x != nil {
			return x
		}
		if !bytes.Equal(task.InputDigest, llmwork.Digest(contract.ModelInputJSON)) {
			return llmwork.ErrObsolete
		}
		traced, trace := llmcapture.WithResultTrace(ctx)
		decision, x := h.Filter.DecideAudited(traced, p.Input, p.Source)
		if x != nil {
			return x
		}
		if !decision.Allow || !decision.Review || !decision.WouldReview || decision.WouldDrop || decision.Defer {
			return llmwork.DeclinedError{Reason: "language_review_not_qualified"}
		}
		var ok bool
		receipt, ok = trace.Result(llmcapture.TaskContentFilter, llmcapture.CandidateSourceNone)
		if !ok {
			return llmwork.ErrHeld
		}
		captureTask = llmcapture.TaskContentFilter
		tags = []string{"llm-language-review"}
		validate = func(raw json.RawMessage) bool {
			var d llmcapture.ContentFilterDecision
			return json.Unmarshal(raw, &d) == nil && d.Outcome == "non_english" && d.Live && d.WouldReview && !d.WouldDrop && !d.IsEnglish && d.EffectiveLLMAction() == "review" && d.Confidence >= d.MinConfidence
		}
	case llmwork.Matcher:
		if h.Matcher == nil || h.Observer == nil {
			return llmwork.ErrHeld
		}
		var p llmmatch.WorkPayload
		if err = decodeDeferredPayload(task.Payload, &p); err != nil {
			return err
		}
		if !p.Type.Valid || !deferredContentTypeAllowed(p.Type, h.Classifier, nil) || (target.ContentType.Valid != p.Type.Valid || target.ContentType.ContentType != p.Type.ContentType) {
			return llmwork.ErrObsolete
		}
		policy = h.Matcher.WorkPolicy(p)
		if !bytes.Equal(task.PolicyDigest, llmwork.Digest(policy)) || !bytes.Equal(task.InputDigest, h.Matcher.WorkInputDigest(source)) {
			return llmwork.ErrObsolete
		}
		result = llmwork.NewApplicationSnapshot(llmwork.Matcher, target, nil).Result()
		traced, trace := llmcapture.WithResultTrace(llmmatch.WithWorkType(ctx, p.Type))
		runner, x := h.Runner.Get()
		if x != nil {
			return x
		}
		dec, x := runner.EvalMatch(traced, source, p.Type)
		if deferred := llmwork.LastDeferral(traced); deferred != nil {
			return deferred
		}
		if x != nil {
			return x
		}
		result, x = classifier.FinalizeDeferredMatch(traced, source, result, dec, h.Matcher, h.Observer)
		if x != nil {
			if errors.Is(x, classification.ErrUnmatched) {
				return llmwork.DeclinedError{Reason: "match_not_qualified"}
			}
			return x
		}
		if result.Content == nil || result.Content.Source != "tmdb" || result.Content.Type != p.Type.ContentType {
			return llmwork.ErrObsolete
		}
		var ok bool
		receipt, ok = trace.Result(llmcapture.TaskMatcherRerank, llmcapture.CandidateSource(dec.CandidateSource))
		if !ok {
			return llmwork.ErrHeld
		}
		captureTask = llmcapture.TaskMatcherRerank
		tags = []string{"llm-matched"}
		validate = func(raw json.RawMessage) bool {
			var d llmcapture.MatchDecision
			return json.Unmarshal(raw, &d) == nil && d.Outcome == "matched" && d.Live && d.WouldAttach && d.GateReason == "" && d.ChosenID == dec.MatchedID && strconv.FormatInt(d.ChosenID, 10) == result.Content.ID && d.Confidence == dec.Confidence && d.Confidence >= d.MinConfidence && d.IsTV == dec.IsTV && d.ResolvedYear == int(result.Content.ReleaseYear)
		}
	default:
		return llmwork.ErrObsolete
	}
	return e.Store.Apply(ctx, e.Lease, "source_bound_application", func(tx pgx.Tx) error {
		current, x := lockedDeferredSource(ctx, tx, task)
		if x != nil {
			return x
		}
		before, x := lockedDeferredTarget(ctx, tx, current, task.Kind == llmwork.Type, task.Kind == llmwork.Language)
		if x != nil {
			return x
		}
		// Detect any catalogue metadata or type edits since stage evaluation too.
		if !sameDeferredTarget(target, before) {
			return llmwork.ErrObsolete
		}
		if !bytes.Equal(task.PolicyDigest, llmwork.Digest(policy)) {
			return llmwork.ErrObsolete
		}
		raw, x := deferredReceiptDecision(ctx, tx, task, receipt, captureTask)
		if x != nil {
			return x
		}
		if !validate(raw) {
			return llmwork.ErrObsolete
		}
		after := before
		if task.Kind != llmwork.Language {
			after = newTorrentContent(current, result, model.NullEnglishAudio{})
			if !after.EnglishAudioSource.Valid {
				after.EnglishAudio = before.EnglishAudio
				after.EnglishAudioSource = before.EnglishAudioSource
			}
			if x = ensureDeferredContent(ctx, tx, &after); x != nil {
				return x
			}
			var claims any
			if after.ReleaseAttributes != nil {
				raw, _ := json.Marshal(after.ReleaseAttributes)
				claims = string(raw)
			}
			languages, _ := json.Marshal(after.Languages)
			episodes, _ := json.Marshal(after.Episodes)
			changed, x := tx.Exec(ctx, `UPDATE torrent_contents SET content_type=$2,content_source=$3,content_id=$4,languages=$5::jsonb,episodes=$6::jsonb,
video_resolution=$7,video_source=$8,video_codec=$9,video_3d=$10,video_modifier=$11,release_group=$12,english_audio=$13,english_audio_source=$14,
release_granularity=$15,release_date=$16,anime_absolute_episode=$17,is_anime=$18,tsv=$19::tsvector,release_attributes=$20::jsonb,updated_at=now() WHERE id=$1 AND content_source IS NULL AND content_id IS NULL`, before.ID, after.ContentType, after.ContentSource, after.ContentID, string(languages), string(episodes), after.VideoResolution, after.VideoSource, after.VideoCodec, after.Video3D, after.VideoModifier, after.ReleaseGroup, after.EnglishAudio, after.EnglishAudioSource, after.ReleaseGranularity, after.ReleaseDate, after.AnimeAbsoluteEpisode, after.IsAnime, after.Tsv.String(), claims)
			if x != nil {
				return x
			}
			if changed.RowsAffected() != 1 {
				return llmwork.ErrObsolete
			}
		}
		for _, tag := range tags {
			if _, x = tx.Exec(ctx, `INSERT INTO torrent_tags(info_hash,name,created_at,updated_at) VALUES($1,$2,now(),now()) ON CONFLICT DO NOTHING`, task.InfoHash, tag); x != nil {
				return x
			}
		}
		return llmwork.RecordApplication(ctx, tx, task, llmwork.NewApplicationSnapshot(task.Kind, after, tags))
	})
}

func sameDeferredTarget(a, b model.TorrentContent) bool {
	return bytes.Equal(llmwork.Digest(llmwork.NewApplicationSnapshot(llmwork.Language, a, nil)), llmwork.Digest(llmwork.NewApplicationSnapshot(llmwork.Language, b, nil))) && a.ID == b.ID && bytes.Equal(llmwork.Digest([]any{a.Content.Type, a.Content.Source, a.Content.ID, a.Content.Title, a.Content.ReleaseYear, a.Content.OriginalLanguage, a.Content.OriginalTitle, a.Content.Adult}), llmwork.Digest([]any{b.Content.Type, b.Content.Source, b.Content.ID, b.Content.Title, b.Content.ReleaseYear, b.Content.OriginalLanguage, b.Content.OriginalTitle, b.Content.Adult}))
}
func deferredContentTypeAllowed(ct model.NullContentType, cfg classifier.Config, flags classifier.Flags) bool {
	if !ct.Valid || (ct.ContentType != model.ContentTypeMovie && ct.ContentType != model.ContentTypeTvShow) {
		return false
	}
	blocked := append([]string(nil), cfg.DeleteContentTypes...)
	for _, fs := range []map[string]any{cfg.Flags, flags} {
		if v, ok := fs["delete_content_types"]; ok {
			switch values := v.(type) {
			case []string:
				blocked = append(blocked, values...)
			case []any:
				for _, value := range values {
					if s, ok := value.(string); ok {
						blocked = append(blocked, s)
					}
				}
			}
		}
	}
	for _, v := range blocked {
		if strings.EqualFold(v, ct.ContentType.String()) {
			return false
		}
	}
	return true
}
func ensureDeferredContent(ctx context.Context, tx pgx.Tx, tc *model.TorrentContent) error {
	if !tc.ContentID.Valid {
		return nil
	}
	c := tc.Content
	if c.Source == "" || c.ID == "" || c.Title == "" {
		return llmwork.ErrObsolete
	}
	_, err := tx.Exec(ctx, `INSERT INTO content(type,source,id,title,release_year,original_language,original_title,adult,tsv,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::tsvector,now(),now()) ON CONFLICT DO NOTHING`, c.Type.String(), c.Source, c.ID, c.Title, c.ReleaseYear, c.OriginalLanguage, c.OriginalTitle, c.Adult, c.Tsv.String())
	if err != nil {
		return err
	}
	var title string
	var year model.Year
	err = tx.QueryRow(ctx, `SELECT title,release_year,created_at FROM content WHERE type=$1 AND source=$2 AND id=$3 FOR SHARE`, c.Type.String(), c.Source, c.ID).Scan(&title, &year, &tc.Content.CreatedAt)
	if err != nil {
		return err
	}
	if title != c.Title || year != c.ReleaseYear {
		return fmt.Errorf("%w: catalogue content changed", llmwork.ErrObsolete)
	}
	return nil
}

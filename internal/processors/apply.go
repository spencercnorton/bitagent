package processors

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/classifier/parsers"
	"github.com/spencercnorton/bitagent/internal/database/fts"
	"github.com/spencercnorton/bitagent/internal/model"
	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
)

func effectiveType(l Lease) string {
	kind := l.Input.Deterministic.ContentType
	if kind != "" {
		return kind
	}
	for i, r := range l.Results {
		if l.Plan.Steps[i].Entry.Mode == "apply" && r.Result.Outcome == v1.Completed && r.Result.Proposal != nil && r.Result.Proposal.ContentType != "" {
			return r.Result.Proposal.ContentType
		}
	}
	return ""
}
func (s *Store) parsedInput(l Lease, src source) (classification.ContentAttributes, error) {
	return parsers.ParseVideoContentWithOptions(src.torrent(hex.EncodeToString(l.Hash)), classification.Result{ContentAttributes: classification.ContentAttributes{ContentType: model.NewNullContentType(model.ContentType(effectiveType(l)))}}, parsers.ParseOptions{NoiseV2: s.noiseV2})
}
func (s *Store) candidates(ctx context.Context, tx pgx.Tx, in v1.Input, l Lease) ([]v1.Candidate, error) {
	out := []v1.Candidate{}
	if in.Deterministic.Match != nil {
		return out, nil
	}
	var src source
	if e := json.Unmarshal(l.Source, &src); e != nil {
		return out, e
	}
	parsed, e := s.parsedInput(l, src)
	if e != nil {
		return out, nil
	}
	title := parsed.BaseTitle.String
	kind := effectiveType(l)
	if title == "" || (kind != "movie" && kind != "tv_show") {
		return out, nil
	}
	rows, e := tx.Query(ctx, `SELECT c.source,c.id,c.type,c.title,COALESCE(c.release_year,0),
 ARRAY(SELECT value FROM content_attributes a WHERE a.content_type=c.type AND a.content_source=c.source AND a.content_id=c.id AND a.key LIKE 'alt_title:%' ORDER BY a.key LIMIT 16),c.original_title
 FROM content c WHERE c.type=$1 AND c.tsv @@ plainto_tsquery('simple',$2) ORDER BY ts_rank(c.tsv,plainto_tsquery('simple',$2)) DESC,c.source,c.id LIMIT 32`, kind, title)
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		var c v1.Candidate
		var original *string
		if e = rows.Scan(&c.Source, &c.ID, &c.ContentType, &c.Title, &c.Year, &c.Aliases, &original); e != nil {
			return out, e
		}
		if original != nil && *original != "" {
			c.Aliases = append(c.Aliases, *original)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// apply is deliberately narrow: existing deterministic facts always win. Typed
// extraction, language and vectors remain attributed proposals, not physical
// facts or eligibility grants. A match can attach only an offered, still-present
// catalogue identity independently confirmed against the source title/year.
func (s *Store) apply(ctx context.Context, tx pgx.Tx, l Lease, src source) error {
	var tc model.TorrentContent
	if e := decodeRow(src.Contents[0], &tc); e != nil {
		return e
	}
	tc.Torrent = src.torrent(hex.EncodeToString(l.Hash))
	tc.InfoHash = tc.Torrent.InfoHash
	originalID := tc.ID
	changed := false
	canEnrich := true
	for i, r := range l.Results {
		if l.Plan.Steps[i].Entry.Mode == "apply" && (r.Result.Outcome == v1.Failed || r.Result.Outcome == v1.Deferred) {
			canEnrich = false
		}
	}

	for i, receipt := range l.Results {
		r, entry := receipt.Result, l.Plan.Steps[i].Entry
		if entry.Mode != "apply" || r.Proposal == nil {
			continue
		}
		p := r.Proposal
		if r.Outcome == v1.Excluded && p.Exclusion != nil {
			binding, _ := json.Marshal(r.Binding)
			if _, e := tx.Exec(ctx, `INSERT INTO processor_exclusions(info_hash,job_id,processor_id,reason,binding) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, l.Hash, l.ID, entry.ID, p.Exclusion.Reason, string(binding)); e != nil {
				return e
			}
		}
		if !canEnrich || r.Outcome != v1.Completed {
			continue
		}
		if p.ContentType != "" && !tc.ContentType.Valid && src.HintIsEmpty() {
			// Type output cannot overwrite any core or operator type. The normal
			// parser supplies release details; a plugin cannot invent them.
			if s.forbiddenTypes[p.ContentType] || s.names != nil && !s.names.EvaluateClassified(tc.InfoHash, tc.Torrent.Name, p.ContentType).Eligible {
				continue
			}
			if s.filter != nil {
				ext := ""
				if src.Extension != nil {
					ext = *src.Extension
				}
				exts := []string{}
				if src.FilesStatus == "single" && ext != "" {
					exts = append(exts, ext)
				}
				for _, f := range src.Files {
					if f.Extension != nil {
						exts = append(exts, *f.Extension)
					}
				}
				langs := []string{}
				for lang := range tc.Languages {
					langs = append(langs, string(lang))
				}
				if !s.filter.DecideDeterministic(contentfilter.Input{Title: src.Name, ContentType: p.ContentType, PrimaryExtension: ext, AllExtensions: exts, Languages: langs}).Allow {
					continue
				}
			}
			tc.ContentType = model.NewNullContentType(model.ContentType(p.ContentType))
			changed = true
			cl, e := classifier.EnrichTypeLocally(ctx, tc.Torrent, classification.Result{ContentAttributes: classification.ContentAttributes{ContentType: tc.ContentType}}, nil, classifier.LocalTypeEnrichmentOptions{ParseNoiseV2: s.noiseV2, SingleEpisodeMaxBytes: s.singleEpisodeMax})
			if e == nil {
				copyEnrichment(&tc, cl)
			}
		}
		if p.Match != nil && !tc.ContentID.Valid && tc.ContentType.Valid {
			// Catalogue changes may introduce another equally exact identity while
			// a job runs. Recheck uniqueness under a short lock; ranking is never
			// authority to choose between indistinguishable catalogue records.
			if _, e := tx.Exec(ctx, `LOCK TABLE content,content_attributes IN SHARE MODE`); e != nil {
				return e
			}
			current, e := s.candidates(ctx, tx, l.Input, l)
			if e != nil {
				return e
			}
			if len(current) == 32 || len(receipt.Candidates) == 32 {
				continue // truncated candidate sets cannot prove uniqueness
			}
			var raw []byte
			var tsv string
			e = tx.QueryRow(ctx, `SELECT to_jsonb(c)-'tsv',c.tsv::text FROM content c WHERE type=$1 AND source=$2 AND id=$3 FOR SHARE`, tc.ContentType.ContentType.String(), p.Match.Source, p.Match.ID).Scan(&raw, &tsv)
			if e == pgx.ErrNoRows {
				continue
			}
			if e != nil {
				return e
			}
			var content model.Content
			if e = decodeRow(raw, &content); e != nil {
				return e
			}
			offered := false
			for _, cand := range receipt.Candidates {
				if cand.MatchRef == *p.Match && cand.Title == content.Title && cand.Year == int(content.ReleaseYear) && cand.ContentType == content.Type.String() {
					offered = true
				}
			}
			if !offered {
				continue
			}
			parsed, e := parsers.ParseVideoContentWithOptions(tc.Torrent, classification.Result{ContentAttributes: classification.ContentAttributes{ContentType: tc.ContentType}}, parsers.ParseOptions{NoiseV2: s.noiseV2})
			if e != nil || !classifier.ExactContentTitle(parsed.BaseTitle.String, content) {
				continue
			}
			if content.Type == model.ContentTypeMovie && (parsed.Date.Year == 0 || parsed.Date.Year != content.ReleaseYear) {
				continue
			}
			if content.Type == model.ContentTypeTvShow && len(tc.Episodes) == 0 {
				continue
			}
			matches := 0
			selectedCurrent := false
			for _, cand := range current {
				c := model.Content{Title: cand.Title, Type: model.ContentType(cand.ContentType), ReleaseYear: model.Year(cand.Year)}
				exact := classifier.ExactContentTitle(parsed.BaseTitle.String, c)
				for _, alias := range cand.Aliases {
					c.OriginalTitle = model.NewNullString(alias)
					exact = exact || classifier.ExactContentTitle(parsed.BaseTitle.String, c)
				}
				if exact && (content.Type != model.ContentTypeMovie || parsed.Date.Year == c.ReleaseYear) {
					matches++
					selectedCurrent = selectedCurrent || cand.MatchRef == *p.Match
				}
			}
			if matches != 1 || !selectedCurrent {
				continue
			}
			content.Tsv, e = fts.ParseTsvector(tsv)
			if e != nil {
				return e
			}
			tc.Content = content
			tc.ContentSource = model.NewNullString(p.Match.Source)
			tc.ContentID = model.NewNullString(p.Match.ID)
			changed = true
		}
	}
	if changed {
		tc.UpdateTsv()
		episodes, _ := json.Marshal(tc.Episodes)
		var attrs any
		if tc.ReleaseAttributes != nil {
			encoded, _ := json.Marshal(tc.ReleaseAttributes)
			attrs = string(encoded)
		}
		// Update just core-validated fields. No catalogue delete, no privacy mutation,
		// no English-audio claim, no replacement of an existing match or manual hint.
		_, e := tx.Exec(ctx, `UPDATE torrent_contents SET content_type=$2,content_source=$3,content_id=$4,episodes=$5,video_resolution=$6,video_source=$7,video_codec=$8,video_3d=$9,video_modifier=$10,release_group=$11,release_attributes=$12,release_granularity=$13,tsv=$14,updated_at=clock_timestamp() WHERE id=$1`, originalID, tc.ContentType, tc.ContentSource, tc.ContentID, string(episodes), tc.VideoResolution, tc.VideoSource, tc.VideoCodec, tc.Video3D, tc.VideoModifier, tc.ReleaseGroup, attrs, tc.ReleaseGranularity, tc.Tsv.String())
		if e != nil {
			return e
		}
	}
	_, after, e := readSource(ctx, tx, l.Hash, false)
	if e != nil {
		return e
	}
	results, _ := json.Marshal(l.Results)
	_, e = tx.Exec(ctx, `INSERT INTO processor_applications(info_hash,job_id,input_revision,applied_revision,pipeline_revision,results) VALUES($1,$2,$3,$4,$5,$6)
 ON CONFLICT(info_hash) DO UPDATE SET job_id=$2,input_revision=$3,applied_revision=$4,pipeline_revision=$5,results=$6,applied_at=clock_timestamp()`, l.Hash, l.ID, l.InputRevision, digest(after), l.PipelineRevision, string(results))
	return e
}
func (s source) HintIsEmpty() bool {
	return len(s.Hint) == 0 || strings.TrimSpace(string(s.Hint)) == "null"
}
func copyEnrichment(tc *model.TorrentContent, cl classification.Result) {
	if !tc.VideoResolution.Valid {
		tc.VideoResolution = cl.VideoResolution
	}
	if !tc.VideoSource.Valid {
		tc.VideoSource = cl.VideoSource
	}
	if !tc.VideoCodec.Valid {
		tc.VideoCodec = cl.VideoCodec
	}
	if !tc.Video3D.Valid {
		tc.Video3D = cl.Video3D
	}
	if !tc.VideoModifier.Valid {
		tc.VideoModifier = cl.VideoModifier
	}
	if !tc.ReleaseGroup.Valid {
		tc.ReleaseGroup = cl.ReleaseGroup
	}
	if len(tc.Episodes) == 0 {
		tc.Episodes = cl.Episodes
	}
	if tc.ReleaseAttributes == nil {
		tc.ReleaseAttributes = cl.ReleaseAttributes
	}
	if !tc.ReleaseGranularity.Valid {
		tc.ReleaseGranularity = model.DeriveReleaseGranularity(tc.ContentType, tc.Episodes, tc.Torrent.Name)
	}
}

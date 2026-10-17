package processors

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"sort"
	"strings"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/classifier/parsers"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
)

func (s *Store) CoreParse(_ context.Context, req v1.CoreParseRequest) (v1.CoreParseResponse, error) {
	if len(req.Name) == 0 || len(req.Name) > 4096 || len(req.Files) > 256 {
		return v1.CoreParseResponse{}, ErrControl
	}
	if req.ContentType != "" && !slices.Contains(v1.ValidContentTypes, req.ContentType) {
		return v1.CoreParseResponse{}, ErrControl
	}
	t := model.Torrent{
		Name:        req.Name,
		Size:        uint(max(0, req.Size)),
		FilesStatus: model.FilesStatusSingle,
	}
	if len(req.Files) > 0 {
		t.FilesStatus = model.FilesStatusMulti
		t.FilesCount = model.NewNullUint(uint(len(req.Files)))
		for i, f := range req.Files {
			t.Files = append(t.Files, model.TorrentFile{
				Index: uint(i),
				Path:  f.Path,
				Size:  uint(max(0, f.Size)),
			})
		}
	}
	cl := classification.Result{}
	if req.ContentType != "" {
		cl.ContentType = model.NewNullContentType(model.ContentType(req.ContentType))
	}
	parsed, err := parsers.ParseVideoContentWithOptions(t, cl, parsers.ParseOptions{NoiseV2: s.noiseV2})
	if err != nil {
		return v1.CoreParseResponse{}, err
	}
	ex := v1.Extraction{
		Title: parsed.BaseTitle.String,
		Year:  int(parsed.Date.Year),
	}
	if !parsed.Date.IsNil() {
		ex.ReleaseDate = parsed.Date.IsoDateString()
	}
	if parsed.VideoResolution.Valid {
		ex.VideoResolution = parsed.VideoResolution.VideoResolution.String()
	}
	if parsed.VideoSource.Valid {
		ex.VideoSource = parsed.VideoSource.VideoSource.String()
	}
	if parsed.VideoCodec.Valid {
		ex.VideoCodec = parsed.VideoCodec.VideoCodec.String()
	}
	if parsed.Video3D.Valid {
		ex.Video3D = parsed.Video3D.Video3D.String()
	}
	if parsed.VideoModifier.Valid {
		ex.VideoModifier = parsed.VideoModifier.VideoModifier.String()
	}
	if parsed.ReleaseGroup.Valid {
		ex.ReleaseGroup = parsed.ReleaseGroup.String
	}
	if len(parsed.Episodes) > 0 {
		entries := parsed.Episodes.SeasonEntries()
		if len(entries) > 0 {
			season := entries[0].Season
			ex.Season = &season
			ex.Episodes = append(ex.Episodes, entries[0].Episodes...)
		}
	}
	gran := model.DeriveReleaseGranularity(cl.ContentType, parsed.Episodes, req.Name)
	if gran.Valid {
		ex.ReleaseGranularity = gran.ReleaseGranularity.String()
	}
	if parsed.ReleaseAttributes != nil {
		ex.HDRFormats = append([]string{}, parsed.ReleaseAttributes.HDRFormats...)
		ex.AudioFormats = append([]string{}, parsed.ReleaseAttributes.AudioFormats...)
		if parsed.ReleaseAttributes.AudioChannels != nil && *parsed.ReleaseAttributes.AudioChannels != "" {
			ex.AudioChannels = []string{*parsed.ReleaseAttributes.AudioChannels}
		}
		ex.VideoEditions = append([]string{}, parsed.ReleaseAttributes.AudioFeatures...)
		ex.ReleaseFlags = append([]string{}, parsed.ReleaseAttributes.Revisions...)
	}
	var lang *v1.LanguageClaim
	if len(parsed.Languages) > 0 {
		codes := make([]string, 0, len(parsed.Languages))
		for l := range parsed.Languages {
			codes = append(codes, string(l))
		}
		sort.Strings(codes)
		lang = &v1.LanguageClaim{
			Audio:     codes,
			Subtitles: []string{},
			Evidence:  "metadata_claim",
		}
	}
	outType := req.ContentType
	if outType == "" && parsed.ContentType.Valid {
		outType = parsed.ContentType.ContentType.String()
	}
	return v1.CoreParseResponse{
		Contract:    v1.CoreOpsContract,
		Extraction:  ex,
		Language:    lang,
		ContentType: outType,
	}, nil
}

func (s *Store) CoreSearch(ctx context.Context, req v1.CoreSearchRequest) (v1.CoreSearchResponse, error) {
	if !slices.Contains(v1.ValidContentTypes, req.ContentType) {
		return v1.CoreSearchResponse{}, ErrControl
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 16
	}
	if limit > 32 {
		limit = 32
	}
	p, err := s.pool.Get()
	if err != nil {
		return v1.CoreSearchResponse{}, err
	}
	out := v1.CoreSearchResponse{
		Contract:   v1.CoreOpsContract,
		Candidates: []v1.Candidate{},
	}
	if req.Source != "" && req.ExternalID != "" {
		rows, err := p.Query(ctx, `SELECT c.source,c.id,c.type,c.title,COALESCE(c.release_year,0),
  ARRAY(SELECT value FROM content_attributes a WHERE a.content_type=c.type AND a.content_source=c.source AND a.content_id=c.id AND a.key LIKE 'alt_title:%' ORDER BY a.key LIMIT 16),c.original_title
  FROM content c WHERE c.type=$1 AND c.source=$2 AND c.id=$3 LIMIT $4`, req.ContentType, req.Source, req.ExternalID, limit)
		if err != nil {
			return out, err
		}
		defer rows.Close()
		for rows.Next() {
			var c v1.Candidate
			var original *string
			if err = rows.Scan(&c.Source, &c.ID, &c.ContentType, &c.Title, &c.Year, &c.Aliases, &original); err != nil {
				return out, err
			}
			if original != nil && *original != "" {
				c.Aliases = append(c.Aliases, *original)
			}
			out.Candidates = append(out.Candidates, c)
		}
		return out, rows.Err()
	}
	title := strings.TrimSpace(req.Title)
	if title == "" || len(title) > 512 {
		return out, ErrControl
	}
	rows, err := p.Query(ctx, `SELECT c.source,c.id,c.type,c.title,COALESCE(c.release_year,0),
  ARRAY(SELECT value FROM content_attributes a WHERE a.content_type=c.type AND a.content_source=c.source AND a.content_id=c.id AND a.key LIKE 'alt_title:%' ORDER BY a.key LIMIT 16),c.original_title
  FROM content c WHERE c.type=$1 AND c.tsv @@ plainto_tsquery('simple',$2) AND ($3=0 OR COALESCE(c.release_year,0)=$3)
  ORDER BY ts_rank(c.tsv,plainto_tsquery('simple',$2)) DESC,c.source,c.id LIMIT $4`, req.ContentType, title, req.Year, limit)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var c v1.Candidate
		var original *string
		if err = rows.Scan(&c.Source, &c.ID, &c.ContentType, &c.Title, &c.Year, &c.Aliases, &original); err != nil {
			return out, err
		}
		if original != nil && *original != "" {
			c.Aliases = append(c.Aliases, *original)
		}
		out.Candidates = append(out.Candidates, c)
	}
	return out, rows.Err()
}

func (s *Store) CoreLiveness(ctx context.Context, req v1.CoreLivenessRequest) (v1.CoreLivenessResponse, error) {
	hash, err := hex.DecodeString(strings.ToLower(strings.TrimSpace(req.InfoHash)))
	if err != nil || len(hash) != 20 {
		return v1.CoreLivenessResponse{}, ErrControl
	}
	p, err := s.pool.Get()
	if err != nil {
		return v1.CoreLivenessResponse{}, err
	}
	out := v1.CoreLivenessResponse{
		Contract: v1.CoreOpsContract,
		InfoHash: hex.EncodeToString(hash),
		Liveness: v1.LivenessClaim{
			Observation: "unknown",
			Source:      "core_postgres",
		},
	}
	var seeders, leechers *int
	_ = p.QueryRow(ctx, `SELECT seeders,leechers FROM torrent_contents WHERE info_hash=$1 LIMIT 1`, hash).Scan(&seeders, &leechers)
	out.Liveness.Seeders = seeders
	out.Liveness.Leechers = leechers
	var state string
	if err := p.QueryRow(ctx, `SELECT state FROM torrent_liveness WHERE info_hash=$1`, hash).Scan(&state); err == nil && state != "" {
		out.Liveness.Observation = state
	} else if seeders != nil {
		if *seeders > 0 {
			out.Liveness.Observation = "live"
		} else {
			out.Liveness.Observation = "zero_seeders"
		}
	}
	_ = p.QueryRow(ctx, `SELECT count(*) FROM torrent_tracker_seeds WHERE info_hash=$1`, hash).Scan(&out.TrackerCount)
	return out, nil
}

func (s *Store) CorePolicy(_ context.Context, req v1.CorePolicyRequest) (v1.CorePolicyResponse, error) {
	if len(req.Name) == 0 || len(req.Name) > 4096 {
		return v1.CorePolicyResponse{}, ErrControl
	}
	out := v1.CorePolicyResponse{
		Contract:         v1.CoreOpsContract,
		Allowed:          true,
		NameEligible:     true,
		FilterAllowed:    true,
		ExtensionAllowed: true,
		Reasons:          []string{},
	}
	if req.ContentType != "" && s.forbiddenTypes[req.ContentType] {
		out.Allowed = false
		out.FilterAllowed = false
		out.Reasons = append(out.Reasons, "forbidden_content_type")
	}
	var id protocol.ID
	if req.InfoHash != "" {
		parsedID, err := protocol.ParseID(req.InfoHash)
		if err != nil {
			return out, ErrControl
		}
		id = parsedID
	}
	if s.names != nil {
		decision := s.names.Evaluate(id, req.Name)
		if req.ContentType != "" {
			decision = s.names.EvaluateClassified(id, req.Name, req.ContentType)
		}
		if !decision.Eligible {
			out.Allowed = false
			out.NameEligible = false
			if decision.Reason != "" {
				out.Reasons = append(out.Reasons, decision.Reason)
			} else {
				out.Reasons = append(out.Reasons, "name_policy_rejected")
			}
		}
	}
	if s.filter != nil && req.ContentType != "" {
		fDec := s.filter.DecideDeterministic(contentfilter.Input{
			Title:            req.Name,
			ContentType:      req.ContentType,
			PrimaryExtension: req.PrimaryExtension,
			AllExtensions:    req.AllExtensions,
			Languages:        req.Languages,
		})
		if !fDec.Allow {
			out.Allowed = false
			out.FilterAllowed = false
			if fDec.Reason != contentfilter.ReasonNone {
				out.Reasons = append(out.Reasons, fDec.Reason.String())
			} else {
				out.Reasons = append(out.Reasons, "content_filter_rejected")
			}
		}
	}
	return out, nil
}

var _ = errors.New

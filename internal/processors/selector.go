package processors

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
)

type torrentContext struct {
	InfoHash        []byte
	ContentType     string
	Extensions      []string
	IsAnime         bool
	HasContentMatch bool
	Excluded        bool
	LastUncertainty *float64
	Tags            map[string]bool
	Scores          map[string]float64
	ExtractedTitle  string
	ExtractedYear   int
}

func normalizeExt(ext string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(ext)), ".")
}

func buildTorrentContext(hash []byte, src source, in v1.Input, results []Receipt, plan Plan, upToStep int) torrentContext {
	tc := torrentContext{
		InfoHash:        hash,
		ContentType:     in.Deterministic.ContentType,
		HasContentMatch: in.Deterministic.Match != nil,
		Tags:            map[string]bool{},
		Scores:          map[string]float64{},
		ExtractedTitle:  in.Deterministic.Title,
		ExtractedYear:   in.Deterministic.Year,
	}
	for _, t := range src.Tags {
		tc.Tags[t] = true
	}
	seenExt := map[string]bool{}
	addExt := func(e *string) {
		if e == nil {
			return
		}
		n := normalizeExt(*e)
		if n != "" && !seenExt[n] {
			seenExt[n] = true
			tc.Extensions = append(tc.Extensions, n)
		}
	}
	addExt(src.Extension)
	for _, f := range src.Files {
		addExt(f.Extension)
	}
	if len(src.Contents) == 1 {
		var row struct {
			IsAnime bool `json:"is_anime"`
		}
		_ = json.Unmarshal(src.Contents[0], &row)
		tc.IsAnime = row.IsAnime
	}
	limit := min(upToStep, len(results))
	for i := 0; i < limit; i++ {
		r := results[i].Result
		if r.Uncertainty != nil {
			u := *r.Uncertainty
			tc.LastUncertainty = &u
		}
		if i >= len(plan.Steps) || plan.Steps[i].Entry.Mode != "apply" {
			continue
		}
		if r.Outcome == v1.Excluded && r.Proposal != nil && r.Proposal.Exclusion != nil {
			tc.Excluded = true
		}
		if r.Outcome != v1.Completed || r.Proposal == nil {
			continue
		}
		p := r.Proposal
		if p.ContentType != "" && tc.ContentType == "" {
			tc.ContentType = p.ContentType
		}
		if p.Extraction != nil {
			if p.Extraction.Title != "" {
				tc.ExtractedTitle = p.Extraction.Title
			}
			if p.Extraction.Year > 0 {
				tc.ExtractedYear = p.Extraction.Year
			}
			if p.Extraction.IsAnime != nil {
				tc.IsAnime = *p.Extraction.IsAnime
			}
		}
		if p.Match != nil && p.Match.ID != "" {
			tc.HasContentMatch = true
		}
		for _, tag := range p.AddTags {
			tc.Tags[tag] = true
		}
		for _, tag := range p.RemoveTags {
			delete(tc.Tags, tag)
		}
		for dim, w := range p.Weights {
			tc.Scores[dim] += w
		}
	}
	return tc
}

func matchesSelector(sel *v1.Selector, processorID string, tc torrentContext) bool {
	if sel.IsEmpty() {
		return true
	}
	effectiveType := tc.ContentType
	if effectiveType == "" {
		effectiveType = "unknown"
	}
	if len(sel.ContentTypes) > 0 && !slices.Contains(sel.ContentTypes, effectiveType) {
		return false
	}
	if len(sel.ExcludeTypes) > 0 && slices.Contains(sel.ExcludeTypes, effectiveType) {
		return false
	}
	if len(sel.Extensions) > 0 {
		matchedExt := false
		for _, want := range sel.Extensions {
			if slices.Contains(tc.Extensions, normalizeExt(want)) {
				matchedExt = true
				break
			}
		}
		if !matchedExt {
			return false
		}
	}
	if sel.IsAnime != nil && tc.IsAnime != *sel.IsAnime {
		return false
	}
	if sel.HasContentMatch != nil && tc.HasContentMatch != *sel.HasContentMatch {
		return false
	}
	if sel.Excluded != nil && tc.Excluded != *sel.Excluded {
		return false
	}
	if sel.MinUncertainty != nil {
		if tc.LastUncertainty != nil && *tc.LastUncertainty < *sel.MinUncertainty {
			return false
		}
	}
	for _, reqTag := range sel.RequiredTags {
		if !tc.Tags[reqTag] {
			return false
		}
	}
	for _, st := range sel.ScoreThresholds {
		val := tc.Scores[st.Dimension]
		if st.Min != nil && val < *st.Min {
			return false
		}
		if st.Max != nil && val > *st.Max {
			return false
		}
	}
	if sel.SampleBasisPoints != nil {
		bps := *sel.SampleBasisPoints
		if bps <= 0 {
			return false
		}
		if bps < 10000 {
			sum := sha256.Sum256([]byte(processorID + ":" + hex.EncodeToString(tc.InfoHash)))
			bucket := int(binary.BigEndian.Uint64(sum[:8]) % 10000)
			if bucket >= bps {
				return false
			}
		}
	}
	return true
}

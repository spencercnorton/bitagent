package processor

import (
	"context"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/assert"
)

type postFilterLLM struct {
	calls atomic.Int32
}

func (f *postFilterLLM) Classify(context.Context, string) (contentfilter.LLMVerdict, error) {
	f.calls.Add(1)
	return contentfilter.LLMVerdict{IsEnglish: true, Confidence: 1}, nil
}

func privateGateFilter(llm contentfilter.LLMClient) *contentfilter.Filter {
	cfg := contentfilter.NewDefaultConfig()
	cfg.Enabled = true
	cfg.LLMEnabled = true
	cfg.LLMDailyBudget = 10
	cfg.LLMCacheTTL = "1h"
	cfg.LLMRuleMinerWindow = "1h"
	return contentfilter.NewWithLLM(cfg, llm, contentfilter.LLMCallbacks{})
}

// nullExt is a small helper so tests don't have to import the
// undocumented model.NewNullString shape inline; equivalent to a
// "valid, populated" NullString.
func nullExt(s string) model.NullString {
	return model.NullString{String: s, Valid: true}
}

// nullCT helper for ContentType.
func nullCT(c model.ContentType) model.NullContentType {
	return model.NewNullContentType(c)
}

func TestTorrentToFilterInput_PrimaryByLargestFile(t *testing.T) {
	// Three files: a tiny .nfo, a huge .mkv, and a small .srt. The
	// filter cares about the largest-by-size file's extension.
	tor := model.Torrent{
		Name: "Some Movie 2024 1080p",
		Files: []model.TorrentFile{
			{Path: "Some Movie 2024 1080p.nfo", Extension: nullExt("nfo"), Size: 4096},
			{Path: "Some Movie 2024 1080p.mkv", Extension: nullExt("mkv"), Size: 8_000_000_000},
			{Path: "Some Movie 2024 1080p.srt", Extension: nullExt("srt"), Size: 65_536},
		},
	}
	in := torrentToFilterInput(tor, classification.Result{})
	assert.Equal(t, "Some Movie 2024 1080p", in.Title)
	assert.Equal(t, "mkv", in.PrimaryExtension,
		"primary should be the largest file's extension")
	sort.Strings(in.AllExtensions)
	assert.Equal(t, []string{"mkv", "nfo", "srt"}, in.AllExtensions)
}

func TestTorrentToFilterInput_PropagatesNativePrivateFlag(t *testing.T) {
	tor := model.Torrent{
		Name:    "Private.Release.2026.mkv",
		Private: true,
	}
	in := torrentToFilterInput(tor, classification.Result{})
	assert.True(t, in.Private, "post-classifier content filter must retain the authoritative metainfo flag")
}

func TestNativePrivatePostFilterNeverCallsLLMWithMissingOrStaleEvidence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		privacy PrivacyStore
	}{
		{name: "missing evidence"},
		{name: "stale public evidence", privacy: &fakePrivacy{isPrivate: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := processor{privacy: tc.privacy}
			tor := model.Torrent{
				Name:      "Private Release 2026",
				Private:   true,
				Extension: nullExt("mkv"),
			}
			if p.shouldSkipContentFilter(context.Background(), tor, classification.Result{}) {
				t.Fatal("native private torrent should retain deterministic filtering")
			}

			llm := &postFilterLLM{}
			d := privateGateFilter(llm).Decide(torrentToFilterInput(tor, classification.Result{}))
			if !d.Allow || d.WouldDrop {
				t.Fatalf("residual private torrent should be kept without model use: %+v", d)
			}
			if llm.calls.Load() != 0 {
				t.Fatalf("native private torrent reached content-filter LLM %d times", llm.calls.Load())
			}
		})
	}
}

func TestTorrentToFilterInput_FallsBackToTorrentExtension_WhenFilesUnhydrated(t *testing.T) {
	// Single-file torrents in the preload window may arrive with an
	// empty Files slice but the torrent-level Extension populated.
	// metaInfoToFilterInput handles that pre-classifier; the
	// post-classifier helper must too — otherwise the
	// blocked-extension drop reason would silently miss single-file
	// torrents at this hook.
	tor := model.Torrent{
		Name:      "ubuntu-24.04.1-desktop-amd64.iso",
		Extension: nullExt("iso"),
		Files:     nil,
	}
	in := torrentToFilterInput(tor, classification.Result{})
	assert.Equal(t, "iso", in.PrimaryExtension)
	assert.Equal(t, []string{"iso"}, in.AllExtensions)
}

func TestTorrentToFilterInput_ExtensionFromPath_WhenNullStringEmpty(t *testing.T) {
	// Some old rows have a non-empty Path but a NULL Extension column.
	// The fallback parses the extension from the path so the filter
	// still has a primary-by-size signal.
	tor := model.Torrent{
		Name: "Some.Album.2024",
		Files: []model.TorrentFile{
			{Path: "Some.Album.2024/01-track.flac", Size: 50_000_000},
			{Path: "Some.Album.2024/02-track.flac", Size: 48_000_000},
		},
	}
	in := torrentToFilterInput(tor, classification.Result{})
	assert.Equal(t, "flac", in.PrimaryExtension)
	assert.Equal(t, []string{"flac"}, in.AllExtensions)
}

func TestTorrentToFilterInput_LanguageAndContentTypeThreaded(t *testing.T) {
	// Post-classifier hook's whole reason for existing: language tag
	// and content type are NON-EMPTY here so Filter.Decide knows to
	// short-circuit the LLM tier (the "residual" cohort is exactly
	// titles where Languages is empty).
	tor := model.Torrent{
		Name: "Some Show S01E01 1080p",
		Files: []model.TorrentFile{
			{Path: "Some Show S01E01 1080p.mkv", Extension: nullExt("mkv"), Size: 2_000_000_000},
		},
	}
	cl := classification.Result{
		ContentAttributes: classification.ContentAttributes{
			ContentType: nullCT(model.ContentTypeTvShow),
			Languages: model.Languages{
				model.Language("en"): {},
				model.Language("es"): {},
			},
		},
	}
	in := torrentToFilterInput(tor, cl)
	assert.Equal(t, "tv_show", in.ContentType,
		"ContentType must round-trip from cl.ContentType.ContentType.String()")
	sort.Strings(in.Languages)
	assert.Equal(t, []string{"en", "es"}, in.Languages,
		"Languages must contain every language from cl.Languages, not just one")
}

func TestTorrentToFilterInput_EmptyLanguagesEmptyContentType_ResidualCohort(t *testing.T) {
	// When the classifier didn't language-tag or content-type the
	// torrent (TMDB miss), the input must surface that as
	// `Languages=[]` and `ContentType=""` so Filter.shouldConsultLLM
	// flips to true and the LLM tier engages. This is the cohort the
	// LLM tier exists for.
	tor := model.Torrent{
		Name: "Vladimir.Putin.Pinata.2024.WEBRip-RARBG",
		Files: []model.TorrentFile{
			{Path: "vp-2024.mkv", Extension: nullExt("mkv"), Size: 1_500_000_000},
		},
	}
	in := torrentToFilterInput(tor, classification.Result{})
	assert.Equal(t, "", in.ContentType)
	assert.Empty(t, in.Languages,
		"empty Languages is the LLM-tier trigger condition; helper must NOT inject a default like 'en'")
}

func TestTorrentToFilterInput_DuplicateExtensionsDeduped(t *testing.T) {
	// Multi-file torrent with several .mp3s. AllExtensions is a
	// dedupe-set semantically; the contentfilter mp3-only check
	// keys off "every audio ext is mp3" so duplicates would just
	// inflate the slice.
	tor := model.Torrent{
		Name: "An Album",
		Files: []model.TorrentFile{
			{Path: "01.mp3", Extension: nullExt("mp3"), Size: 5_000_000},
			{Path: "02.mp3", Extension: nullExt("mp3"), Size: 5_500_000},
			{Path: "03.mp3", Extension: nullExt("mp3"), Size: 5_200_000},
			{Path: "cover.jpg", Extension: nullExt("jpg"), Size: 80_000},
		},
	}
	in := torrentToFilterInput(tor, classification.Result{})
	assert.Equal(t, "mp3", in.PrimaryExtension)
	sort.Strings(in.AllExtensions)
	assert.Equal(t, []string{"jpg", "mp3"}, in.AllExtensions,
		"AllExtensions must be deduped")
}

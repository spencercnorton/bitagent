package classifier

import (
	"context"
	"errors"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/require"
)

type typeEnrichmentSearch struct {
	LocalSearch
	candidates []model.Content
	err        error
	calls      int
}

func (s *typeEnrichmentSearch) ContentCandidatesBySearch(ctx context.Context, ct model.ContentType, title string, year model.Year, limit int) ([]model.Content, error) {
	s.calls++
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("local continuation has no deadline")
	}
	return s.candidates, s.err
}

func typeEnrichmentRunner(t *testing.T, search LocalSearch) Runner {
	t.Helper()
	source, err := yamlSourceProvider{rawSourceProvider: coreSourceProvider{}}.source()
	require.NoError(t, err)
	source.Workflows["repeated"] = []any{
		map[string]any{"run_workflow": []any{"default", "default"}},
	}
	c := compiler{
		options:      []compilerOption{compilerFeatures(defaultFeatures), celEnvOption},
		dependencies: dependencies{search: search, parseNoiseV2: true},
	}
	r, err := c.Compile(source)
	require.NoError(t, err)
	return r
}

func typeEnrichmentTorrent(name string) model.Torrent {
	return model.Torrent{
		Name: name, Size: 70 * 1024 * 1024,
		FilesStatus: model.FilesStatusSingle, Extension: model.NewNullString("mkv"),
	}
}

func typeEnrichmentFlags() Flags {
	return Flags{"apis_enabled": false, "tmdb_enabled": false, "llm_match_enabled": false}
}

func TestTypeFallbackLocalContinuationRunsOnceAndRetainsSourceAttributes(t *testing.T) {
	for _, tc := range []struct {
		name, release, title string
		ct                   model.ContentType
		year                 model.Year
	}{
		{"movie", "Amber.Signal.2025.1080p.BluRay.x265-GROUP.mkv", "Amber Signal", model.ContentTypeMovie, 2025},
		{"tv", "Amber.Signal.S02E04.1080p.BluRay.x265-GROUP.mkv", "Amber Signal", model.ContentTypeTvShow, 2019},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := model.Content{Type: tc.ct, Source: "tmdb", ID: "42", Title: tc.title, ReleaseYear: tc.year}
			local := &typeEnrichmentSearch{candidates: []model.Content{content}}
			runner := typeEnrichmentRunner(t, local)
			var calls int
			ctx := WithTypeFallback(context.Background(), func(_ context.Context, cl classification.Result) (classification.Result, error) {
				calls++
				cl.ContentType = model.NewNullContentType(tc.ct)
				return cl, nil
			})
			got, err := runner.Run(ctx, "repeated", typeEnrichmentFlags(), typeEnrichmentTorrent(tc.release))
			require.NoError(t, err)
			require.Equal(t, &content, got.Content)
			require.Equal(t, model.NewNullVideoResolution(model.VideoResolutionV1080p), got.VideoResolution)
			require.Equal(t, model.NewNullVideoSource(model.VideoSourceBluRay), got.VideoSource)
			require.Equal(t, model.NewNullVideoCodec(model.VideoCodecX265), got.VideoCodec)
			require.Equal(t, model.NewNullString("GROUP"), got.ReleaseGroup)
			require.Contains(t, got.Tags, typeLocalEnrichedTagName)
			require.NotContains(t, got.Tags, llmMatchedTagName)
			require.Equal(t, 1, calls, "nested default workflows cannot infer twice")
			require.Equal(t, 1, local.calls, "the continuation cannot recurse or enrich twice")
			if tc.ct == model.ContentTypeTvShow {
				require.Equal(t, model.Episodes{2: {4: {}}}, got.Episodes)
			}
		})
	}
}

func TestTypeFallbackLocalContinuationDoesNotRunBeforeExcludedTypePolicy(t *testing.T) {
	local := &typeEnrichmentSearch{candidates: []model.Content{{Type: model.ContentTypeMovie, Source: "tmdb", ID: "42", Title: "Amber Signal", ReleaseYear: 2025}}}
	r := typeEnrichmentRunner(t, local)
	ctx := WithTypeFallback(context.Background(), func(_ context.Context, cl classification.Result) (classification.Result, error) {
		cl.ContentType = model.NewNullContentType(model.ContentTypeMovie)
		return cl, nil
	})
	flags := typeEnrichmentFlags()
	flags["delete_content_types"] = []any{"movie"}
	got, err := r.Run(ctx, "default", flags, typeEnrichmentTorrent("Amber.Signal.2025.1080p.mkv"))
	require.ErrorIs(t, err, classification.ErrDeleteTorrent)
	require.Equal(t, classification.Result{}, got)
	require.Zero(t, local.calls)
}

func TestTypeFallbackLocalContinuationDeclinesUnavailableAndAmbiguousIdentity(t *testing.T) {
	content := model.Content{Type: model.ContentTypeTvShow, Source: "tmdb", ID: "42", Title: "Amber Signal", ReleaseYear: 2019}
	duplicate := content
	duplicate.ID = "43"
	for _, tc := range []struct {
		name       string
		candidates []model.Content
		err        error
	}{
		{"no candidates", nil, nil},
		{"lookup failure", nil, errors.New("local lookup unavailable")},
		{"same title different identity", []model.Content{content, duplicate}, nil},
		{"saturated candidate set", make([]model.Content, typeLocalCandidateLimit), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := &typeEnrichmentSearch{candidates: tc.candidates, err: tc.err}
			ctx := WithTypeFallback(context.Background(), func(_ context.Context, cl classification.Result) (classification.Result, error) {
				cl.ContentType = model.NewNullContentType(model.ContentTypeTvShow)
				return cl, nil
			})
			got, err := typeEnrichmentRunner(t, local).Run(ctx, "default", typeEnrichmentFlags(), typeEnrichmentTorrent("Amber.Signal.S02E04.1080p.mkv"))
			require.NoError(t, err)
			require.Equal(t, model.NewNullContentType(model.ContentTypeTvShow), got.ContentType)
			require.Nil(t, got.Content)
			require.True(t, got.VideoResolution.Valid)
			require.NotContains(t, got.Tags, typeLocalEnrichedTagName)
		})
	}
}

func TestUniqueTypeLocalIdentityUsesOnlyExactSourceEvidence(t *testing.T) {
	cl := classification.Result{ContentAttributes: classification.ContentAttributes{
		ContentType: model.NewNullContentType(model.ContentTypeMovie), BaseTitle: model.NewNullString("Amber Signal"), Date: model.Date{Year: 2025},
	}}
	content := model.Content{Type: model.ContentTypeMovie, Source: "tmdb", ID: "42", Title: "Amber Signal", ReleaseYear: 2025}
	for _, tc := range []struct {
		name string
		edit func(*model.Content)
	}{
		{"different title", func(c *model.Content) { c.Title = "Amber Signals" }},
		{"wrong remake year", func(c *model.Content) { c.ReleaseYear = 1984 }},
		{"unknown year", func(c *model.Content) { c.ReleaseYear = 0 }},
		{"wrong type", func(c *model.Content) { c.Type = model.ContentTypeTvShow }},
		{"missing identity", func(c *model.Content) { c.ID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := content
			tc.edit(&bad)
			_, ok := uniqueTypeLocalContent(cl, []model.Content{bad}, true)
			require.False(t, ok)
		})
	}
	alias := content
	alias.Title = "Amber Beacon"
	alias.OriginalTitle = model.NewNullString("Amber Signal")
	_, ok := uniqueTypeLocalContent(cl, []model.Content{alias, alias}, false)
	require.True(t, ok, "duplicate rows of one identity are not different works")
	cl.Date.Year = 0
	_, ok = uniqueTypeLocalContent(cl, []model.Content{content}, true)
	require.False(t, ok, "yearless movie cannot borrow a remake identity")
}

func TestTypeFallbackLocalContinuationHonorsLocalSearchFlagAndRunScope(t *testing.T) {
	content := model.Content{Type: model.ContentTypeMovie, Source: "tmdb", ID: "42", Title: "Amber Signal", ReleaseYear: 2025}
	local := &typeEnrichmentSearch{candidates: []model.Content{content}}
	r := typeEnrichmentRunner(t, local)
	var calls int
	ctx := WithTypeFallback(context.Background(), func(_ context.Context, cl classification.Result) (classification.Result, error) {
		calls++
		cl.ContentType = model.NewNullContentType(model.ContentTypeMovie)
		return cl, nil
	})
	tor := typeEnrichmentTorrent("Amber.Signal.2025.1080p.BluRay.x265-GROUP.mkv")
	flags := typeEnrichmentFlags()
	flags["local_search_enabled"] = false
	got, err := r.Run(ctx, "default", flags, tor)
	require.NoError(t, err)
	require.True(t, got.VideoResolution.Valid, "source attributes are available without identity lookup")
	require.Nil(t, got.Content)
	require.Zero(t, local.calls)
	flags["local_search_enabled"] = true
	for range 2 {
		got, err = r.Run(ctx, "default", flags, tor)
		require.NoError(t, err)
		require.Equal(t, &content, got.Content)
	}
	require.Equal(t, 3, calls, "each Run owns its independent continuation state")
	require.Equal(t, 2, local.calls)
}

func TestTypeFallbackLocalContinuationLeavesExistingTypesAndNonVideoPredictionsAlone(t *testing.T) {
	for _, ct := range []model.ContentType{model.ContentTypeMovie, model.ContentTypeMusic} {
		t.Run(ct.String(), func(t *testing.T) {
			local := &typeEnrichmentSearch{}
			r := typeEnrichmentRunner(t, local)
			var calls int
			ctx := WithTypeFallback(context.Background(), func(_ context.Context, cl classification.Result) (classification.Result, error) {
				calls++
				cl.ContentType = model.NewNullContentType(ct)
				return cl, nil
			})
			tor := typeEnrichmentTorrent("Amber.Signal.2025.1080p.mkv")
			flags := typeEnrichmentFlags()
			flags["local_search_enabled"] = false
			if ct == model.ContentTypeMovie {
				tor.Hint.ContentType = ct
			}
			got, err := r.Run(ctx, "default", flags, tor)
			require.NoError(t, err)
			require.Equal(t, model.NewNullContentType(ct), got.ContentType)
			require.Nil(t, got.Content)
			require.Zero(t, local.calls)
			require.NotContains(t, got.Tags, typeLocalEnrichedTagName)
			if ct == model.ContentTypeMovie {
				require.Zero(t, calls)
			} else {
				require.Equal(t, 1, calls)
				require.False(t, got.VideoResolution.Valid)
			}
		})
	}
}

func TestEnrichTypeLocallyIsReusableWithoutWorkflowOrTypeInference(t *testing.T) {
	content := model.Content{Type: model.ContentTypeTvShow, Source: "tmdb", ID: "42", Title: "Amber Signal", ReleaseYear: 2019}
	local := &typeEnrichmentSearch{candidates: []model.Content{content}}
	tor := typeEnrichmentTorrent("Amber.Signal.S02E04.1080p.BluRay.x265-GROUP.mkv")
	cl := classification.Result{ContentAttributes: classification.ContentAttributes{ContentType: model.NewNullContentType(model.ContentTypeTvShow)}}
	ctx := WithTypeFallback(context.Background(), func(context.Context, classification.Result) (classification.Result, error) {
		t.Fatal("the reusable local helper must not infer another type")
		return classification.Result{}, nil
	})
	got, err := EnrichTypeLocally(ctx, tor, cl, local, LocalTypeEnrichmentOptions{LocalSearchEnabled: true})
	require.NoError(t, err)
	require.Equal(t, &content, got.Content)
	require.Equal(t, model.Episodes{2: {4: {}}}, got.Episodes)
	require.True(t, got.VideoCodec.Valid)
	require.Equal(t, 1, local.calls)
	again, err := EnrichTypeLocally(ctx, tor, got, local, LocalTypeEnrichmentOptions{LocalSearchEnabled: true})
	require.NoError(t, err)
	require.Equal(t, got, again)
	require.Equal(t, 1, local.calls, "an attached result is already complete")
	require.Nil(t, NewLocalSearch(nil, NewDefaultConfig()))
}

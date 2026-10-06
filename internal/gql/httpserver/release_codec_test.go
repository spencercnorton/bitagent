package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/99designs/gqlgen/graphql"
	"github.com/gin-gonic/gin"
	"github.com/spencercnorton/bitagent/internal/database/query"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/gql"
	"github.com/spencercnorton/bitagent/internal/gql/resolvers"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/require"
)

type codecClaimSearch struct{ search.Search }

func (codecClaimSearch) TorrentContent(context.Context, ...query.Option) (search.TorrentContentResult, error) {
	var items []search.TorrentContentResultItem
	for _, codec := range []model.VideoCodec{model.VideoCodecX265, model.VideoCodecHEVC, model.VideoCodecAV1} {
		items = append(items, search.TorrentContentResultItem{TorrentContent: model.TorrentContent{
			ID: codec.String(), VideoCodec: model.NewNullVideoCodec(codec), ReleaseGroup: model.NewNullString("GROUP"),
			EnglishAudio: model.NewNullEnglishAudio(model.EnglishAudioDub),
		}})
	}
	return search.TorrentContentResult{Items: items}, nil
}

func TestGraphQLRetainsLegacyAndModernCodecClaims(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	b := builder{schema: lazy.New(func() (graphql.ExecutableSchema, error) {
		return gql.NewExecutableSchema(gql.Config{Resolvers: &resolvers.Resolver{Search: codecClaimSearch{}}}), nil
	})}
	require.NoError(t, b.Apply(engine))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{ torrentContent { search(input:{}) { items { videoCodec releaseGroup englishAudio } } } }"}`))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `{"data":{"torrentContent":{"search":{"items":[
{"videoCodec":"x265","releaseGroup":"GROUP","englishAudio":"dub"},
{"videoCodec":"HEVC","releaseGroup":"GROUP","englishAudio":"dub"},
{"videoCodec":"AV1","releaseGroup":"GROUP","englishAudio":"dub"}]}}}}`, w.Body.String())
	schema, err := b.schema.Get()
	require.NoError(t, err)
	var names []string
	for _, value := range schema.Schema().Types["VideoCodec"].EnumValues {
		names = append(names, value.Name)
	}
	require.Contains(t, names, "x265")
	require.Contains(t, names, "HEVC")
	require.Contains(t, names, "AV1")
	require.NotNil(t, schema.Schema().Types["EnglishAudio"], "regeneration preserves the existing audio enum")
}

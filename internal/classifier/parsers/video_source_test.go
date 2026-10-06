package parsers

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseVideoContentSourceTokens(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"WEB-DL", "WEB.DL", "WEBDL"} {
		t.Run(source, func(t *testing.T) {
			attrs, err := ParseVideoContent(
				model.Torrent{Name: "Example.Movie.2024.1080p." + source + ".H264-GROUP"},
				classification.Result{ContentAttributes: classification.ContentAttributes{
					ContentType: model.NewNullContentType(model.ContentTypeMovie),
				}},
			)
			require.NoError(t, err)
			assert.Equal(t, model.NewNullVideoSource(model.VideoSourceWEBDL), attrs.VideoSource)
			assert.Equal(t, "Example Movie", attrs.BaseTitle.String)
			assert.Equal(t, model.Year(2024), attrs.Date.Year)
			assert.Equal(t, model.NewNullVideoResolution(model.VideoResolutionV1080p), attrs.VideoResolution)
			assert.Equal(t, model.NewNullVideoCodec(model.VideoCodecH264), attrs.VideoCodec)
			assert.Equal(t, "GROUP", attrs.ReleaseGroup.String)
		})
	}
}

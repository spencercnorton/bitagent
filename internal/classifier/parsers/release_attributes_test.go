package parsers

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/require"
)

func TestReleaseAttributeParsingBindsOriginalNameAndPreservesHintAuthority(t *testing.T) {
	name := "Amber.Signal.2025.2160p.BDREMUX.HEVC.DV.HDR10.TrueHD.7.1.Atmos-GROUP.mkv"
	attrs, err := ParseVideoContentWithOptions(model.Torrent{Name: name}, classification.Result{}, ParseOptions{NoiseV2: true})
	require.NoError(t, err)
	require.Equal(t, model.NewNullString("Amber Signal"), attrs.BaseTitle)
	require.NotNil(t, attrs.ReleaseAttributes)
	digest := sha256.Sum256([]byte(name))
	require.Equal(t, hex.EncodeToString(digest[:]), attrs.ReleaseAttributes.SourceNameSHA256)
	cl := classification.ContentAttributes{ContentType: model.NewNullContentType(model.ContentTypeMovie), VideoCodec: model.NewNullVideoCodec(model.VideoCodecH264)}
	cl.Merge(attrs)
	require.Equal(t, model.VideoCodecH264, cl.VideoCodec.VideoCodec, "existing hinted codec remains authoritative")
	require.NotNil(t, cl.ReleaseAttributes)
	require.Equal(t, []string{"DOLBY_VISION", "HDR10"}, cl.ReleaseAttributes.HDRFormats)
}

package model

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExplicitReleaseAttributesKeepUnknownAndDoNotInventTracks(t *testing.T) {
	attrs := InferReleaseAttributes("Amber.Signal.2025.mkv", "2160p.HEVC.DV.HDR10+.TrueHD.7.1.Atmos.PROPER.REPACK")
	require.NotNil(t, attrs)
	require.Equal(t, 1, attrs.Version)
	require.Equal(t, ReleaseAttributesParser, attrs.Parser)
	digest := sha256.Sum256([]byte("Amber.Signal.2025.mkv"))
	require.Equal(t, hex.EncodeToString(digest[:]), attrs.SourceNameSHA256)
	require.Equal(t, []string{"DOLBY_VISION", "HDR10_PLUS"}, attrs.HDRFormats)
	require.Equal(t, []string{"TRUEHD"}, attrs.AudioFormats)
	require.Equal(t, "7.1", *attrs.AudioChannels)
	require.Equal(t, []string{"ATMOS"}, attrs.AudioFeatures)
	require.Equal(t, []string{"PROPER", "REPACK"}, attrs.Revisions)
	require.Nil(t, attrs.Encoder, "HEVC alone is not an x265 encoder claim")
	require.Nil(t, InferReleaseAttributes("plain.mkv", "1080p.BluRay"))
	require.Nil(t, InferReleaseAttributes("plain.mkv", "dual.audio"), "dual does not name formats or channels")
	conflict := InferReleaseAttributes("mixed.mkv", "AAC2.0.TrueHD7.1.x264.x265")
	require.NotNil(t, conflict)
	require.Equal(t, []string{"AAC", "TRUEHD"}, conflict.AudioFormats)
	require.Nil(t, conflict.AudioChannels, "multiple channel layouts cannot become one inferred layout")
	require.Nil(t, conflict.Encoder)
}

func TestAudioAndRevisionClaimsAreTokenBounded(t *testing.T) {
	attrs := InferReleaseAttributes("source", "DDP5.1.DTS-HD.MA.5.1.DTS-X.AAC2.0.INTERNAL.READNFO")
	require.NotNil(t, attrs)
	require.Equal(t, []string{"AAC", "DTS", "EAC3"}, attrs.AudioFormats)
	require.Equal(t, []string{"DTS_X"}, attrs.AudioFeatures)
	require.Nil(t, attrs.AudioChannels)
	require.Equal(t, []string{"INTERNAL", "READNFO"}, attrs.Revisions)
	require.Nil(t, InferReleaseAttributes("source", "properly.repacked.atmosphere.dvdrip"))
	encoded := InferReleaseAttributes("source", "x265.HEVC")
	require.Equal(t, "x265", *encoded.Encoder)
}

func TestRemuxAndCamRipAreExplicitExistingFieldClaims(t *testing.T) {
	for _, input := range []string{"BDREMUX", "BdRemux", "bdremux"} {
		require.Equal(t, NewNullVideoSource(VideoSourceBluRay), InferVideoSource(input))
		require.Equal(t, NewNullVideoModifier(VideoModifierREMUX), InferVideoModifier(input))
	}
	require.Equal(t, NewNullVideoSource(VideoSourceCAM), InferVideoSource("CAMRip"))
	require.False(t, InferVideoModifier("remuxed title").Valid)
}

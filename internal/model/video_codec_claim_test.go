package model

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCodecAndIndependentGroupClaims(t *testing.T) {
	for _, tc := range []struct {
		input string
		codec VideoCodec
		group string
	}{
		{"1080p.H.264-GROUP.mkv", VideoCodecH264, "GROUP"},
		{"1080p.H_264-GROUP", VideoCodecH264, "GROUP"},
		{"1080p.H-264-GROUP", VideoCodecH264, "GROUP"},
		{"1080p.H264-GROUP", VideoCodecH264, "GROUP"},
		{"1080p.AVC-GROUP", VideoCodecH264, "GROUP"},
		{"1080p.x264.DTS-GROUP.mkv", VideoCodecX264, "GROUP"},
		{"1080p.x265.TrueHD.7.1-GROUP.mkv", VideoCodecX265, "GROUP"},
		{"2160p.HEVC-GROUP", VideoCodecHEVC, "GROUP"},
		{"2160p.H265-GROUP", VideoCodecHEVC, "GROUP"},
		{"2160p.H.265.DDP5.1-GROUP.mkv", VideoCodecHEVC, "GROUP"},
		{"2160p.H_265-GROUP", VideoCodecHEVC, "GROUP"},
		{"2160p.H-265-GROUP", VideoCodecHEVC, "GROUP"},
		{"2160p.H 265-GROUP", VideoCodecHEVC, "GROUP"},
		{"2160p.AV1.DTS-HD.MA.5.1-GROUP.mkv", VideoCodecAV1, "GROUP"},
		{"1080p.DDP5.1-GROUP.mkv", "", "GROUP"},
		{"1080p.DTS-GROUP.mkv", "", "GROUP"},
		{"1080p.WEB-DL-GROUP.mkv", "", "GROUP"},
		{"1080p.WEB-DL", "", ""},
		{"1080p.AV1-GROUP.PROPER.mkv", VideoCodecAV1, "GROUP"},
		{"Amber-Signal.mkv", "", ""},
		{"Amber-Story.2025.mkv", "", ""},
		{"1080p.x265-REPACK.mkv", VideoCodecX265, ""},
		{"1080p.x265-v2.mkv", VideoCodecX265, ""},
		{"1080p.UnknownCodec-GROUP.mkv", "", ""},
		{"[NotARecognizedFansub] Amber Signal - 12.mkv", "", ""},
		{"1080p.av1encoded.mkv", "", ""},
		{"1080p.prehevc.mkv", "", ""},
		{"1080p.H264.x265-GROUP.mkv", "", "GROUP"},
		{"1080p.H264 x265-GROUP.mkv", "", "GROUP"},
		{"1080p.x265.H.265-GROUP.mkv", VideoCodecX265, "GROUP"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			codec, group := InferVideoCodecAndReleaseGroup(tc.input)
			require.Equal(t, tc.codec != "", codec.Valid)
			require.Equal(t, tc.codec, codec.VideoCodec)
			require.Equal(t, tc.group != "", group.Valid)
			require.Equal(t, tc.group, group.String)
		})
	}
}

func TestCodecClaimsPreserveLegacyValuesAndModernRoundTrips(t *testing.T) {
	for _, expected := range []VideoCodec{VideoCodecH264, VideoCodecX264, VideoCodecX265, VideoCodecXviD, VideoCodecDivX, VideoCodecMPEG2, VideoCodecMPEG4, VideoCodecHEVC, VideoCodecAV1} {
		codec := InferVideoCodec(expected.String())
		require.Equal(t, NewNullVideoCodec(expected), codec)
		encoded, err := json.Marshal(codec)
		require.NoError(t, err)
		require.Equal(t, `"`+expected.String()+`"`, string(encoded))
		var decoded NullVideoCodec
		require.NoError(t, json.Unmarshal(encoded, &decoded))
		require.Equal(t, codec.VideoCodec, decoded.VideoCodec)
		var scanned NullVideoCodec
		require.NoError(t, scanned.Scan(expected.String()))
		require.Equal(t, codec.VideoCodec, scanned.VideoCodec)
		var gql bytes.Buffer
		codec.MarshalGQL(&gql)
		require.Equal(t, string(encoded), gql.String())
		require.NoError(t, scanned.UnmarshalGQL(expected.String()))
	}
	var unknown NullVideoCodec
	require.NoError(t, unknown.Scan(nil))
	require.False(t, unknown.Valid)
	require.False(t, InferVideoCodec("advertised codec unknown").Valid)
	require.Equal(t, VideoCodecHEVC, InferVideoCodec("HEVC").VideoCodec, "HEVC does not claim an x265 encoder")
}

func TestCodecTokenConstructionIsDeterministic(t *testing.T) {
	expected := videoCodecTokenPattern()
	for range 100 {
		require.Equal(t, expected, videoCodecTokenPattern())
	}
}

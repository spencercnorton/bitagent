package model

import (
	"regexp"
	"sort"
	"strings"
)

// VideoCodec represents a release-name codec claim. Legacy x264/x265 values
// remain available when those exact encoder labels are advertised; HEVC does
// not imply that the x265 encoder was used.
// ENUM(H264, x264, x265, XviD, DivX, MPEG2, MPEG4, HEVC, AV1)
type VideoCodec string

func (v VideoCodec) Label() string {
	return v.String()
}

var videoCodecAliases = map[string]VideoCodec{
	"avc":   VideoCodecH264,
	"h.264": VideoCodecH264,
	"h-264": VideoCodecH264,
	"h_264": VideoCodecH264,
	"h 264": VideoCodecH264,
	"h265":  VideoCodecHEVC,
	"h.265": VideoCodecHEVC,
	"h-265": VideoCodecHEVC,
	"h_265": VideoCodecHEVC,
	"h 265": VideoCodecHEVC,
}

func videoCodecTokenPattern() string {
	names := namesToLower(VideoCodecNames()...)
	for videoCodec := range videoCodecAliases {
		names = append(names, videoCodec)
	}

	sort.Slice(names, func(i, j int) bool {
		if len(names[i]) != len(names[j]) {
			return len(names[i]) > len(names[j])
		}
		return names[i] < names[j]
	})
	for i := range names {
		names[i] = regexp.QuoteMeta(names[i])
	}
	return strings.Join(names, "|")
}

var videoCodecRegex = regexp.MustCompile(`(?i)(?:^|[^\pL\pN])(` + videoCodecTokenPattern() + `)(?:$|[^\pL\pN])`)

// InferVideoCodecAndReleaseGroup retains the existing API while keeping the
// independent group claim available when no known codec token is present.
func InferVideoCodecAndReleaseGroup(input string) (NullVideoCodec, NullString) {
	return InferVideoCodec(input), InferReleaseGroup(input)
}

// InferVideoCodec returns unknown when distinct codec families are advertised.
// It preserves the first explicit legacy spelling within one codec family.
func InferVideoCodec(input string) NullVideoCodec {
	var selected NullVideoCodec
	for offset := 0; offset < len(input); {
		match := videoCodecRegex.FindStringSubmatchIndex(input[offset:])
		if match == nil {
			break
		}
		token := input[offset+match[2] : offset+match[3]]
		codec, err := ParseVideoCodec(token)
		if err != nil {
			codec = videoCodecAliases[strings.ToLower(token)]
		}
		if selected.Valid && codecFamily(selected.VideoCodec) != codecFamily(codec) {
			return NullVideoCodec{}
		}
		if !selected.Valid {
			selected = NewNullVideoCodec(codec)
		}
		// Preserve the separator after this token so adjacent codec claims
		// cannot evade the contradictory-family check.
		offset += match[3]
	}
	return selected
}

func codecFamily(codec VideoCodec) string {
	switch codec {
	case VideoCodecH264, VideoCodecX264:
		return "H264"
	case VideoCodecHEVC, VideoCodecX265:
		return "HEVC"
	case VideoCodecMPEG4, VideoCodecXviD, VideoCodecDivX:
		return "MPEG4"
	default:
		return codec.String()
	}
}

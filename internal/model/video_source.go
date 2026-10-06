package model

import (
	"regexp"
	"sort"
	"strings"

	"github.com/spencercnorton/bitagent/internal/keywords"
)

// VideoSource represents the source of a video
// ENUM(CAM, TELESYNC, TELECINE, WORKPRINT, DVD, TV, WEBDL, WEBRip, BluRay)
type VideoSource string

func (v VideoSource) Label() string {
	return v.String()
}

var videoSourceAliases = map[string]VideoSource{
	"bdremux": VideoSourceBluRay,
	"bdrip":   VideoSourceBluRay,
	"blu-ray": VideoSourceBluRay,
	"brrip":   VideoSourceBluRay,
	"dvd5":    VideoSourceDVD,
	"dvd9":    VideoSourceDVD,
	"dvdrip":  VideoSourceDVD,
	"hdtv":    VideoSourceTV,
	"iptvrip": VideoSourceTV,
	"satrip":  VideoSourceTV,
	"web":     VideoSourceWEBRip,
	"web-dl":  VideoSourceWEBDL,
	"web.dl":  VideoSourceWEBDL,
	"web-rip": VideoSourceWEBRip,
}

func createVideoSourceRegex() *regexp.Regexp {
	names := namesToLower(VideoSourceNames()...)
	for alias := range videoSourceAliases {
		names = append(names, alias)
	}
	// The keyword matcher tries alternatives in order. A generic "web" can
	// otherwise consume the separator in "web-dl" before its full token wins.
	// Longest first preserves specific sources; the tie-break makes map-derived
	// aliases produce the same regex in every process.
	sort.Slice(names, func(i, j int) bool {
		if len(names[i]) != len(names[j]) {
			return len(names[i]) > len(names[j])
		}
		return names[i] < names[j]
	})

	return keywords.MustNewRegexFromKeywords(names...)
}

var videoSourceRegex = createVideoSourceRegex()

func InferVideoSource(input string) NullVideoSource {
	if match := videoSourceRegex.FindStringSubmatch(input); match != nil {
		if parsed, parseErr := ParseVideoSource(match[1]); parseErr == nil {
			return NewNullVideoSource(parsed)
		}

		if aliased, ok := videoSourceAliases[strings.ToLower(match[1])]; ok {
			return NewNullVideoSource(aliased)
		}
	}

	return NullVideoSource{}
}

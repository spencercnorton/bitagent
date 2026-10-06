package model

import (
	"regexp"
	"strings"
)

var (
	releaseGroupExtension = regexp.MustCompile(`(?i)\.(?:mkv|mp4|avi|mov|m4v|wmv|ts|mpg|mpeg|vob|iso|torrent)$`)
	releaseGroupSuffix    = regexp.MustCompile(`-([\pL\pN][\pL\pN_]{0,63})$`)
	releaseGroupRevision  = regexp.MustCompile(`(?i)[ ._]+(?:proper|repack|internal|readnfo)$`)
	// A trailing group must immediately follow release evidence, not a
	// hyphenated title. Audio claims and sources can provide that evidence
	// independently of whether a codec was recognized.
	releaseGroupEvidence = regexp.MustCompile(`(?i)(?:^|[^\pL\pN])(?:` + videoCodecTokenPattern() +
		`|dts(?:[ ._-](?:hd|ma|es|x))*|true[ ._-]?hd|atmos|e[ ._-]?ac3|ac[ ._-]?3|ddp?|aac|flac|opus` +
		`|\d{3,4}p|web[ ._-]?(?:dl|rip)|web|blu[ ._-]?ray|b[dr]rip|bdremux|dvd(?:rip)?|hdtv|remux|proper|repack)` +
		`(?:[ ._]?\d(?:[ ._]\d)?(?:[ ._]?ch)?)?$`)
	releaseGroupVersion = regexp.MustCompile(`(?i)^(?:v\d+|\d+|\d{3,4}[pi])$`)
)

var reservedReleaseGroup = map[string]bool{
	"dl": true, "rip": true, "web": true, "webrip": true, "webdl": true,
	"bluray": true, "bdremux": true, "remux": true, "proper": true,
	"repack": true, "internal": true, "readnfo": true, "remastered": true,
	"extended": true, "unrated": true, "uncut": true, "limited": true,
	"complete": true, "multi": true, "dubbed": true, "subbed": true,
	"hdr": true, "hdr10": true, "dv": true, "atmos": true, "truehd": true,
	"dts": true, "hd": true, "ma": true, "aac": true, "ac3": true,
	"dd": true, "ddp": true, "flac": true, "opus": true,
	"avc": true, "h264": true, "h265": true, "hevc": true, "av1": true,
	"x264": true, "x265": true, "xvid": true, "divx": true,
}

// InferReleaseGroup infers only a bounded trailing scene group supported by
// adjacent technical release evidence. Leading fansub-group authority remains
// with the classifier's curated anime detector.
func InferReleaseGroup(input string) NullString {
	input = strings.TrimSpace(input)
	input = releaseGroupExtension.ReplaceAllString(input, "")
	for range 3 {
		stripped := releaseGroupRevision.ReplaceAllString(input, "")
		if stripped == input {
			break
		}
		input = stripped
	}
	match := releaseGroupSuffix.FindStringSubmatchIndex(input)
	if match == nil {
		return NullString{}
	}
	group := input[match[2]:match[3]]
	if reservedReleaseGroup[strings.ToLower(group)] || releaseGroupVersion.MatchString(group) ||
		!releaseGroupEvidence.MatchString(input[:match[0]]) {
		return NullString{}
	}
	return NewNullString(group)
}

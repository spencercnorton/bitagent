package model

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
)

const ReleaseAttributesParser = "release-attributes-v1"

// ReleaseAttributes contains advertised filename claims, never verified media
// tracks. A nil value means no supported explicit evidence was retained.
type ReleaseAttributes struct {
	Version          int      `json:"version"`
	Parser           string   `json:"parser"`
	SourceNameSHA256 string   `json:"sourceNameSha256"`
	HDRFormats       []string `json:"hdrFormats,omitempty"`
	AudioFormats     []string `json:"audioFormats,omitempty"`
	AudioChannels    *string  `json:"audioChannels,omitempty"`
	AudioFeatures    []string `json:"audioFeatures,omitempty"`
	Revisions        []string `json:"revisions,omitempty"`
	Encoder          *string  `json:"encoder,omitempty"`
}

var releaseAttributeClaims = []struct {
	pattern *regexp.Regexp
	field   string
	value   string
}{
	{claimRegex(`hdr10(?:\+|[ ._-]?plus)`), "hdr", "HDR10_PLUS"},
	{claimRegex(`hdr10`), "hdr", "HDR10"},
	{claimRegex(`hdr`), "hdr", "HDR"},
	{claimRegex(`dv|dovi|dolby[ ._-]?vision`), "hdr", "DOLBY_VISION"},
	{audioClaimRegex(`true[ ._-]?hd`), "audio", "TRUEHD"},
	{audioClaimRegex(`dts(?:[ ._-]?(?:hd|ma|hres|es|x))*`), "audio", "DTS"},
	{audioClaimRegex(`e[ ._-]?ac[ ._-]?3|ddp|dd\+`), "audio", "EAC3"},
	{audioClaimRegex(`ac[ ._-]?3|dd`), "audio", "AC3"},
	{audioClaimRegex(`aac`), "audio", "AAC"},
	{audioClaimRegex(`flac`), "audio", "FLAC"},
	{audioClaimRegex(`opus`), "audio", "OPUS"},
	{claimRegex(`atmos`), "feature", "ATMOS"},
	{claimRegex(`dts[ ._-]?x`), "feature", "DTS_X"},
	{claimRegex(`proper`), "revision", "PROPER"},
	{claimRegex(`repack`), "revision", "REPACK"},
	{claimRegex(`readnfo`), "revision", "READNFO"},
	{claimRegex(`internal`), "revision", "INTERNAL"},
}

var audioChannelClaim = regexp.MustCompile(`(?i)(?:^|[^\pL\pN])(?:true[ ._-]?hd|dts(?:[ ._-]?(?:hd|ma|hres|es|x))*|e?[ ._-]?ac[ ._-]?3|ddp?|aac|flac|opus)[ ._-]*([1-9][ ._][01]|[1-9]ch)(?:$|[^\pL\pN])`)

func claimRegex(token string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(?:^|[^\pL\pN])(?:` + token + `)(?:$|[^\pL\pN])`)
}

func audioClaimRegex(token string) *regexp.Regexp {
	return claimRegex(`(?:` + token + `)(?:[ ._-]?[1-9](?:[ ._][01])?(?:[ ._]?ch)?)?`)
}

// InferReleaseAttributes reads only a technical suffix. sourceName is retained
// as a digest so subsequent repairs can compare the exact release input.
func InferReleaseAttributes(sourceName, technicalSuffix string) *ReleaseAttributes {
	sets := map[string]map[string]bool{"hdr": {}, "audio": {}, "feature": {}, "revision": {}}
	for _, claim := range releaseAttributeClaims {
		if claim.pattern.MatchString(technicalSuffix) {
			sets[claim.field][claim.value] = true
		}
	}
	// More specific claims subsume the generic family label.
	if sets["hdr"]["HDR10_PLUS"] {
		delete(sets["hdr"], "HDR10")
	}
	if sets["hdr"]["HDR10"] || sets["hdr"]["HDR10_PLUS"] {
		delete(sets["hdr"], "HDR")
	}
	if sets["audio"]["EAC3"] {
		delete(sets["audio"], "AC3")
	}
	attrs := &ReleaseAttributes{Version: 1, Parser: ReleaseAttributesParser}
	digest := sha256.Sum256([]byte(sourceName))
	attrs.SourceNameSHA256 = hex.EncodeToString(digest[:])
	values := func(field string) []string {
		var out []string
		for value := range sets[field] {
			out = append(out, value)
		}
		sort.Strings(out)
		return out
	}
	attrs.HDRFormats, attrs.AudioFormats = values("hdr"), values("audio")
	attrs.AudioFeatures, attrs.Revisions = values("feature"), values("revision")
	var channel string
	ambiguousChannels := false
	for offset := 0; offset < len(technicalSuffix); {
		match := audioChannelClaim.FindStringSubmatchIndex(technicalSuffix[offset:])
		if match == nil {
			break
		}
		value := strings.ToLower(technicalSuffix[offset+match[2] : offset+match[3]])
		value = strings.NewReplacer(" ", ".", "_", ".").Replace(value)
		if channel != "" && channel != value {
			ambiguousChannels = true
		}
		channel = value
		offset += match[3]
	}
	if channel != "" && !ambiguousChannels {
		attrs.AudioChannels = &channel
	}
	encoders := make(map[string]bool)
	for _, encoder := range []string{"x264", "x265"} {
		if claimRegex(encoder).MatchString(technicalSuffix) {
			encoders[encoder] = true
		}
	}
	if len(encoders) == 1 {
		for encoder := range encoders {
			value := encoder
			attrs.Encoder = &value
		}
	}
	if len(attrs.HDRFormats)+len(attrs.AudioFormats)+len(attrs.AudioFeatures)+len(attrs.Revisions) == 0 &&
		attrs.AudioChannels == nil && attrs.Encoder == nil {
		return nil
	}
	return attrs
}

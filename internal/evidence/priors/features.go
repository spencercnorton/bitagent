package priors

import (
	"path"
	"regexp"
	"sort"
	"strings"
)

// FeatureKey identifies a single dimension we maintain a Beta(α, β)
// prior on. The exact set of types is fixed — adding a new key_type
// is a code change, not a config change, so the ranker and the
// resolver always agree on what is in scope.
type FeatureKey struct {
	Type  string
	Value string
}

// Feature key types. Kept as constants rather than an iota-based enum
// so the SQL key_type column reads naturally and surveys via
// `select distinct key_type from grab_outcome_priors` are
// human-readable.
const (
	FeatureSource       = "source"        // tracker / indexer name (e.g. "rarbg", "yts.am")
	FeatureReleaseGroup = "release_group" // scene/p2p group (e.g. "NTb", "PSA", "RARBG")
	FeatureQualityTag   = "quality_tag"   // WEB-DL, BluRay, REMUX, HDTV, WEBRip
	FeatureResolution   = "resolution"    // 2160p, 1080p, 720p, 480p
	FeatureCodec        = "codec"         // x264, x265, h264, h265, av1, hevc
	FeatureExtension    = "extension"     // mkv, mp4, ts, avi
)

// Features extracted from a release title + source list, ready to be
// counted against grab_outcome_priors.
//
// The slice is stable-ordered (sorted by Type, then Value) so two
// extractions of the same input produce equal slices — important for
// deterministic tests and idempotent persistence.
func Extract(releaseTitle string, sources []string, primaryExt string) []FeatureKey {
	keys := make([]FeatureKey, 0, 6)

	for _, src := range sources {
		v := normaliseSource(src)
		if v == "" {
			continue
		}
		keys = append(keys, FeatureKey{Type: FeatureSource, Value: v})
	}

	if rg := extractReleaseGroup(releaseTitle); rg != "" {
		keys = append(keys, FeatureKey{Type: FeatureReleaseGroup, Value: rg})
	}
	if qt := extractQualityTag(releaseTitle); qt != "" {
		keys = append(keys, FeatureKey{Type: FeatureQualityTag, Value: qt})
	}
	if res := extractResolution(releaseTitle); res != "" {
		keys = append(keys, FeatureKey{Type: FeatureResolution, Value: res})
	}
	if codec := extractCodec(releaseTitle); codec != "" {
		keys = append(keys, FeatureKey{Type: FeatureCodec, Value: codec})
	}
	if ext := normaliseExtension(primaryExt); ext != "" {
		keys = append(keys, FeatureKey{Type: FeatureExtension, Value: ext})
	}

	sort.SliceStable(keys, func(i, j int) bool {
		if keys[i].Type != keys[j].Type {
			return keys[i].Type < keys[j].Type
		}
		return keys[i].Value < keys[j].Value
	})
	return dedupe(keys)
}

// dedupe removes duplicate keys preserving the first occurrence.
// Two source rows under the same indexer normalise to the same value;
// counting them twice would double-weight that feature on a single
// torrent.
func dedupe(keys []FeatureKey) []FeatureKey {
	if len(keys) <= 1 {
		return keys
	}
	out := keys[:0:len(keys)]
	seen := make(map[FeatureKey]struct{}, len(keys))
	for _, k := range keys {
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}

// normaliseSource collapses a tracker URL or indexer name to a stable
// short string. Two trackers under the same operator (rarbg.to /
// rarbg.is / rarbgaccess.org) compress to one feature value.
func normaliseSource(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	// Strip URL scheme / path / port.
	s = strings.TrimPrefix(s, "udp://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "https://")
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[:i]
	}
	// Drop common subdomains so tracker.rarbg.to → rarbg.to.
	for _, prefix := range []string{"www.", "tracker.", "open.", "explodie."} {
		s = strings.TrimPrefix(s, prefix)
	}
	// Collapse to apex+TLD; we don't need finer granularity.
	parts := strings.Split(s, ".")
	if len(parts) >= 2 {
		s = strings.Join(parts[len(parts)-2:], ".")
	}
	return s
}

// normaliseExtension lower-cases and trims a file extension. The
// catalog stores extensions with no dot prefix; we accept either form.
func normaliseExtension(ext string) string {
	ext = strings.ToLower(strings.TrimSpace(ext))
	ext = strings.TrimPrefix(ext, ".")
	if ext == "" {
		return ""
	}
	if len(ext) > 8 {
		// pathologically long extensions are almost certainly junk.
		return ""
	}
	return ext
}

// extractReleaseGroup pulls the trailing `-GROUP` token from a scene
// or p2p release name. Convention: "Title.Year.Quality.Codec-GROUP".
// Conservative regex — we'd rather miss a malformed name than
// mis-classify an arbitrary trailing token as a group.
var releaseGroupRe = regexp.MustCompile(`-([A-Za-z0-9]{2,16})(?:\.[A-Za-z0-9]{1,4})?$`)

func extractReleaseGroup(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	// Strip a trailing extension first so "Foo-GRP.mkv" → "Foo-GRP".
	if ext := path.Ext(name); ext != "" && len(ext) <= 5 {
		name = strings.TrimSuffix(name, ext)
	}
	m := releaseGroupRe.FindStringSubmatch(name)
	if len(m) < 2 {
		return ""
	}
	g := strings.ToLower(m[1])
	// Reject suffixes that are obviously not release-group identifiers.
	switch g {
	case "1080p", "720p", "2160p", "480p", "x264", "x265", "h264", "h265", "av1", "hevc":
		return ""
	}
	return g
}

var (
	qualityTagRe = regexp.MustCompile(`(?i)\b(WEB-DL|WEBRip|WEB|BluRay|BDRip|BRRip|HDTV|DVDRip|REMUX|HDRip|CAM|HDCAM|TS|TC)\b`)
	resolutionRe = regexp.MustCompile(`(?i)\b(2160p|1440p|1080p|720p|480p|360p)\b`)
	codecRe      = regexp.MustCompile(`(?i)\b(x265|x264|h\.?265|h\.?264|hevc|av1|avc|xvid|divx)\b`)
)

func extractQualityTag(name string) string {
	m := qualityTagRe.FindString(name)
	if m == "" {
		return ""
	}
	return strings.ToLower(strings.ReplaceAll(m, "-", ""))
}

func extractResolution(name string) string {
	m := resolutionRe.FindString(name)
	if m == "" {
		return ""
	}
	return strings.ToLower(m)
}

func extractCodec(name string) string {
	m := codecRe.FindString(name)
	if m == "" {
		return ""
	}
	c := strings.ToLower(m)
	c = strings.ReplaceAll(c, ".", "")
	switch c {
	case "h265", "hevc":
		return "x265"
	case "h264", "avc":
		return "x264"
	}
	return c
}

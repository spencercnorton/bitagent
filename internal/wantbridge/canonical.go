package wantbridge

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Canonicalise reduces an arbitrary torrent name OR a wantlist entry
// title to the comparable Canonical shape. Both sides of the match
// run through this function so they end up directly comparable.
//
// Examples:
//
//	"The.Wire.S01E03.Lessons.1080p.BluRay.x264-MIHD"
//	  -> {Kind: KindTV, Title: "the wire", Season: 1, EpisodeSet: [3], Year: 0}
//
//	"Breaking.Bad.S05.Complete.720p.WEB-DL"
//	  -> {Kind: KindTV, Title: "breaking bad", Season: 5, EpisodeSet: nil, Year: 0}
//
//	"Inception 2010 1080p BluRay x264"
//	  -> {Kind: KindMovie, Title: "inception", Season: -1, Year: 2010}
//
//	"Pink Floyd - The Wall (1979) [FLAC]"
//	  -> {Kind: KindMusic, Title: "pink floyd", AlbumHint: "the wall", Year: 1979}
//
// Empty / too-short input returns Kind=KindUnknown — the matcher
// treats those as "no signal, leave at Tier 1."
func Canonicalise(name string) Canonical {
	if name == "" {
		return Canonical{Kind: KindUnknown, Season: -1}
	}

	lower := strings.ToLower(name)
	musicHinted := hasMusicHint(lower)

	// Detect "Artist - Album" pattern BEFORE the replacer flattens
	// hyphens. The " - " (space-dash-space) shape is distinct from
	// "compound-word" hyphens — the latter doesn't have spaces
	// around it and doesn't trigger this branch.
	var (
		albumHint     string
		artistOnlyTok []string
		hyphenSplit   bool
	)
	if hyphenIdx := strings.Index(lower, " - "); hyphenIdx > 0 {
		left := lower[:hyphenIdx]
		right := lower[hyphenIdx+3:]
		// Tokenise both sides with the same replacer used for the
		// non-hyphen path; pick album tokens up to the first cut.
		leftTokens := tokeniseWithReplacer(left)
		albumTokens := albumTokensFromRight(right)
		if len(leftTokens) > 0 && len(albumTokens) > 0 {
			artistOnlyTok = leftTokens
			albumHint = strings.Join(albumTokens, " ")
			hyphenSplit = true
		}
	}

	// Tokenise the (possibly truncated) lower string.
	var tokens []string
	if hyphenSplit {
		tokens = artistOnlyTok
	} else {
		tokens = tokeniseWithReplacer(lower)
	}
	if len(tokens) == 0 && !hyphenSplit {
		return Canonical{Kind: KindUnknown, Season: -1}
	}

	// Detect TV season/episode markers — S01E03, S01, S01E03E04, etc.
	season, episodeSet, tvCutIdx := extractSeasonEpisode(tokens)

	// Detect a 4-digit year. We search the WHOLE original token set
	// (not just artist-side) so a music release like
	// "Pink Floyd - The Wall (1979) [FLAC]" still recovers the year.
	yearSearchTokens := tokens
	if hyphenSplit {
		// Re-tokenise the full string for year detection only.
		yearSearchTokens = tokeniseWithReplacer(lower)
	}
	year, yearCutIdx := extractYear(yearSearchTokens)

	// Take the title tokens up to the FIRST cut point. Whichever
	// of (season marker, year, release-tag-token) appears earliest
	// is treated as the boundary between "title" and "release noise."
	cut := len(tokens)
	if tvCutIdx >= 0 && tvCutIdx < cut {
		cut = tvCutIdx
	}
	if !hyphenSplit && yearCutIdx >= 0 && yearCutIdx < cut {
		cut = yearCutIdx
	}
	// Also stop at the first release-tag token.
	for i, t := range tokens {
		if i < cut && isReleaseTagToken(t) {
			cut = i
			break
		}
	}

	titleTokens := tokens[:cut]
	title := strings.Join(titleTokens, " ")

	// Decide kind. Order matters — TV (most specific) > music > movie.
	// "Unknown" is reserved for inputs we can't fingerprint at all
	// (empty / all-noise / no usable title).
	//
	// CRITICAL: every non-Unknown kind requires title != "". A
	// degenerate input like "1080p.x264.WEB-DL.2024" has year=2024
	// but title="" because every token was a release tag — we MUST
	// NOT classify that as KindMovie because the matcher would then
	// try to bloom-test "mv::2024" which is nonsense.
	kind := KindUnknown
	if title != "" {
		switch {
		case season >= 0:
			kind = KindTV
		case musicHinted && albumHint != "":
			kind = KindMusic
		case year > 0:
			kind = KindMovie
		}
	} else if hyphenSplit && albumHint != "" && musicHinted {
		// Special case: "[FLAC] - The Wall" type oddities where the
		// artist side stripped to nothing but album is present.
		// Rare; mark as music with the album hint as title.
		kind = KindMusic
		title = albumHint
		albumHint = ""
	}

	return Canonical{
		Kind:       kind,
		Title:      title,
		Year:       year,
		Season:     season,
		EpisodeSet: episodeSet,
		AlbumHint:  albumHint,
	}
}

// tokeniseWithReplacer applies the standard separator-to-space
// replacer + Fields split. Pulled out so the hyphen-split path can
// re-use the same tokenisation as the main path.
func tokeniseWithReplacer(s string) []string {
	r := strings.NewReplacer(
		".", " ", "_", " ", "-", " ",
		"[", " ", "]", " ", "(", " ", ")", " ",
		"{", " ", "}", " ",
	)
	return strings.Fields(r.Replace(s))
}

// albumTokensFromRight extracts album tokens from the right-of-hyphen
// half. Stops at year / open-bracket / release-tag — same kind of
// cut logic as the main title extraction.
func albumTokensFromRight(right string) []string {
	tokens := tokeniseWithReplacer(right)
	if len(tokens) == 0 {
		return nil
	}
	cut := len(tokens)
	if y, idx := extractYear(tokens); y > 0 && idx >= 0 && idx < cut {
		cut = idx
	}
	for i, t := range tokens {
		if i < cut && isReleaseTagToken(t) {
			cut = i
			break
		}
	}
	return tokens[:cut]
}

// seasonRE matches:
//
//	"s01"        -> season 1, no episodes
//	"s01e03"     -> season 1, episode 3
//	"s01e03e04"  -> season 1, episodes 3 and 4
//	"s10e123"    -> season 10, episode 123 (allows long-running shows)
//
// Case is already lowercased by the caller.
var seasonRE = regexp.MustCompile(`^s(\d{1,2})((?:e\d{1,3})*)$`)

// extractSeasonEpisode walks the tokens left-to-right; the first
// token matching seasonRE is the cut point. Returns:
//
//	season:     -1 if no match
//	episodeSet: nil if no episodes (season pack), or sorted slice
//	cutIdx:     index of the matching token, or -1 if none
func extractSeasonEpisode(tokens []string) (season int, episodeSet []int, cutIdx int) {
	for i, t := range tokens {
		m := seasonRE.FindStringSubmatch(t)
		if m == nil {
			continue
		}
		s, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		var eps []int
		if m[2] != "" {
			// m[2] looks like "e03e04"; split on 'e'
			for _, part := range strings.Split(m[2], "e") {
				if part == "" {
					continue
				}
				if e, err := strconv.Atoi(part); err == nil {
					eps = append(eps, e)
				}
			}
		}
		return s, eps, i
	}
	return -1, nil, -1
}

// extractYear walks tokens looking for a 4-digit year (1900-2099).
// Returns (year, cutIdx) or (0, -1) if none found.
func extractYear(tokens []string) (year int, cutIdx int) {
	for i, t := range tokens {
		if len(t) != 4 {
			continue
		}
		all := true
		for _, r := range t {
			if !unicode.IsDigit(r) {
				all = false
				break
			}
		}
		if !all {
			continue
		}
		y, err := strconv.Atoi(t)
		if err != nil {
			continue
		}
		if y >= 1900 && y <= 2099 {
			return y, i
		}
	}
	return 0, -1
}

// isReleaseTagToken reports whether a token is scene-release noise.
// Conservative list — false positives would chop legitimate title
// words. Matches contentfilter.isReleaseTag style.
func isReleaseTagToken(tok string) bool {
	switch tok {
	case "1080p", "2160p", "720p", "480p", "4k", "uhd", "hdr", "dv", "imax",
		"x264", "x265", "h264", "h265", "hevc", "av1", "avc",
		"web", "webrip", "webdl", "bluray", "brrip", "bdrip", "dvdrip",
		"hdrip", "hdtv", "dvd", "remux", "amzn", "atmos", "dts", "ac3", "aac",
		"dl", "ray",
		"complete", "internal", "repack", "proper", "extended", "uncut",
		"yify", "yts", "rarbg", "ettv", "eztv", "mihd",
		"flac", "mp3":
		return true
	}
	return false
}

// hasMusicHint returns true if the name contains tokens commonly seen
// in music releases: explicit format tags, "ost", "discography", etc.
// Used as a corroborating signal alongside the album-hint heuristic
// because "Movie - Subtitle (2020)" looks structurally identical to
// "Artist - Album (2020)."
func hasMusicHint(lower string) bool {
	musicMarkers := []string{
		" flac", "[flac]", " mp3", "[mp3]", " ost", "[ost]",
		"discography", "[discography]", "lossless", "[lossless]",
		"soundtrack", "[soundtrack]", "[24bit]", "[16bit]", "[wav]",
	}
	for _, m := range musicMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// canonicalKey is the bloom-filter / hash-table key for a Canonical.
// Two canonicals with the same key represent the same wanted thing;
// the matcher considers them equivalent.
//
// For TV: "tv:title:s01"   (season-level granularity for matching;
//                            episode-set is checked separately)
// For movies: "mv:title:2010"  (year required for disambiguation)
// For music: "ms:artist:album"
// For unknown: ""             (never indexable; always Tier 1)
func canonicalKey(c Canonical) string {
	switch c.Kind {
	case KindTV:
		if c.Season < 0 {
			return "tv:" + c.Title
		}
		return "tv:" + c.Title + ":s" + zeroPad(c.Season, 2)
	case KindMovie:
		if c.Year > 0 {
			return "mv:" + c.Title + ":" + strconv.Itoa(c.Year)
		}
		return "mv:" + c.Title
	case KindMusic:
		if c.AlbumHint != "" {
			return "ms:" + c.Title + ":" + c.AlbumHint
		}
		return "ms:" + c.Title
	default:
		return ""
	}
}

// canonicalKeys returns all keys a wantlist entry should be indexed
// under so that fuzzier torrent canonicalisations still hit. For TV
// we index both the season-specific key AND the title-only key, so
// a season pack with no episode marker still matches.
func canonicalKeys(c Canonical) []string {
	primary := canonicalKey(c)
	if primary == "" {
		return nil
	}
	out := []string{primary}
	if c.Kind == KindTV && c.Season >= 0 {
		// Title-only fallback so a torrent with no season marker
		// still flags as a possible match. The matcher's
		// confidence drops accordingly (handled in Service.Match).
		out = append(out, "tv:"+c.Title)
	}
	return out
}

func zeroPad(n int, width int) string {
	s := strconv.Itoa(n)
	for len(s) < width {
		s = "0" + s
	}
	return s
}

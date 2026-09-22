package classifier

import (
	"github.com/agnivade/levenshtein"
	"github.com/mozillazg/go-unidecode"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/regex"
)

const levenshteinThreshold = 5

func levenshteinFindBestMatch[T any](target string, items []T, getCandidates func(T) []string) (t T, ok bool) {
	minDistance := levenshteinThreshold + 1
	bestMatch := -1

	for i, item := range items {
		candidates := getCandidates(item)

		distance := levenshteinFindMinDistance(target, candidates)
		if distance >= 0 && distance < minDistance {
			minDistance = distance
			bestMatch = i

			if distance == 0 {
				break
			}
		}
	}

	if bestMatch == -1 {
		return t, false
	}

	return items[bestMatch], true
}

// fuzzyFindBestMatch is the fuzzy-match-enabled variant of levenshteinFindBestMatch.
// When fuzzyMatchEnabled is false it behaves identically to levenshteinFindBestMatch.
// When true, each candidate is accepted if EITHER:
//   - length-scaled Levenshtein ≤ min(5, ceil(0.15*len(normQuery))), OR
//   - tokenSetRatio ≥ 0.90 (Jaccard over normalised word-token sets)
//
// AND in both cases the first normalised token of the query and the best
// matching candidate string must be equal (precision gate: prevents
// "The Batman" ↔ "Batman Begins", "Rocky" ↔ "Rocky IV").
//
// Japanese-origin targets (Han/kana in the raw query) get two accommodations,
// both no-ops for every other title so non-Japanese matching is byte-for-byte
// unchanged:
//   - a native-script key (nativeMatchKey) is compared directly against each
//     candidate, so a kanji/kana title matches TMDB original_name / ja /
//     alt_title:* candidates even though unidecode mangles kanji into Mandarin
//     pinyin. An exact native-key hit is a decisive (distance 0) match.
//   - the first-token precision gate is skipped, because transliterated kana
//     carries no reliable word spacing ("しんげきのきょじん" → one token
//     "shingekinokyojin"), so a first-token equality test would wrongly block
//     an otherwise-close romaji match. Precision is still enforced by the
//     Levenshtein threshold and token-set ratio.
//
// yearPenalty is an optional per-item tiebreaker: lower is better. Pass nil
// to use zero penalty for all items (e.g. when year is already baked into
// retrieval). When two items have the same best match quality, the one with
// the lower yearPenalty wins.
func fuzzyFindBestMatch[T any](
	target string,
	items []T,
	getCandidates func(T) []string,
	getYearPenalty func(T) int,
	fuzzyMatchEnabled bool,
) (t T, ok bool) {
	if !fuzzyMatchEnabled {
		return levenshteinFindBestMatch(target, items, getCandidates)
	}

	normTarget := normalizeTitleForMatch(foldTitlePunct(target))
	threshold := fuzzyLevThreshold(normTarget)
	targetFirstTok := firstToken(normTarget)

	// Japanese-origin target: enable native-script matching and drop the
	// first-token gate (see doc comment). Both derive from the raw query, so a
	// non-empty targetNative and targetIsJP are set only for Han/kana titles.
	targetIsJP := contentfilter.ContainsJapaneseScript(target)
	targetNative := ""
	if targetIsJP {
		targetNative = nativeMatchKey(target)
	}

	type candidate struct {
		idx     int
		levDist int
		tsr     float64
		penalty int
	}

	var best *candidate
	// consider keeps the best candidate: lower year penalty first, then lower
	// Levenshtein distance, then higher token-set ratio.
	consider := func(c *candidate) {
		if best == nil ||
			c.penalty < best.penalty ||
			(c.penalty == best.penalty && c.levDist < best.levDist) ||
			(c.penalty == best.penalty && c.levDist == best.levDist && c.tsr > best.tsr) {
			best = c
		}
	}

	for i, item := range items {
		candidates := getCandidates(item)
		penalty := 0
		if getYearPenalty != nil {
			penalty = getYearPenalty(item)
		}

		for _, raw := range candidates {
			// Native-script direct match (kanji↔kanji, kana↔kana). An exact
			// native-key hit is decisive; skip the romanised path for it.
			if targetNative != "" && nativeMatchKey(raw) == targetNative {
				consider(&candidate{idx: i, levDist: 0, tsr: 1, penalty: penalty})
				continue
			}

			normCand := normalizeTitleForMatch(foldTitlePunct(raw))

			// Precision gate: first token must match (skipped for JP targets,
			// whose transliteration has no reliable word spacing).
			if !targetIsJP && firstToken(normCand) != targetFirstTok {
				continue
			}

			levDist := levenshtein.ComputeDistance(normTarget, normCand)
			tsr := tokenSetRatio(normTarget, normCand)

			accepted := levDist <= threshold || tsr >= 0.90
			if !accepted {
				continue
			}

			consider(&candidate{idx: i, levDist: levDist, tsr: tsr, penalty: penalty})
		}
	}

	if best == nil {
		return t, false
	}

	return items[best.idx], true
}

func levenshteinFindMinDistance(target string, candidates []string) int {
	normTarget := levenshteinNormalizeString(target)
	triedCandidates := make(map[string]struct{}, len(candidates))
	minDistance := -1

	for _, candidate := range candidates {
		normCandidate := levenshteinNormalizeString(candidate)
		if _, ok := triedCandidates[normCandidate]; ok {
			continue
		}

		distance := levenshtein.ComputeDistance(normTarget, normCandidate)
		if minDistance == -1 || distance < minDistance {
			minDistance = distance
		}

		triedCandidates[normCandidate] = struct{}{}
	}

	return minDistance
}

func levenshteinNormalizeString(str string) string {
	return regex.NormalizeString(unidecode.Unidecode(str))
}

package llmeval

import (
	"fmt"
	"math"
)

const (
	// MatcherSpecialistAlgorithmID is the complete, promotion-visible local
	// postprocessor contract for hosted embedding matcher systems.
	MatcherSpecialistAlgorithmID = "embedding-cosine-type-year-ppb-v1"

	MatcherSpecialistScoreScalePPB int64 = 1_000_000_000
)

// MatcherSpecialistCandidateEligible applies the same type and movie-year
// safety constraints used after the production LLM reranker. The embedding
// provider still receives every frozen document in one request; this predicate
// only controls which returned vectors may win locally.
func MatcherSpecialistCandidateEligible(
	releaseName string,
	extraction MatcherExtraction,
	candidate MatcherCandidate,
) bool {
	if candidate.Type != extraction.Type {
		return false
	}
	if extraction.Type == MediaTypeTV || candidate.Year == 0 {
		return true
	}
	if extraction.Year > 0 &&
		absMatcherSpecialistInt(extraction.Year-candidate.Year) > 1 {
		return false
	}
	rawYears := matcherSpecialistReleaseYears(releaseName)
	if len(rawYears) == 0 {
		return true
	}
	for _, year := range rawYears {
		if absMatcherSpecialistInt(year-candidate.Year) <= 1 {
			return true
		}
	}
	return false
}

// MatcherSpecialistScorePPB converts a cosine similarity into a deterministic
// integer score. Cosine values outside [0,1] are clamped because the normalized
// evaluator confidence contract is bounded; NaN and infinities fail closed.
func MatcherSpecialistScorePPB(score float64) (int64, error) {
	if math.IsNaN(score) || math.IsInf(score, 0) {
		return 0, fmt.Errorf("matcher specialist score is not finite")
	}
	score = math.Max(0, math.Min(1, score))
	return int64(math.Round(score * float64(MatcherSpecialistScoreScalePPB))), nil
}

func MatcherSpecialistConfidence(scorePPB int64) (float64, error) {
	if scorePPB < 0 || scorePPB > MatcherSpecialistScoreScalePPB {
		return 0, fmt.Errorf("matcher specialist score_ppb must be in [0,1000000000]")
	}
	return float64(scorePPB) / float64(MatcherSpecialistScoreScalePPB), nil
}

func MatcherSpecialistThresholds(
	base DecisionThresholds,
	thresholdPPB int64,
) (DecisionThresholds, error) {
	confidence, err := MatcherSpecialistConfidence(thresholdPPB)
	if err != nil {
		return DecisionThresholds{}, err
	}
	base.MatcherAttachConfidence = confidence
	if err := base.Validate(); err != nil {
		return DecisionThresholds{}, err
	}
	return base, nil
}

func matcherSpecialistReleaseYears(name string) []int {
	var years []int
	for index := 0; index+4 <= len(name); index++ {
		if index > 0 && matcherSpecialistDigit(name[index-1]) {
			continue
		}
		if index+4 < len(name) && matcherSpecialistDigit(name[index+4]) {
			continue
		}
		year := 0
		valid := true
		for offset := 0; offset < 4; offset++ {
			value := name[index+offset]
			if !matcherSpecialistDigit(value) {
				valid = false
				break
			}
			year = year*10 + int(value-'0')
		}
		if valid && year >= 1900 && year <= 2099 {
			years = append(years, year)
		}
	}
	return years
}

func matcherSpecialistDigit(value byte) bool {
	return value >= '0' && value <= '9'
}

func absMatcherSpecialistInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

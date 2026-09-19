package llmmatch

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ExtractPrompt returns the exact stage-1 policy used by production.
// Evaluation tooling calls this accessor instead of carrying a prompt copy
// that can silently drift from the live matcher.
func ExtractPrompt() string {
	return extractSystemPrompt
}

// ExtractInput formats the exact stage-1 user input used by production.
func ExtractInput(name string, files []string) string {
	var sb strings.Builder
	sb.WriteString("release_name: ")
	sb.WriteString(name)
	if len(files) > 0 && len(files) <= 5 {
		sb.WriteString("\nfiles:\n")
		for _, path := range files {
			sb.WriteString("  - ")
			sb.WriteString(path)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// RerankPrompt returns the exact stage-2 policy used by production.
func RerankPrompt() string {
	return rerankSystemPrompt
}

// RerankInput formats the exact stage-2 user input used by production.
func RerankInput(name string, ext Extraction, candidates []Candidate) string {
	var sb strings.Builder
	sb.WriteString("release_name: ")
	sb.WriteString(name)
	fmt.Fprintf(&sb, "\nparsed: title=%q year=%d type=%s", ext.Title, ext.Year, ext.Type)
	sb.WriteString("\ncandidates:\n")
	for i, candidate := range candidates {
		overview := candidate.Overview
		if len(overview) > 240 {
			overview = overview[:240]
		}
		fmt.Fprintf(
			&sb,
			"  %d) tmdb_id=%d | %s (%d) | %s\n",
			i+1,
			candidate.ID,
			candidate.Title,
			candidate.Year,
			overview,
		)
	}
	return sb.String()
}

// EvaluationChatRequestJSON serializes the production matcher request with the
// same builder used by the live HTTP path. Evaluation controls use these bytes
// directly so field presence and the double-newline /no_think delimiter cannot
// drift from production.
func EvaluationChatRequestJSON(
	model string,
	system string,
	user string,
	maxTokens int,
) ([]byte, error) {
	return json.Marshal(newChatRequest(model, system, user, maxTokens))
}

// EvaluationConfiguredChatRequestJSON includes the same provider pin/privacy
// policy as production, so a route canary evaluates the bytes we will send.
func EvaluationConfiguredChatRequestJSON(cfg Config, system, user string, maxTokens int) ([]byte, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(newConfiguredChatRequest(cfg, system, user, maxTokens))
}

// EvaluationDecodeExtraction applies the exact production stage-1 wire
// decoder. Evaluation code must call this accessor rather than maintaining a
// stricter shadow decoder: fences, null integer fields, and numeric strings
// are all provider shapes the live matcher intentionally accepts, while an
// incomplete action contract still fails closed.
func EvaluationDecodeExtraction(raw []byte) (Extraction, error) {
	extraction, err := decodeExtraction(raw)
	if err != nil {
		return Extraction{}, err
	}
	if err := normalizeAndValidateExtraction(&extraction); err != nil {
		return Extraction{}, err
	}
	return extraction, nil
}

// EvaluationDecodeRerank applies the exact production stage-2 wire decoder
// and selection normalization. In particular, a model-selected ID that was
// not offered is the same clean decline as it is on the live matcher path.
func EvaluationDecodeRerank(
	raw []byte,
	candidates []Candidate,
) (int64, float64, error) {
	return decodeAndNormalizeRerank(raw, candidates)
}

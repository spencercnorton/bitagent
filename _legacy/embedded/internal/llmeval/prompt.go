package llmeval

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/junkpurge"
)

// PromptRequest is the provider-neutral request boundary. The policy and input
// are kept separate so chat and Responses transports can serialize the same
// test case without changing its semantics.
type PromptRequest struct {
	Task                Task
	SchemaName          string
	System              string
	User                string
	MaxCompletionTokens int
	Schema              map[string]any
	SpecializedRerank   *SpecializedRerankRequest
}

type SpecializedRerankRequest struct {
	Query       string
	ReleaseName string
	Extraction  MatcherExtraction
	Documents   []SpecializedRerankDocument
}

type SpecializedRerankDocument struct {
	TMDBID int64
	Type   MediaType
	Year   int
	Text   string
}

// RequestContractSHA256 binds a result to every non-secret input that can
// change the request, its interpretation, or its reported cost. In particular,
// it covers the exact rendered prompt/schema, provider route, transport
// controls, prices, and decision thresholds. A resumed run must recompute this
// digest instead of relying on the deliberately compact SystemDescriptor.
func RequestContractSHA256(
	record CorpusRecord,
	system SystemConfig,
	thresholds DecisionThresholds,
) (string, error) {
	prompt, err := BuildPrompt(record)
	if err != nil {
		return "", err
	}
	promptJSON, err := PromptSHA256Input(prompt)
	if err != nil {
		return "", fmt.Errorf("canonicalize prompt: %w", err)
	}
	payload, err := json.Marshal(struct {
		Domain     string             `json:"domain"`
		System     SystemConfig       `json:"system"`
		Thresholds DecisionThresholds `json:"thresholds"`
		Prompt     json.RawMessage    `json:"prompt"`
	}{
		Domain:     "bitagent-llmeval-request-contract-v1",
		System:     system,
		Thresholds: thresholds,
		Prompt:     promptJSON,
	})
	if err != nil {
		return "", fmt.Errorf("canonicalize request contract: %w", err)
	}
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("%x", sum), nil
}

// BuildPrompt constructs the exact production policy and input shape for one
// frozen record. Transport-only controls (JSON Schema, provider pinning,
// reasoning settings) are applied later and therefore cannot change the case.
func BuildPrompt(record CorpusRecord) (PromptRequest, error) {
	switch record.Task {
	case TaskMatcherExtract:
		if record.MatcherExtract == nil {
			return PromptRequest{}, fmt.Errorf("case %q: missing matcher_extract payload", record.CaseID)
		}
		input := record.MatcherExtract.Input
		return PromptRequest{
			Task:                record.Task,
			SchemaName:          "bitagent_matcher_extract",
			System:              llmmatch.ExtractPrompt(),
			User:                llmmatch.ExtractInput(input.ReleaseName, input.FilePaths),
			MaxCompletionTokens: 120,
			Schema:              matcherExtractSchema(),
		}, nil

	case TaskMatcherRerank:
		if record.MatcherRerank == nil {
			return PromptRequest{}, fmt.Errorf("case %q: missing matcher_rerank payload", record.CaseID)
		}
		input := record.MatcherRerank.Input
		extraction := llmmatch.Extraction{
			Title:   input.Extraction.Title,
			Year:    input.Extraction.Year,
			Type:    string(input.Extraction.Type),
			Season:  input.Extraction.Season,
			Episode: input.Extraction.Episode,
			IsAnime: input.Extraction.IsAnime,
			English: string(input.Extraction.English),
			IsPack:  input.Extraction.IsPack,
			IsAdult: input.Extraction.IsAdult,
			OK:      input.Extraction.Title != "",
		}
		candidates := make([]llmmatch.Candidate, 0, len(input.Candidates))
		for _, candidate := range input.Candidates {
			candidates = append(candidates, llmmatch.Candidate{
				ID:       candidate.TMDBID,
				Title:    candidate.Title,
				Year:     candidate.Year,
				Overview: candidate.Overview,
				// AltTitles intentionally stay out of the serialized prompt; they
				// are copied only for the post-response identity gate.
				AltTitles: append([]string(nil), candidate.AltTitles...),
			})
		}
		request := PromptRequest{
			Task:                record.Task,
			SchemaName:          "bitagent_matcher_rerank",
			System:              llmmatch.RerankPrompt(),
			User:                llmmatch.RerankInput(input.ReleaseName, extraction, candidates),
			MaxCompletionTokens: 60,
			Schema:              matcherRerankSchema(),
			SpecializedRerank: &SpecializedRerankRequest{
				Query: fmt.Sprintf(
					"Instruct: Identify the single TMDB entry described by this torrent release. "+
						"A wrong match is worse than no match.\nQuery: release=%q; parsed_title=%q; year=%d; type=%s",
					input.ReleaseName,
					input.Extraction.Title,
					input.Extraction.Year,
					input.Extraction.Type,
				),
				ReleaseName: input.ReleaseName,
				Extraction:  input.Extraction,
			},
		}
		for _, candidate := range input.Candidates {
			request.SpecializedRerank.Documents = append(
				request.SpecializedRerank.Documents,
				SpecializedRerankDocument{
					TMDBID: candidate.TMDBID,
					Type:   candidate.Type,
					Year:   candidate.Year,
					Text: fmt.Sprintf(
						"Document: TMDB %s %q; original_title=%q; year=%d; overview=%q",
						candidate.Type,
						candidate.Title,
						candidate.OriginalTitle,
						candidate.Year,
						candidate.Overview,
					),
				},
			)
		}
		return request, nil

	case TaskContentFilter:
		if record.ContentFilter == nil {
			return PromptRequest{}, fmt.Errorf("case %q: missing contentfilter payload", record.CaseID)
		}
		return PromptRequest{
			Task:                record.Task,
			SchemaName:          "bitagent_contentfilter",
			System:              contentfilter.EvaluationPrompt(),
			User:                record.ContentFilter.Input.Title,
			MaxCompletionTokens: 120,
			Schema:              contentFilterSchema(),
		}, nil

	case TaskJunkPurge:
		if record.JunkPurge == nil {
			return PromptRequest{}, fmt.Errorf("case %q: missing junkpurge payload", record.CaseID)
		}
		return PromptRequest{
			Task:                record.Task,
			SchemaName:          "bitagent_junkpurge",
			System:              junkpurge.EvaluationPrompt(),
			User:                record.JunkPurge.Input.TorrentName,
			MaxCompletionTokens: 120,
			Schema:              junkPurgeSchema(),
		}, nil
	default:
		return PromptRequest{}, fmt.Errorf("case %q: unsupported task %q", record.CaseID, record.Task)
	}
}

// PromptSHA256Input returns the canonical bytes whose digest identifies a
// prompt contract. It includes the schema because changing an enum or required
// field can materially change model behavior even when the prose is unchanged.
func PromptSHA256Input(request PromptRequest) ([]byte, error) {
	return json.Marshal(struct {
		Task                Task                      `json:"task"`
		SchemaName          string                    `json:"schema_name"`
		System              string                    `json:"system"`
		User                string                    `json:"user"`
		MaxCompletionTokens int                       `json:"max_completion_tokens"`
		Schema              map[string]any            `json:"schema"`
		SpecializedRerank   *SpecializedRerankRequest `json:"specialized_rerank,omitempty"`
	}{
		Task:                request.Task,
		SchemaName:          request.SchemaName,
		System:              request.System,
		User:                request.User,
		MaxCompletionTokens: request.MaxCompletionTokens,
		Schema:              request.Schema,
		SpecializedRerank:   request.SpecializedRerank,
	})
}

func matcherExtractSchema() map[string]any {
	return objectSchema(
		map[string]any{
			"title":    map[string]any{"type": "string"},
			"year":     map[string]any{"type": "integer", "minimum": 0},
			"type":     map[string]any{"type": "string", "enum": []string{"movie", "tv"}},
			"season":   map[string]any{"type": "integer", "minimum": 0},
			"episode":  map[string]any{"type": "integer", "minimum": 0},
			"is_anime": map[string]any{"type": "boolean"},
			"english":  map[string]any{"type": "string", "enum": []string{"dub", "sub", "none", "unknown"}},
			"is_pack":  map[string]any{"type": "boolean"},
			"is_adult": map[string]any{"type": "boolean"},
		},
		"title", "year", "type", "season", "episode", "is_anime", "english", "is_pack", "is_adult",
	)
}

func matcherRerankSchema() map[string]any {
	return objectSchema(
		map[string]any{
			"tmdb_id":    map[string]any{"type": "integer", "minimum": 0},
			"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
		},
		"tmdb_id", "confidence",
	)
}

func contentFilterSchema() map[string]any {
	return objectSchema(
		map[string]any{
			"is_english": map[string]any{"type": "boolean"},
			"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
			"reason":     map[string]any{"type": "string"},
		},
		"is_english", "confidence", "reason",
	)
}

func junkPurgeSchema() map[string]any {
	return objectSchema(
		map[string]any{
			"verdict": map[string]any{
				"type": "string",
				"enum": []string{"junk", "real_mangled", "real_absent", "unsure"},
			},
			"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
		},
		"verdict", "confidence",
	)
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	}
}

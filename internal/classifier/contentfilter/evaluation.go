package contentfilter

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

var (
	evaluationLeadingGroupRE = regexp.MustCompile(
		`^\s*(?:\[[^\]\r\n]{1,64}\]|【[^】\r\n]{1,64}】)\s*`,
	)
	evaluationSitePrefixRE = regexp.MustCompile(
		`(?i)^\s*(?:https?://|www\.)?[a-z0-9.-]+\.(?:com|net|org|tv|to|me|cc)\s*(?:[-–—:|]\s*)+`,
	)
	evaluationSceneSuffixRE = regexp.MustCompile(
		`-[A-Za-z0-9][A-Za-z0-9._]{1,31}\s*$`,
	)
	evaluationEpisodeTokenRE = regexp.MustCompile(
		`(?i)^(?:s\d{1,3}(?:e\d{1,4})?|e\d{1,4}|season\d{1,3}|episode\d{1,4})$`,
	)
	evaluationReleaseMarkerRE = regexp.MustCompile(
		`(?i)(?:^|[._\s-])(?:2160p|1080p|720p|480p|4k|uhd|web-?dl|web-?rip|bluray|blu-ray|bdrip|hdtv|remux|x26[45]|h\.?26[45]|hevc|av1)(?:$|[._\s-])`,
	)
)

// EvaluationPrompt returns the exact policy used by the production residual
// language classifier. Evaluation tooling uses this accessor so prompt drift
// is visible in the corpus/run identity rather than silently changing results.
func EvaluationPrompt() string {
	return llmInstructions
}

// EvaluationResponsesRequestJSON serializes the exact production Responses
// request. The evaluator consumes these bytes directly for its fidelity
// control instead of maintaining a second request builder.
func EvaluationResponsesRequestJSON(model, title string) ([]byte, error) {
	return json.Marshal(responsesRequestBody(model, title))
}

// EvaluationChatRequestJSON serializes the legacy direct/local Chat shape.
// Production OpenRouter requests use EvaluationCapture, which adds the exact
// configured provider policy and output bound to the captured model input.
func EvaluationChatRequestJSON(model, title string) ([]byte, error) {
	return json.Marshal(chatRequestBody(model, title))
}

// EvaluationParseVerdict applies the exact production verdict decoder.
func EvaluationParseVerdict(text string) (LLMVerdict, error) {
	return parseLLMVerdict(text)
}

// EvaluationGroupKey returns a deliberately conservative release-family key
// for group-preserving evaluation splits. Unlike ordinary title matching, it
// stops at the first year, technical release tag, or episode marker so codec,
// quality, season, and scene-group variants cannot inflate statistical power.
// Over-grouping unrelated titles is acceptable here: it reduces power rather
// than creating false independence.
func EvaluationGroupKey(title string) string {
	cleaned := strings.TrimSpace(title)
	for {
		next := evaluationLeadingGroupRE.ReplaceAllString(cleaned, "")
		if next == cleaned {
			break
		}
		cleaned = strings.TrimSpace(next)
	}
	cleaned = evaluationSitePrefixRE.ReplaceAllString(cleaned, "")
	if evaluationReleaseMarkerRE.MatchString(cleaned) ||
		strings.Count(cleaned, ".") >= 2 ||
		strings.Count(cleaned, "_") >= 2 {
		cleaned = evaluationSceneSuffixRE.ReplaceAllString(cleaned, "")
	}

	normalizedSeparators := strings.NewReplacer(
		".", " ",
		"_", " ",
		"-", " ",
		"[", " ",
		"]", " ",
		"(", " ",
		")", " ",
	).Replace(strings.ToLower(cleaned))
	var identityTokens []string
	for _, token := range strings.Fields(normalizedSeparators) {
		if isReleaseTag(token) || evaluationEpisodeTokenRE.MatchString(token) {
			break
		}
		identityTokens = append(identityTokens, token)
		// Long subtitles and edition labels can vary independently of the
		// underlying media identity. A conservative prefix keeps those cases
		// together; a very large source pool supplies the lost power.
		if len(identityTokens) == 6 {
			break
		}
	}
	var keyTokens []string
	for _, token := range identityTokens {
		var cleanedToken strings.Builder
		for _, value := range token {
			if unicode.IsLetter(value) || unicode.IsDigit(value) {
				cleanedToken.WriteRune(value)
			}
		}
		if cleanedToken.Len() > 0 {
			keyTokens = append(keyTokens, cleanedToken.String())
		}
	}
	key := strings.Join(keyTokens, " ")
	if key == "" {
		return "unresolved-release-family"
	}
	return key
}

// EvaluationCaptureContract is an exact, secret-free snapshot of the
// production model-visible request shape for one eligible title.
type EvaluationCaptureContract struct {
	Model          string
	PromptVersion  string
	APIStyle       string
	Endpoint       string
	SystemPrompt   string
	ContractID     string
	ModelInputJSON json.RawMessage
	TaskInputJSON  json.RawMessage
}

// CaptureLLMEligible is the prospective production hook gate. Unlike
// EvaluationLLMEligible (which supports read-only diagnostics without a live
// client), this requires the exact deployed LLM client to be configured.
func (f *Filter) CaptureLLMEligible(in Input) bool {
	if f == nil || !f.cfg.Enabled {
		return false
	}
	reason, _ := f.evaluate(in)
	return reason == ReasonNone && f.shouldConsultLLM(in)
}

func (f *Filter) EvaluationCapture(
	in Input,
) (EvaluationCaptureContract, error) {
	model := f.cfg.LLMModel
	if model == "" {
		model = "gpt-5.4-nano"
	}
	promptVersion := f.cfg.LLMPromptVersion
	if promptVersion == "" {
		promptVersion = "v1"
	}
	style := resolveAPIStyle(f.cfg.LLMBaseURL, f.cfg.LLMApiStyle)
	baseURL := strings.TrimRight(f.cfg.LLMBaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	var body map[string]any
	systemPrompt := llmInstructions
	endpoint := baseURL + "/chat/completions"
	switch style {
	case apiStyleResponses:
		body = responsesRequestBodyBounded(model, in.Title, f.cfg.LLMMaxOutputTokens)
		endpoint = baseURL + "/responses"
	case apiStyleOllama:
		body = ollamaRequestBodyBounded(model, in.Title, f.cfg.LLMMaxOutputTokens)
		endpoint = strings.TrimSuffix(baseURL, "/v1") + "/api/chat"
	default:
		body = chatRequestBodyWithRouting(
			model, in.Title, f.cfg.LLMOpenrouterProvider, f.cfg.LLMMaxOutputTokens,
			f.cfg.LLMOpenaiDataSharing,
		)
		if !f.cfg.LLMOpenaiDataSharing {
			systemPrompt += "\n/no_think"
		}
	}
	modelInput, err := json.Marshal(body)
	if err != nil {
		return EvaluationCaptureContract{}, err
	}
	if f.cfg.LLMMaxRequestBytes <= 0 || len(modelInput) > f.cfg.LLMMaxRequestBytes {
		return EvaluationCaptureContract{}, fmt.Errorf("contentfilter model request exceeds configured byte limit")
	}
	allExtensions := make([]string, len(in.AllExtensions))
	copy(allExtensions, in.AllExtensions)
	languages := make([]string, len(in.Languages))
	copy(languages, in.Languages)
	sort.Strings(allExtensions)
	sort.Strings(languages)
	taskInputObject := map[string]any{
		"title":          in.Title,
		"min_confidence": f.cfg.LLMMinConfidenceForDrop,
		"live":           f.cfg.Enforce,
		"eligibility_input": map[string]any{
			"private":           in.Private,
			"primary_extension": in.PrimaryExtension,
			"all_extensions":    allExtensions,
			"content_type":      in.ContentType,
			"languages":         languages,
		},
	}
	if f.cfg.LLMOpenrouterProvider != "" {
		taskInputObject["openrouter_provider"] = f.cfg.LLMOpenrouterProvider
	}
	if f.cfg.LLMOpenaiDataSharing {
		taskInputObject["openai_data_sharing"] = true
	}
	taskInput, err := json.Marshal(taskInputObject)
	if err != nil {
		return EvaluationCaptureContract{}, err
	}
	return EvaluationCaptureContract{
		Model:         model,
		PromptVersion: promptVersion,
		APIStyle:      style,
		Endpoint:      endpoint,
		SystemPrompt:  systemPrompt,
		ContractID: contentFilterContractID(
			style, f.cfg.LLMOpenrouterProvider, f.cfg.LLMOpenaiDataSharing,
		),
		ModelInputJSON: modelInput,
		TaskInputJSON:  taskInput,
	}, nil
}

func contentFilterContractID(style, provider string, openAIDataSharing ...bool) string {
	if style == apiStyleChat && len(openAIDataSharing) > 0 && openAIDataSharing[0] {
		return "contentfilter-chat-model-input-v3-openai-data-sharing"
	}
	if style == apiStyleChat && provider != "" {
		return "contentfilter-chat-model-input-v2-openrouter"
	}
	return "contentfilter-" + style + "-model-input-v1"
}

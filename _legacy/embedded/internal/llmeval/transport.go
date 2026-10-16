package llmeval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
)

const maxProviderResponseBytes = 1 << 20

type providerResponseMode uint8

const (
	providerResponseStandard providerResponseMode = iota
	providerResponseMatcherProduction
)

type RouteProof string

const (
	RouteProofUnverified          RouteProof = ""
	RouteProofDirectResponseModel RouteProof = "direct_response_model"
	RouteProofRouterMetadata      RouteProof = "router_metadata"
	RouteProofGenerationMetadata  RouteProof = "generation_metadata"
)

type Completion struct {
	Text              []byte
	Usage             Usage
	MatcherSpecialist *MatcherSpecialistResultAudit
	RequestTiming     *RequestTiming
	ReturnedModel     string
	ReturnedProvider  string
	GenerationID      string
	RouteProof        RouteProof
	UsageCostReported bool
}

// CallError exposes only a bounded machine code. Provider bodies are not
// retained because they may echo torrent names, prompts, or credentials.
type CallError struct {
	Code string
}

func (e *CallError) Error() string {
	return e.Code
}

type HostedClient struct {
	HTTP                            *http.Client
	generationMetadataRetryOverride *generationMetadataRetryPolicy
}

type generationMetadataRetryPolicy struct {
	Delays  []time.Duration
	Timeout time.Duration
}

func defaultGenerationMetadataRetryPolicy() generationMetadataRetryPolicy {
	return generationMetadataRetryPolicy{
		// Attempts occur at approximately 0, 200, 600, 1,400, and
		// 3,000 milliseconds. This is one more metadata-only window than the
		// original policy and remains inside a separate four-second audit budget.
		Delays: []time.Duration{
			200 * time.Millisecond,
			400 * time.Millisecond,
			800 * time.Millisecond,
			1600 * time.Millisecond,
		},
		Timeout: time.Duration(OpenRouterGenerationAuditDeadlineMS) * time.Millisecond,
	}
}

func NewHostedClient(timeout time.Duration) *HostedClient {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &HostedClient{HTTP: &http.Client{Timeout: timeout}}
}

func (c *HostedClient) Complete(
	ctx context.Context,
	system SystemConfig,
	apiKey string,
	prompt PromptRequest,
) (Completion, error) {
	if !system.SupportsTask(prompt.Task) {
		return Completion{}, &CallError{Code: "unsupported_task"}
	}
	primaryStarted := time.Now()
	primaryCtx := ctx
	cancelPrimary := func() {}
	if system.RequestTimeoutMS > 0 {
		primaryCtx, cancelPrimary = context.WithTimeout(
			ctx,
			time.Duration(system.RequestTimeoutMS)*time.Millisecond,
		)
	}
	var completion Completion
	var callErr error
	switch system.APIKind {
	case APIKindChat:
		completion, callErr = c.completeChat(primaryCtx, system, apiKey, prompt)
	case APIKindResponses:
		completion, callErr = c.completeResponses(primaryCtx, system, apiKey, prompt)
	case APIKindEmbedding:
		completion, callErr = c.completeEmbedding(primaryCtx, system, apiKey, prompt)
	case APIKindRerank:
		completion, callErr = c.completeRerank(primaryCtx, system, apiKey, prompt)
	default:
		cancelPrimary()
		return Completion{}, &CallError{Code: "unsupported_api_kind"}
	}
	primaryElapsed := elapsedMillisecondsCeil(time.Since(primaryStarted))
	cancelPrimary()
	completion.RequestTiming = &RequestTiming{
		ElapsedMS:      primaryElapsed,
		DeadlineMS:     system.RequestTimeoutMS,
		TotalElapsedMS: primaryElapsed,
	}
	if system.Provider == "openrouter" &&
		(completion.RouteProof != RouteProofRouterMetadata ||
			!completion.UsageCostReported) {
		if strings.TrimSpace(completion.GenerationID) == "" {
			if callErr == nil {
				callErr = &CallError{Code: "route_unverifiable"}
			}
		} else {
			auditStarted := time.Now()
			var routeErr error
			var attempts int
			completion, attempts, routeErr = c.reconcileOpenRouterGeneration(
				ctx,
				system,
				apiKey,
				completion,
			)
			auditElapsed := elapsedMillisecondsCeil(time.Since(auditStarted))
			auditErrorCode := callErrorCode(routeErr)
			completion.RequestTiming.RouteAudit = &RouteAuditTiming{
				ElapsedMS:  auditElapsed,
				DeadlineMS: c.metadataRetryPolicy().Timeout.Milliseconds(),
				Attempts:   attempts,
				Succeeded:  routeErr == nil,
				ErrorCode:  auditErrorCode,
			}
			completion.RequestTiming.TotalElapsedMS = elapsedMillisecondsCeil(
				time.Since(primaryStarted),
			)
			if isRecoverableRouteAuditError(routeErr) {
				completion.Usage.AccountingComplete = false
				completion.UsageCostReported = false
			}
			// Preserve the primary provider/envelope error when one already
			// occurred, but still attempt reconciliation so any billable usage is
			// exact. Delayed accounting alone does not discard an otherwise valid
			// action with authenticated inline route proof; integrity failures and
			// caller cancellation/deadlines still fail closed.
			if callErr == nil && routeErr != nil {
				switch {
				case completionHasVerifiedInlineRoute(completion) &&
					isRecoverableRouteAuditError(routeErr):
					// Effectiveness evidence remains valid; accounting remains
					// explicitly incomplete and promotion-ineligible.
				case isRecoverableRouteAuditError(routeErr):
					callErr = &CallError{Code: "route_unverifiable"}
				default:
					callErr = routeErr
				}
			}
		}
	}
	completion.Usage = withExplicitUnavailableCostSource(completion.Usage)
	return completion, callErr
}

func (c *HostedClient) completeChat(
	ctx context.Context,
	system SystemConfig,
	apiKey string,
	prompt PromptRequest,
) (Completion, error) {
	endpoint, body, err := GenerativeRequestBody(system, prompt)
	if err != nil {
		return Completion{}, err
	}
	productionMatcher := system.UsesDeployedWireContract() &&
		(prompt.Task == TaskMatcherExtract || prompt.Task == TaskMatcherRerank)
	responseMode := providerResponseStandard
	if productionMatcher {
		responseMode = providerResponseMatcherProduction
	}

	raw, responseMeta, err := c.doJSON(
		ctx,
		strings.TrimRight(system.BaseURL, "/")+
			strings.TrimPrefix(endpoint, "/v1"),
		apiKey,
		body,
		system.Provider == "openrouter",
		responseMode,
	)
	if err != nil {
		return completionFromResponseMetadata(responseMeta), err
	}
	if productionMatcher {
		return matcherProductionCompletion(raw, responseMeta, system), nil
	}

	var response struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
				Refusal string `json:"refusal"`
			} `json:"message"`
		} `json:"choices"`
		Usage              chatUsage          `json:"usage"`
		OpenRouterMetadata openRouterMetadata `json:"openrouter_metadata"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return completionFromResponseMetadata(responseMeta),
			&CallError{Code: "decode_envelope"}
	}
	usage, accountingComplete := decodeCompleteChatUsage(
		jsonObjectField(raw, "usage"),
	)
	requestID := responseMeta.RequestID
	if responseID := validAuxiliaryIdentifier(response.ID); responseID != "" {
		requestID = responseID
	}
	normalizedUsage := usage.normalize(system, requestID)
	normalizedUsage.AccountingComplete = accountingComplete
	normalizedUsage = sanitizeNormalizedUsage(normalizedUsage, requestID)
	completion := Completion{
		Usage:             normalizedUsage,
		GenerationID:      responseMeta.GenerationID,
		UsageCostReported: accountingComplete && usage.hasUsableCost(system),
	}
	if system.Provider == "openrouter" {
		if completion.GenerationID == "" {
			// Successful OpenRouter chat response IDs are generation IDs and
			// remain available on cache replays where router metadata is
			// intentionally stripped.
			completion.GenerationID = validAuxiliaryIdentifier(response.ID)
		}
		model, provider, ok := response.OpenRouterMetadata.selectedRoute()
		if ok {
			completion.ReturnedModel = model
			completion.ReturnedProvider = provider
			completion.RouteProof = RouteProofRouterMetadata
		} else {
			// Successful cache replays intentionally omit router metadata.
			// Leave both fields empty so the promotion-grade route verifier
			// fails closed instead of trusting undocumented envelope fields.
			completion.ReturnedModel = ""
			completion.ReturnedProvider = ""
		}
	} else {
		completion.ReturnedModel = strings.TrimSpace(response.Model)
		if completion.ReturnedModel != "" {
			completion.RouteProof = RouteProofDirectResponseModel
		}
	}
	if len(response.Choices) == 0 {
		return completion, &CallError{Code: "empty_choices"}
	}
	message := response.Choices[0].Message
	if message.Refusal != "" {
		return completion, &CallError{Code: "refusal"}
	}
	if strings.TrimSpace(message.Content) == "" {
		return completion, &CallError{Code: "empty_content"}
	}
	completion.Text = []byte(message.Content)
	return completion, nil
}

// matcherProductionCompletion keeps the action-bearing content on the exact
// live matcher path. Route and usage fields are evaluator audit metadata, so a
// malformed irrelevant field must not turn live-valid content into a schema
// failure. Unusable route metadata remains unverified and is rejected later by
// the run's independent route-attestation gate.
func matcherProductionCompletion(
	raw []byte,
	metadata providerResponseMetadata,
	system SystemConfig,
) Completion {
	var auxiliary map[string]json.RawMessage
	if err := json.Unmarshal(raw, &auxiliary); err != nil {
		// The shared live decoder already proved the action envelope, so an
		// auxiliary audit decode failure may only remove route/usage evidence.
		auxiliary = nil
	}

	requestID := validAuxiliaryIdentifier(metadata.RequestID)
	if id := validAuxiliaryJSONIdentifier(auxiliary["id"]); id != "" {
		requestID = id
	}
	usage, accountingComplete := decodeCompleteChatUsage(auxiliary["usage"])
	normalizedUsage := usage.normalize(system, requestID)
	normalizedUsage.AccountingComplete = accountingComplete
	if err := normalizedUsage.Validate(); err != nil {
		normalizedUsage = Usage{
			RequestID:  requestID,
			CostSource: CostSourceUnavailable,
		}
	}
	completion := Completion{
		Text:              metadata.MatcherContent,
		Usage:             normalizedUsage,
		GenerationID:      metadata.GenerationID,
		UsageCostReported: accountingComplete && usage.hasUsableCost(system),
	}
	if system.Provider == "openrouter" {
		if completion.GenerationID == "" {
			// OpenRouter Chat response IDs are generation IDs. Retaining the
			// body ID lets an otherwise live-identical matcher response fall
			// back to authenticated generation reconciliation when inline
			// evaluator metadata or cost is absent.
			completion.GenerationID = validAuxiliaryJSONIdentifier(auxiliary["id"])
		}
		var routeMetadata openRouterMetadata
		if rawMetadata := auxiliary["openrouter_metadata"]; len(rawMetadata) != 0 &&
			json.Unmarshal(rawMetadata, &routeMetadata) == nil {
			model, provider, ok := routeMetadata.selectedRoute()
			if ok {
				completion.ReturnedModel = model
				completion.ReturnedProvider = provider
				completion.RouteProof = RouteProofRouterMetadata
			}
		}
		return completion
	}
	if model := validAuxiliaryJSONIdentifier(auxiliary["model"]); model != "" {
		completion.ReturnedModel = model
		completion.RouteProof = RouteProofDirectResponseModel
	}
	return completion
}

func validAuxiliaryJSONIdentifier(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return validAuxiliaryIdentifier(value)
}

func validAuxiliaryIdentifier(value string) string {
	value = strings.TrimSpace(value)
	if err := validateIdentifier("provider response identifier", value); err != nil {
		return ""
	}
	return value
}

func decodeCompleteChatUsage(raw json.RawMessage) (chatUsage, bool) {
	if len(raw) == 0 {
		return chatUsage{}, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return chatUsage{}, false
	}
	for _, name := range []string{"prompt_tokens", "completion_tokens"} {
		var value int64
		if len(fields[name]) == 0 || json.Unmarshal(fields[name], &value) != nil ||
			value <= 0 {
			return chatUsage{}, false
		}
	}
	var usage chatUsage
	if err := json.Unmarshal(raw, &usage); err != nil ||
		usage.PromptTokens < 0 || usage.CompletionTokens < 0 ||
		usage.PromptDetails.CachedTokens < 0 ||
		usage.PromptDetails.CacheWriteTokens < 0 ||
		usage.CompletionDetails.ReasoningTokens < 0 ||
		usage.PromptDetails.CachedTokens > usage.PromptTokens ||
		usage.PromptDetails.CacheWriteTokens >
			usage.PromptTokens-usage.PromptDetails.CachedTokens ||
		usage.CompletionDetails.ReasoningTokens > usage.CompletionTokens {
		return chatUsage{}, false
	}
	return usage, true
}

func jsonObjectField(raw []byte, name string) json.RawMessage {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	return fields[name]
}

func usageObjectHasPositiveFields(raw json.RawMessage, names ...string) bool {
	if len(raw) == 0 {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return false
	}
	for _, name := range names {
		var value int64
		if len(fields[name]) == 0 || json.Unmarshal(fields[name], &value) != nil ||
			value <= 0 {
			return false
		}
	}
	return true
}

func usageObjectHasAnyPositiveField(raw json.RawMessage, names ...string) bool {
	if len(raw) == 0 {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return false
	}
	for _, name := range names {
		var value int64
		if len(fields[name]) != 0 && json.Unmarshal(fields[name], &value) == nil &&
			value > 0 {
			return true
		}
	}
	return false
}

func sanitizeNormalizedUsage(usage Usage, requestID string) Usage {
	if err := usage.Validate(); err == nil {
		return usage
	}
	// Invalid token relationships make accounting incomplete, but do not erase
	// a bounded charge that may have come from authenticated provider billing
	// metadata. The cost fuse can conservatively retain it while promotion
	// remains blocked by AccountingComplete=false.
	sanitized := Usage{
		RequestID:  validAuxiliaryIdentifier(requestID),
		CostSource: usage.CostSource,
	}
	if sanitized.CostSource != CostSourceProviderReported &&
		sanitized.CostSource != CostSourceManifestEstimate {
		sanitized.CostSource = CostSourceUnavailable
	}
	if usage.CostMicroUSD > 0 &&
		sanitized.CostSource != CostSourceUnavailable {
		sanitized.CostMicroUSD = usage.CostMicroUSD
	}
	return sanitized
}

func (c *HostedClient) completeResponses(
	ctx context.Context,
	system SystemConfig,
	apiKey string,
	prompt PromptRequest,
) (Completion, error) {
	endpoint, body, err := GenerativeRequestBody(system, prompt)
	if err != nil {
		return Completion{}, err
	}

	raw, responseMeta, err := c.doJSON(
		ctx,
		strings.TrimRight(system.BaseURL, "/")+
			strings.TrimPrefix(endpoint, "/v1"),
		apiKey,
		body,
		false,
		providerResponseStandard,
	)
	if err != nil {
		return completionFromResponseMetadata(responseMeta), err
	}
	var response struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		OutputText string `json:"output_text"`
		Output     []struct {
			Type    string `json:"type"`
			Content []struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Refusal string `json:"refusal"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			InputDetails struct {
				CachedTokens     int64 `json:"cached_tokens"`
				CacheWriteTokens int64 `json:"cache_write_tokens"`
			} `json:"input_tokens_details"`
			OutputDetails struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return completionFromResponseMetadata(responseMeta),
			&CallError{Code: "decode_envelope"}
	}
	requestID := responseMeta.RequestID
	if responseID := validAuxiliaryIdentifier(response.ID); responseID != "" {
		requestID = responseID
	}
	usage := Usage{
		RequestID:         requestID,
		InputTokens:       response.Usage.InputTokens,
		CachedInputTokens: response.Usage.InputDetails.CachedTokens,
		CacheWriteTokens:  response.Usage.InputDetails.CacheWriteTokens,
		OutputTokens:      response.Usage.OutputTokens,
		ReasoningTokens:   response.Usage.OutputDetails.ReasoningTokens,
		CostSource:        CostSourceManifestEstimate,
		AccountingComplete: usageObjectHasPositiveFields(
			jsonObjectField(raw, "usage"),
			"input_tokens",
			"output_tokens",
		),
	}
	usage.CostMicroUSD = system.EstimateCostWithCacheWriteMicroUSD(
		usage.InputTokens,
		usage.CachedInputTokens,
		usage.CacheWriteTokens,
		usage.OutputTokens,
	)
	usage = sanitizeNormalizedUsage(usage, requestID)
	completion := Completion{
		Usage:         usage,
		ReturnedModel: strings.TrimSpace(response.Model),
		GenerationID:  responseMeta.GenerationID,
		RouteProof:    RouteProofDirectResponseModel,
	}
	if completion.ReturnedModel == "" {
		completion.RouteProof = RouteProofUnverified
	}
	text := strings.TrimSpace(response.OutputText)
	for _, output := range response.Output {
		for _, content := range output.Content {
			if content.Type == "refusal" || content.Refusal != "" {
				return completion, &CallError{Code: "refusal"}
			}
			if text == "" && (content.Type == "output_text" || content.Type == "text") {
				text = strings.TrimSpace(content.Text)
			}
		}
	}
	if text == "" {
		return completion, &CallError{Code: "empty_content"}
	}
	completion.Text = []byte(text)
	return completion, nil
}

func (c *HostedClient) completeEmbedding(
	ctx context.Context,
	system SystemConfig,
	apiKey string,
	prompt PromptRequest,
) (Completion, error) {
	if system.Provider != "openrouter" {
		return Completion{}, &CallError{Code: "unsupported_provider"}
	}
	if system.PromptVersion != MatcherSpecialistAlgorithmID {
		return Completion{}, &CallError{Code: "unsupported_specialist_algorithm"}
	}
	if !system.ZDR {
		return Completion{}, &CallError{Code: "zdr_required"}
	}
	specialized := prompt.SpecializedRerank
	if specialized == nil || strings.TrimSpace(specialized.Query) == "" ||
		len(specialized.Documents) == 0 {
		return Completion{}, &CallError{Code: "missing_specialized_input"}
	}

	inputs := make([]string, 0, len(specialized.Documents)+1)
	inputs = append(inputs, specialized.Query)
	for _, document := range specialized.Documents {
		inputs = append(inputs, document.Text)
	}
	body := map[string]any{
		"model":           system.Model,
		"input":           inputs,
		"encoding_format": "float",
		"provider":        openRouterProviderPolicy(system, false),
	}
	raw, responseMeta, err := c.doJSON(
		ctx,
		strings.TrimRight(system.BaseURL, "/")+"/embeddings",
		apiKey,
		body,
		false,
		providerResponseStandard,
	)
	if err != nil {
		return completionFromResponseMetadata(responseMeta), err
	}

	var response struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Data  []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			PromptTokens int64 `json:"prompt_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return completionFromResponseMetadata(responseMeta),
			&CallError{Code: "decode_envelope"}
	}
	requestID := responseMeta.RequestID
	if responseID := validAuxiliaryIdentifier(response.ID); responseID != "" {
		requestID = responseID
	}
	inputTokens := response.Usage.PromptTokens
	if inputTokens == 0 {
		inputTokens = response.Usage.TotalTokens
	}
	usage := Usage{
		RequestID:   requestID,
		InputTokens: inputTokens,
		CostSource:  CostSourceManifestEstimate,
		AccountingComplete: usageObjectHasAnyPositiveField(
			jsonObjectField(raw, "usage"),
			"prompt_tokens",
			"total_tokens",
		),
	}
	usage.CostMicroUSD = system.EstimateCostMicroUSD(inputTokens, 0, 0)
	usage = sanitizeNormalizedUsage(usage, requestID)
	completion := Completion{
		Usage:         usage,
		ReturnedModel: strings.TrimSpace(response.Model),
		GenerationID:  responseMeta.GenerationID,
	}
	vectors, err := orderedEmbeddings(response.Data, len(inputs))
	if err != nil {
		return completion, err
	}

	bestIndex := -1
	bestScorePPB := int64(-1)
	eligibleCandidates := 0
	for index := 1; index < len(vectors); index++ {
		document := specialized.Documents[index-1]
		if !MatcherSpecialistCandidateEligible(
			specialized.ReleaseName,
			specialized.Extraction,
			MatcherCandidate{
				TMDBID: document.TMDBID,
				Type:   document.Type,
				Year:   document.Year,
			},
		) {
			continue
		}
		eligibleCandidates++
		score, err := cosineSimilarity(vectors[0], vectors[index])
		if err != nil {
			return completion, err
		}
		scorePPB, err := MatcherSpecialistScorePPB(score)
		if err != nil {
			return completion, &CallError{Code: "invalid_embeddings"}
		}
		// Strict comparison preserves the first frozen candidate on PPB ties.
		if scorePPB > bestScorePPB {
			bestScorePPB = scorePPB
			bestIndex = index - 1
		}
	}
	audit := &MatcherSpecialistResultAudit{
		AlgorithmID:        MatcherSpecialistAlgorithmID,
		EligibleCandidates: eligibleCandidates,
	}
	selectedTMDBID := int64(0)
	confidence := float64(0)
	if bestIndex >= 0 {
		selectedTMDBID = specialized.Documents[bestIndex].TMDBID
		confidence, err = MatcherSpecialistConfidence(bestScorePPB)
		if err != nil {
			return completion, &CallError{Code: "encode_result"}
		}
		audit.SelectedTMDBID = selectedTMDBID
		audit.ScorePPB = bestScorePPB
	}
	normalized, err := json.Marshal(map[string]any{
		"tmdb_id":    selectedTMDBID,
		"confidence": confidence,
	})
	if err != nil {
		return completion, &CallError{Code: "encode_result"}
	}
	completion.Text = normalized
	completion.MatcherSpecialist = audit
	return completion, nil
}

func (c *HostedClient) completeRerank(
	ctx context.Context,
	system SystemConfig,
	apiKey string,
	prompt PromptRequest,
) (Completion, error) {
	if system.Provider != "openrouter" {
		return Completion{}, &CallError{Code: "unsupported_provider"}
	}
	// OpenRouter's current rerank endpoints are useful redacted/synthetic
	// references but are not in the ZDR feed. Refuse the request here so a
	// checked-in reference route cannot accidentally receive production text.
	if !system.ZDR {
		return Completion{}, &CallError{Code: "zdr_required"}
	}
	specialized := prompt.SpecializedRerank
	if specialized == nil || strings.TrimSpace(specialized.Query) == "" ||
		len(specialized.Documents) == 0 {
		return Completion{}, &CallError{Code: "missing_specialized_input"}
	}

	documents := make([]string, 0, len(specialized.Documents))
	for _, document := range specialized.Documents {
		documents = append(documents, document.Text)
	}
	body := map[string]any{
		"model":     system.Model,
		"query":     specialized.Query,
		"documents": documents,
		"top_n":     1,
		"provider":  openRouterProviderPolicy(system, false),
	}
	raw, responseMeta, err := c.doJSON(
		ctx,
		strings.TrimRight(system.BaseURL, "/")+"/rerank",
		apiKey,
		body,
		false,
		providerResponseStandard,
	)
	if err != nil {
		return completionFromResponseMetadata(responseMeta), err
	}

	var response struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		} `json:"results"`
		Usage struct {
			SearchUnits int64 `json:"search_units"`
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return completionFromResponseMetadata(responseMeta),
			&CallError{Code: "decode_envelope"}
	}
	requestID := responseMeta.RequestID
	if responseID := validAuxiliaryIdentifier(response.ID); responseID != "" {
		requestID = responseID
	}
	usage := Usage{
		RequestID:   requestID,
		InputTokens: response.Usage.TotalTokens,
		CostSource:  CostSourceManifestEstimate,
		AccountingComplete: usageObjectHasAnyPositiveField(
			jsonObjectField(raw, "usage"),
			"total_tokens",
			"search_units",
		),
	}
	usage.CostMicroUSD = system.EstimateCostMicroUSD(usage.InputTokens, 0, 0)
	usage = sanitizeNormalizedUsage(usage, requestID)
	completion := Completion{
		Usage:         usage,
		ReturnedModel: strings.TrimSpace(response.Model),
		GenerationID:  responseMeta.GenerationID,
	}
	if len(response.Results) != 1 {
		return completion, &CallError{Code: "invalid_rerank_results"}
	}
	best := response.Results[0]
	if best.Index < 0 || best.Index >= len(specialized.Documents) ||
		math.IsNaN(best.RelevanceScore) || math.IsInf(best.RelevanceScore, 0) ||
		best.RelevanceScore < 0 || best.RelevanceScore > 1 {
		return completion, &CallError{Code: "invalid_rerank_results"}
	}
	normalized, err := json.Marshal(map[string]any{
		"tmdb_id":    specialized.Documents[best.Index].TMDBID,
		"confidence": best.RelevanceScore,
	})
	if err != nil {
		return completion, &CallError{Code: "encode_result"}
	}
	completion.Text = normalized
	return completion, nil
}

func openRouterProviderPolicy(system SystemConfig, requireParameters bool) map[string]any {
	provider, quantization := splitOpenRouterProviderEndpoint(system.ProviderEndpoint)
	policy := map[string]any{
		"order":              []string{provider},
		"only":               []string{provider},
		"allow_fallbacks":    false,
		"require_parameters": requireParameters,
		"data_collection":    "deny",
		"zdr":                system.ZDR,
	}
	if quantization != "" {
		policy["quantizations"] = []string{quantization}
	}
	return policy
}

func splitOpenRouterProviderEndpoint(endpoint string) (string, string) {
	endpoint = strings.ToLower(strings.TrimSpace(endpoint))
	parts := strings.Split(endpoint, "/")
	if len(parts) < 2 {
		return endpoint, ""
	}
	last := parts[len(parts)-1]
	switch last {
	case "int4", "int8", "fp4", "fp6", "fp8", "fp16", "bf16", "fp32", "unknown":
		return strings.Join(parts[:len(parts)-1], "/"), last
	default:
		return endpoint, ""
	}
}

type openRouterMetadata struct {
	Endpoints struct {
		Available []struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
			Selected bool   `json:"selected"`
		} `json:"available"`
	} `json:"endpoints"`
}

func (metadata openRouterMetadata) selectedRoute() (string, string, bool) {
	var model, provider string
	selected := 0
	for _, endpoint := range metadata.Endpoints.Available {
		if !endpoint.Selected {
			continue
		}
		selected++
		model = strings.TrimSpace(endpoint.Model)
		provider = strings.TrimSpace(endpoint.Provider)
	}
	if selected != 1 || model == "" || provider == "" {
		return "", "", false
	}
	return model, provider, true
}

func completionTokenLimit(system SystemConfig, prompt PromptRequest) int {
	if system.MaxCompletionTokensOverride > 0 {
		return system.MaxCompletionTokensOverride
	}
	return prompt.MaxCompletionTokens
}

func orderedEmbeddings(data []struct {
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}, expected int) ([][]float64, error) {
	if len(data) != expected {
		return nil, &CallError{Code: "embedding_count_mismatch"}
	}
	ordered := make([][]float64, expected)
	for _, item := range data {
		if item.Index < 0 || item.Index >= expected || ordered[item.Index] != nil ||
			len(item.Embedding) == 0 {
			return nil, &CallError{Code: "invalid_embeddings"}
		}
		for _, value := range item.Embedding {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return nil, &CallError{Code: "invalid_embeddings"}
			}
		}
		ordered[item.Index] = item.Embedding
	}
	return ordered, nil
}

func cosineSimilarity(left, right []float64) (float64, error) {
	if len(left) == 0 || len(left) != len(right) {
		return 0, &CallError{Code: "embedding_dimension_mismatch"}
	}
	var dot, leftNorm, rightNorm float64
	for index := range left {
		dot += left[index] * right[index]
		leftNorm += left[index] * left[index]
		rightNorm += right[index] * right[index]
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0, &CallError{Code: "zero_embedding"}
	}
	return dot / (math.Sqrt(leftNorm) * math.Sqrt(rightNorm)), nil
}

type chatUsage struct {
	PromptTokens     int64    `json:"prompt_tokens"`
	CompletionTokens int64    `json:"completion_tokens"`
	Cost             *float64 `json:"cost"`
	PromptDetails    struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (u chatUsage) normalize(system SystemConfig, requestID string) Usage {
	usage := Usage{
		RequestID:         requestID,
		InputTokens:       u.PromptTokens,
		CachedInputTokens: u.PromptDetails.CachedTokens,
		CacheWriteTokens:  u.PromptDetails.CacheWriteTokens,
		OutputTokens:      u.CompletionTokens,
		ReasoningTokens:   u.CompletionDetails.ReasoningTokens,
	}
	if u.hasUsableCost(system) {
		usage.CostSource = CostSourceProviderReported
		usage.CostMicroUSD = roundPositiveMicroUSD(
			*u.Cost * system.billingMultiplier() * 1_000_000,
		)
	} else {
		usage.CostSource = CostSourceManifestEstimate
		usage.CostMicroUSD = system.EstimateCostWithCacheWriteMicroUSD(
			usage.InputTokens,
			usage.CachedInputTokens,
			usage.CacheWriteTokens,
			usage.OutputTokens,
		)
		if !u.hasNonzeroUsage() {
			usage.CostSource = CostSourceUnavailable
		}
	}
	return usage
}

func (u chatUsage) hasUsableCost(system SystemConfig) bool {
	if u.Cost == nil || *u.Cost < 0 || math.IsNaN(*u.Cost) ||
		math.IsInf(*u.Cost, 0) {
		return false
	}
	if *u.Cost == 0 && u.hasNonzeroUsage() && !system.isZeroPriced() {
		// A paid route cannot consume tokens for exactly $0. OpenRouter has
		// occasionally returned an incomplete inline usage object, so force the
		// authenticated generation lookup instead of recording a false free
		// request. Truly free routes and genuinely zero-token responses remain
		// valid zero-cost observations.
		return false
	}
	microUSD := *u.Cost * system.billingMultiplier() * 1_000_000
	return microUSD < float64(math.MaxInt64)
}

func (u chatUsage) hasNonzeroUsage() bool {
	return u.PromptTokens != 0 ||
		u.CompletionTokens != 0 ||
		u.PromptDetails.CachedTokens != 0 ||
		u.PromptDetails.CacheWriteTokens != 0 ||
		u.CompletionDetails.ReasoningTokens != 0
}

type providerResponseMetadata struct {
	RequestID      string
	GenerationID   string
	MatcherContent []byte
}

func completionFromResponseMetadata(metadata providerResponseMetadata) Completion {
	requestID := metadata.RequestID
	if metadata.GenerationID != "" {
		requestID = metadata.GenerationID
	}
	return Completion{
		Usage: Usage{
			RequestID:  requestID,
			CostSource: CostSourceUnavailable,
		},
		GenerationID: metadata.GenerationID,
	}
}

func (c *HostedClient) doJSON(
	ctx context.Context,
	endpoint string,
	apiKey string,
	body any,
	enableOpenRouterMetadata bool,
	responseMode providerResponseMode,
) ([]byte, providerResponseMetadata, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, providerResponseMetadata{}, &CallError{Code: "encode_request"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, providerResponseMetadata{}, &CallError{Code: "build_request"}
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Content-Type", "application/json")
	if responseMode != providerResponseMatcherProduction {
		request.Header.Set("HTTP-Referer", "https://github.com/spencercnorton/bitagent")
		request.Header.Set("X-OpenRouter-Title", "BitAgent LLM Evaluation")
	}
	if enableOpenRouterMetadata {
		request.Header.Set("X-OpenRouter-Metadata", "enabled")
	}

	response, err := c.HTTP.Do(request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, providerResponseMetadata{}, &CallError{Code: "timeout"}
		}
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return nil, providerResponseMetadata{}, &CallError{Code: "canceled"}
		}
		return nil, providerResponseMetadata{}, &CallError{Code: "network"}
	}
	defer response.Body.Close()
	metadata := providerResponseMetadata{
		RequestID: validAuxiliaryIdentifier(
			response.Header.Get("x-request-id"),
		),
		GenerationID: validAuxiliaryIdentifier(
			response.Header.Get("x-generation-id"),
		),
	}
	if responseMode == providerResponseMatcherProduction {
		raw, content, err := llmmatch.ReadMatcherChatResponse(
			response.StatusCode,
			response.Body,
		)
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, metadata, &CallError{Code: "timeout"}
			}
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil, metadata, &CallError{Code: "canceled"}
			}
			return nil, metadata, matcherProductionCallError(err)
		}
		metadata.MatcherContent = content
		return raw, metadata, nil
	}

	raw, err := io.ReadAll(io.LimitReader(
		response.Body,
		maxProviderResponseBytes+1,
	))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, metadata, &CallError{Code: "timeout"}
		}
		if errors.Is(err, context.Canceled) ||
			errors.Is(ctx.Err(), context.Canceled) {
			return nil, metadata, &CallError{Code: "canceled"}
		}
		return nil, metadata, &CallError{Code: "read_response"}
	}
	if len(raw) > maxProviderResponseBytes {
		return nil, metadata, &CallError{Code: "response_too_large"}
	}
	if response.StatusCode/100 != 2 {
		return nil, metadata, &CallError{
			Code: fmt.Sprintf("http_%d", response.StatusCode),
		}
	}
	return raw, metadata, nil
}

func matcherProductionCallError(err error) *CallError {
	switch {
	case errors.Is(err, llmmatch.ErrMatcherChatHTTPStatus):
		return &CallError{Code: "http_status"}
	case errors.Is(err, llmmatch.ErrMatcherChatResponseRead):
		return &CallError{Code: "read_response"}
	case errors.Is(err, llmmatch.ErrMatcherChatNoChoices):
		return &CallError{Code: "empty_choices"}
	default:
		return &CallError{Code: "decode_envelope"}
	}
}

type openRouterGeneration struct {
	Data struct {
		ID                     string   `json:"id"`
		Model                  string   `json:"model"`
		ProviderName           string   `json:"provider_name"`
		TokensPrompt           *int64   `json:"tokens_prompt"`
		TokensCompletion       *int64   `json:"tokens_completion"`
		NativeTokensPrompt     *int64   `json:"native_tokens_prompt"`
		NativeTokensCompletion *int64   `json:"native_tokens_completion"`
		NativeTokensReasoning  *int64   `json:"native_tokens_reasoning"`
		NativeTokensCached     *int64   `json:"native_tokens_cached"`
		TotalCost              *float64 `json:"total_cost"`
	} `json:"data"`
}

func (c *HostedClient) reconcileOpenRouterGeneration(
	ctx context.Context,
	system SystemConfig,
	apiKey string,
	completion Completion,
) (Completion, int, error) {
	generationID := strings.TrimSpace(completion.GenerationID)
	if generationID == "" {
		return completion, 0, &CallError{Code: "route_unverifiable"}
	}
	policy := c.metadataRetryPolicy()
	auditCtx, cancel := context.WithTimeout(ctx, policy.Timeout)
	defer cancel()
	generation, attempts, err := c.getOpenRouterGeneration(
		auditCtx,
		strings.TrimRight(system.BaseURL, "/")+
			"/generation?id="+url.QueryEscape(generationID),
		apiKey,
		generationID,
		policy.Delays,
	)
	if err != nil {
		return completion, attempts, translateRouteAuditError(ctx, auditCtx, err)
	}
	data := generation.Data
	if strings.TrimSpace(data.ID) != generationID {
		return completion, attempts, &CallError{Code: "route_unverifiable"}
	}
	model := strings.TrimSpace(data.Model)
	provider := strings.TrimSpace(data.ProviderName)
	if model == "" || provider == "" || data.TotalCost == nil {
		return completion, attempts, &CallError{Code: "route_unverifiable"}
	}
	if *data.TotalCost < 0 || math.IsNaN(*data.TotalCost) ||
		math.IsInf(*data.TotalCost, 0) {
		return completion, attempts, &CallError{Code: "route_unverifiable"}
	}
	if completionHasVerifiedInlineRoute(completion) &&
		(!sameRouteIdentifier(model, completion.ReturnedModel) ||
			!sameRouteIdentifier(provider, completion.ReturnedProvider)) {
		return completion, attempts, &CallError{Code: "route_unverifiable"}
	}

	inputTokens := completion.Usage.InputTokens
	if data.NativeTokensPrompt != nil || data.TokensPrompt != nil {
		var ok bool
		inputTokens, ok = preferredNonNegativeTokenCount(
			data.NativeTokensPrompt,
			data.TokensPrompt,
		)
		if !ok {
			return completion, attempts, &CallError{Code: "route_unverifiable"}
		}
	}
	outputTokens := completion.Usage.OutputTokens
	if data.NativeTokensCompletion != nil || data.TokensCompletion != nil {
		var ok bool
		outputTokens, ok = preferredNonNegativeTokenCount(
			data.NativeTokensCompletion,
			data.TokensCompletion,
		)
		if !ok {
			return completion, attempts, &CallError{Code: "route_unverifiable"}
		}
	}
	reasoningTokens := completion.Usage.ReasoningTokens
	var ok bool
	if data.NativeTokensReasoning != nil {
		reasoningTokens, ok = optionalNonNegativeTokenCount(data.NativeTokensReasoning)
	} else {
		ok = reasoningTokens >= 0
	}
	if !ok {
		return completion, attempts, &CallError{Code: "route_unverifiable"}
	}
	cachedTokens := completion.Usage.CachedInputTokens
	if data.NativeTokensCached != nil {
		cachedTokens, ok = optionalNonNegativeTokenCount(data.NativeTokensCached)
	} else {
		ok = cachedTokens >= 0
	}
	if !ok || cachedTokens > inputTokens {
		return completion, attempts, &CallError{Code: "route_unverifiable"}
	}
	cacheWriteTokens := completion.Usage.CacheWriteTokens
	if cacheWriteTokens < 0 {
		return completion, attempts, &CallError{Code: "route_unverifiable"}
	}
	if *data.TotalCost == 0 && !system.isZeroPriced() &&
		(inputTokens != 0 || outputTokens != 0 || reasoningTokens != 0 ||
			cachedTokens != 0 || cacheWriteTokens != 0) {
		// Authenticated metadata is authoritative, but a paid route with
		// nonzero work cannot be promoted as a free request. Treat the
		// contradictory accounting as unverifiable just like an inline paid
		// zero-cost usage envelope.
		return completion, attempts, &CallError{Code: "route_unverifiable"}
	}
	costMicroUSD := *data.TotalCost * system.billingMultiplier() * 1_000_000
	if costMicroUSD >= float64(math.MaxInt64) {
		return completion, attempts, &CallError{Code: "route_unverifiable"}
	}

	completion.ReturnedModel = model
	completion.ReturnedProvider = provider
	completion.RouteProof = RouteProofGenerationMetadata
	completion.Usage = Usage{
		RequestID:         generationID,
		InputTokens:       inputTokens,
		CachedInputTokens: cachedTokens,
		CacheWriteTokens:  cacheWriteTokens,
		OutputTokens:      outputTokens,
		ReasoningTokens:   reasoningTokens,
		CostMicroUSD:      roundPositiveMicroUSD(costMicroUSD),
		CostSource:        CostSourceProviderReported,
		AccountingComplete: usageAccountingCompleteForSystem(
			system,
			inputTokens,
			outputTokens,
		),
	}
	completion.Usage = sanitizeNormalizedUsage(completion.Usage, generationID)
	completion.UsageCostReported = completion.Usage.AccountingComplete
	return completion, attempts, nil
}

func usageAccountingCompleteForSystem(
	system SystemConfig,
	inputTokens int64,
	outputTokens int64,
) bool {
	if inputTokens <= 0 {
		return false
	}
	switch system.APIKind {
	case APIKindEmbedding, APIKindRerank:
		return true
	default:
		return outputTokens > 0
	}
}

func preferredNonNegativeTokenCount(
	preferred *int64,
	fallback *int64,
) (int64, bool) {
	if preferred != nil {
		return *preferred, *preferred >= 0
	}
	if fallback != nil {
		return *fallback, *fallback >= 0
	}
	return 0, false
}

func optionalNonNegativeTokenCount(value *int64) (int64, bool) {
	if value == nil {
		return 0, true
	}
	return *value, *value >= 0
}

func (c *HostedClient) getOpenRouterGeneration(
	ctx context.Context,
	endpoint string,
	apiKey string,
	expectedGenerationID string,
	delays []time.Duration,
) (openRouterGeneration, int, error) {
	for attempt := 0; attempt <= len(delays); attempt++ {
		attempts := attempt + 1
		request, err := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			endpoint,
			nil,
		)
		if err != nil {
			return openRouterGeneration{}, attempts, &CallError{Code: "route_unverifiable"}
		}
		request.Header.Set("Authorization", "Bearer "+apiKey)
		response, err := c.HTTP.Do(request)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) ||
				errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return openRouterGeneration{}, attempts, &CallError{Code: "timeout"}
			}
			if errors.Is(err, context.Canceled) ||
				errors.Is(ctx.Err(), context.Canceled) {
				return openRouterGeneration{}, attempts, &CallError{Code: "canceled"}
			}
			return openRouterGeneration{}, attempts, &CallError{Code: "route_audit_unavailable"}
		}
		raw, readErr := io.ReadAll(io.LimitReader(
			response.Body,
			maxProviderResponseBytes+1,
		))
		closeErr := response.Body.Close()
		if readErr != nil {
			if errors.Is(readErr, context.DeadlineExceeded) ||
				errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return openRouterGeneration{}, attempts, &CallError{Code: "timeout"}
			}
			if errors.Is(readErr, context.Canceled) ||
				errors.Is(ctx.Err(), context.Canceled) {
				return openRouterGeneration{}, attempts, &CallError{Code: "canceled"}
			}
			return openRouterGeneration{}, attempts, &CallError{Code: "route_audit_unavailable"}
		}
		if closeErr != nil {
			return openRouterGeneration{}, attempts, &CallError{Code: "route_audit_unavailable"}
		}
		if len(raw) > maxProviderResponseBytes {
			return openRouterGeneration{}, attempts, &CallError{Code: "response_too_large"}
		}
		retryableStatus := response.StatusCode == http.StatusNotFound ||
			response.StatusCode == http.StatusAccepted ||
			response.StatusCode == http.StatusTooManyRequests
		if retryableStatus {
			if attempt < len(delays) {
				if err := waitForGenerationMetadata(ctx, delays[attempt]); err != nil {
					return openRouterGeneration{}, attempts, err
				}
				continue
			}
			return openRouterGeneration{}, attempts, &CallError{Code: "route_audit_unavailable"}
		}
		if response.StatusCode/100 != 2 {
			code := "route_unverifiable"
			if response.StatusCode/100 == 5 {
				code = "route_audit_unavailable"
			}
			return openRouterGeneration{}, attempts, &CallError{Code: code}
		}
		var generation openRouterGeneration
		if err := json.Unmarshal(raw, &generation); err != nil {
			return openRouterGeneration{}, attempts, &CallError{
				Code: "route_unverifiable",
			}
		}
		if strings.TrimSpace(generation.Data.ID) != expectedGenerationID {
			return openRouterGeneration{}, attempts, &CallError{
				Code: "route_unverifiable",
			}
		}
		if strings.TrimSpace(generation.Data.Model) == "" ||
			strings.TrimSpace(generation.Data.ProviderName) == "" ||
			generation.Data.TotalCost == nil {
			if attempt < len(delays) {
				if err := waitForGenerationMetadata(ctx, delays[attempt]); err != nil {
					return openRouterGeneration{}, attempts, err
				}
				continue
			}
			return openRouterGeneration{}, attempts, &CallError{Code: "route_audit_unavailable"}
		}
		return generation, attempts, nil
	}
	return openRouterGeneration{}, len(delays) + 1, &CallError{Code: "route_audit_unavailable"}
}

func waitForGenerationMetadata(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return &CallError{Code: "timeout"}
		}
		return &CallError{Code: "canceled"}
	case <-timer.C:
		return nil
	}
}

func (c *HostedClient) metadataRetryPolicy() generationMetadataRetryPolicy {
	policy := defaultGenerationMetadataRetryPolicy()
	if c.generationMetadataRetryOverride == nil {
		return policy
	}
	override := *c.generationMetadataRetryOverride
	if override.Timeout <= 0 || len(override.Delays) > 15 {
		return policy
	}
	for _, delay := range override.Delays {
		if delay < 0 {
			return policy
		}
	}
	override.Delays = append([]time.Duration(nil), override.Delays...)
	return override
}

func translateRouteAuditError(
	parent context.Context,
	audit context.Context,
	err error,
) error {
	if errors.Is(parent.Err(), context.DeadlineExceeded) {
		return &CallError{Code: "timeout"}
	}
	if errors.Is(parent.Err(), context.Canceled) {
		return &CallError{Code: "canceled"}
	}
	code := callErrorCode(err)
	if errors.Is(audit.Err(), context.DeadlineExceeded) ||
		errors.Is(err, context.DeadlineExceeded) || code == "timeout" {
		return &CallError{Code: "route_audit_timeout"}
	}
	if errors.Is(audit.Err(), context.Canceled) ||
		errors.Is(err, context.Canceled) || code == "canceled" {
		return &CallError{Code: "route_audit_unavailable"}
	}
	return err
}

func callErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var callErr *CallError
	if errors.As(err, &callErr) && validTimingErrorCode(callErr.Code) {
		return callErr.Code
	}
	return "route_unverifiable"
}

func completionHasVerifiedInlineRoute(completion Completion) bool {
	return completion.RouteProof == RouteProofRouterMetadata &&
		strings.TrimSpace(completion.ReturnedModel) != "" &&
		strings.TrimSpace(completion.ReturnedProvider) != ""
}

func sameRouteIdentifier(left, right string) bool {
	normalize := func(value string) string {
		return strings.ToLower(strings.Join(strings.Fields(value), " "))
	}
	return normalize(left) == normalize(right)
}

func isRecoverableRouteAuditError(err error) bool {
	switch callErrorCode(err) {
	case "route_audit_timeout", "route_audit_unavailable":
		return true
	default:
		return false
	}
}

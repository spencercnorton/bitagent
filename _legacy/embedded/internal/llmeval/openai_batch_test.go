package llmeval

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func testDirectBatchSystem(
	api APIKind,
	contract OutputContract,
	task Task,
) SystemConfig {
	system := SystemConfig{
		SystemID:            "openai-batch-control",
		Provider:            "openai",
		Model:               "gpt-5.4-nano",
		PromptVersion:       "batch-test-v1",
		EvaluationLane:      EvaluationLaneRedactedReference,
		APIKind:             api,
		OutputContract:      contract,
		BaseURL:             "https://api.openai.com/v1",
		APIKeyEnv:           "OPENAI_API_KEY",
		ProviderEndpoint:    "api.openai.com",
		Tasks:               []Task{task},
		InputUSDPerMillion:  1,
		OutputUSDPerMillion: 2,
		NoThinkLocation:     NoThinkLocationNone,
	}
	if contract == OutputContractJSONSchema {
		system.EvaluationLane = EvaluationLaneNormalizedStrict
		system.StructuredOutputs = true
	}
	return system
}

func TestBuildOpenAIBatchStateRejectsInexactTimeoutSemantics(t *testing.T) {
	record := testContentRecord("case-timeout", LanguageEnglish)
	corpus := mustCorpus(t, []CorpusRecord{record})

	production := testDirectBatchSystem(
		APIKindChat,
		OutputContractJSONObject,
		TaskContentFilter,
	)
	production.EvaluationLane = EvaluationLaneProductionFidelity
	production.OutputContract = OutputContractPromptOnly
	production.OmitMaxCompletionTokens = true
	production.NoThinkLocation = NoThinkLocationSystem
	production.NoThinkFormat = NoThinkFormatSingleNewline
	for _, timeoutMS := range []int64{0, 60_000} {
		production.RequestTimeoutMS = timeoutMS
		_, _, err := BuildOpenAIBatchState(
			corpus,
			production,
			testBatchBuildOptions(),
		)
		require.ErrorContains(t, err, "cannot produce production_fidelity")
	}
	production.RequestTimeoutMS = 60_000
	historicalState, historicalInput, err := buildOpenAIBatchState(
		corpus,
		production,
		testBatchBuildOptions(),
		false,
	)
	require.NoError(t, err)
	require.NoError(
		t,
		ValidateOpenAIBatchInput(
			historicalState,
			historicalInput,
			corpus,
			production,
			ProductionThresholds(),
		),
	)

	normalized := testDirectBatchSystem(
		APIKindChat,
		OutputContractJSONSchema,
		TaskContentFilter,
	)
	normalized.RequestTimeoutMS = 60_000
	_, _, err = BuildOpenAIBatchState(
		corpus,
		normalized,
		testBatchBuildOptions(),
	)
	require.ErrorContains(t, err, "cannot reproduce a manifest-bound request timeout")
}

func TestNewUncappedBatchSubmissionRejectsLegacyZeroAllowanceState(t *testing.T) {
	system := testDirectBatchSystem(
		APIKindChat,
		OutputContractJSONSchema,
		TaskContentFilter,
	)
	system.OmitMaxCompletionTokens = true
	state := OpenAIBatchState{}
	require.ErrorContains(
		t,
		ValidateOpenAIBatchNewSubmissionState(state, system),
		"requires a bound positive output allowance",
	)
	state.UncappedOutputAllowance = 512
	require.NoError(t, ValidateOpenAIBatchNewSubmissionState(state, system))
}

func testBatchBuildOptions() OpenAIBatchBuildOptions {
	return OpenAIBatchBuildOptions{
		ManifestSHA256:       testManifestSHA,
		EvaluatorBuildSHA256: testEvaluatorBuildSHA,
		MaxCostMicroUSD:      1_000_000,
		Thresholds:           ProductionThresholds(),
	}
}

func TestBuildOpenAIBatchStateExactChatAndResponsesBodies(t *testing.T) {
	t.Run("redacted-reference chat", func(t *testing.T) {
		record := testExtractRecord(
			"case-chat",
			MatcherExtractExpected{Acceptable: []MatcherExtraction{
				testExtraction("Example Movie"),
			}},
		)
		corpus := mustCorpus(t, []CorpusRecord{record})
		system := testDirectBatchSystem(
			APIKindChat,
			OutputContractJSONObject,
			TaskMatcherExtract,
		)
		system.NoThinkLocation = NoThinkLocationUser
		system.NoThinkFormat = NoThinkFormatDoubleNewline
		system.AppendNoThink = true

		state, raw, err := BuildOpenAIBatchState(
			corpus,
			system,
			testBatchBuildOptions(),
		)
		require.NoError(t, err)
		require.Equal(t, "/v1/chat/completions", state.Endpoint)
		require.Equal(t, OpenAIBatchPricingPPM, state.PricingMultiplierPPM)

		var line openAIBatchInputLine
		require.NoError(t, json.Unmarshal(bytes.TrimSpace(raw), &line))
		var body map[string]any
		require.NoError(t, json.Unmarshal(line.Body, &body))
		require.Equal(t, false, body["stream"])
		require.Equal(t, float64(120), body["max_completion_tokens"])
		require.Equal(
			t,
			map[string]any{"type": "json_object"},
			body["response_format"],
		)
		messages := body["messages"].([]any)
		user := messages[1].(map[string]any)["content"].(string)
		require.True(t, strings.HasSuffix(user, "\n\n/no_think"))
		require.NoError(
			t,
			ValidateOpenAIBatchInput(
				state,
				raw,
				corpus,
				system,
				ProductionThresholds(),
			),
		)
	})

	t.Run("normalized responses", func(t *testing.T) {
		record := testContentRecord("case-responses", LanguageEnglish)
		corpus := mustCorpus(t, []CorpusRecord{record})
		system := testDirectBatchSystem(
			APIKindResponses,
			OutputContractJSONSchema,
			TaskContentFilter,
		)
		state, raw, err := BuildOpenAIBatchState(
			corpus,
			system,
			testBatchBuildOptions(),
		)
		require.NoError(t, err)
		require.Equal(t, "/v1/responses", state.Endpoint)
		var line openAIBatchInputLine
		require.NoError(t, json.Unmarshal(bytes.TrimSpace(raw), &line))
		var body map[string]any
		require.NoError(t, json.Unmarshal(line.Body, &body))
		require.Equal(t, float64(120), body["max_output_tokens"])
		format := body["text"].(map[string]any)["format"].(map[string]any)
		require.Equal(t, "json_schema", format["type"])
		require.Equal(t, true, format["strict"])
	})

	t.Run("redacted-reference responses omits cap", func(t *testing.T) {
		record := testContentRecord("case-uncapped", LanguageEnglish)
		corpus := mustCorpus(t, []CorpusRecord{record})
		system := testDirectBatchSystem(
			APIKindResponses,
			OutputContractPromptOnly,
			TaskContentFilter,
		)
		system.OmitMaxCompletionTokens = true
		options := testBatchBuildOptions()
		options.UncappedOutputAllowance = 512
		_, raw, err := BuildOpenAIBatchState(corpus, system, options)
		require.NoError(t, err)
		var line openAIBatchInputLine
		require.NoError(t, json.Unmarshal(bytes.TrimSpace(raw), &line))
		var body map[string]any
		require.NoError(t, json.Unmarshal(line.Body, &body))
		_, hasCap := body["max_output_tokens"]
		require.False(t, hasCap)
		_, hasText := body["text"]
		require.False(t, hasText)
	})

	t.Run("uncapped chat binds explicit allowance", func(t *testing.T) {
		record := testContentRecord("case-uncapped-chat", LanguageEnglish)
		corpus := mustCorpus(t, []CorpusRecord{record})
		system := testDirectBatchSystem(
			APIKindChat,
			OutputContractJSONSchema,
			TaskContentFilter,
		)
		system.OmitMaxCompletionTokens = true
		options := testBatchBuildOptions()
		options.UncappedOutputAllowance = 512
		state, raw, err := BuildOpenAIBatchState(corpus, system, options)
		require.NoError(t, err)
		require.Equal(t, int64(512), state.UncappedOutputAllowance)
		var line openAIBatchInputLine
		require.NoError(t, json.Unmarshal(bytes.TrimSpace(raw), &line))
		var body map[string]any
		require.NoError(t, json.Unmarshal(line.Body, &body))
		require.NotContains(t, body, "max_completion_tokens")
		require.NotContains(t, body, "max_tokens")

		options.UncappedOutputAllowance = 0
		_, _, err = BuildOpenAIBatchState(corpus, system, options)
		require.ErrorContains(t, err, "uncapped generative request requires output allowance")
	})
}

func TestBuildOpenAIBatchStateCostFuseUsesHalfPrice(t *testing.T) {
	record := testContentRecord("case-cost", LanguageEnglish)
	corpus := mustCorpus(t, []CorpusRecord{record})
	system := testDirectBatchSystem(
		APIKindChat,
		OutputContractJSONSchema,
		TaskContentFilter,
	)
	options := testBatchBuildOptions()
	state, _, err := BuildOpenAIBatchState(corpus, system, options)
	require.NoError(t, err)
	require.Positive(t, state.EstimatedCeilingMicroUSD)

	options.MaxCostMicroUSD = state.EstimatedCeilingMicroUSD - 1
	_, _, err = BuildOpenAIBatchState(corpus, system, options)
	require.ErrorContains(t, err, "exceeds explicit cost fuse")
}

func TestOpenAIBatchPricingHalvesEveryCostComponentExactlyOnce(t *testing.T) {
	system := testDirectBatchSystem(
		APIKindChat,
		OutputContractJSONSchema,
		TaskContentFilter,
	)
	system.InputUSDPerMillion = 2
	system.CachedInputUSDPerMillion = 4
	system.CacheWriteUSDPerMillion = 6
	system.OutputUSDPerMillion = 8
	system.USDPerRequest = 0.01

	priced := openAIBatchPricedSystem(system)
	require.Equal(t, 1.0, priced.InputUSDPerMillion)
	require.Equal(t, 2.0, priced.CachedInputUSDPerMillion)
	require.Equal(t, 3.0, priced.CacheWriteUSDPerMillion)
	require.Equal(t, 4.0, priced.OutputUSDPerMillion)
	require.Equal(t, 0.005, priced.USDPerRequest)
}

func TestOpenAIBatchStateRejectsImmutableTampering(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testContentRecord("case-state", LanguageEnglish),
	})
	system := testDirectBatchSystem(
		APIKindChat,
		OutputContractJSONSchema,
		TaskContentFilter,
	)
	state, _, err := BuildOpenAIBatchState(
		corpus,
		system,
		testBatchBuildOptions(),
	)
	require.NoError(t, err)
	state.CostFuseMicroUSD++
	require.ErrorContains(t, state.Validate(), "plan identity mismatch")
}

func TestReadOpenAIBatchStateUpgradesLegacyCleanupStates(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testContentRecord("case-legacy-state", LanguageEnglish),
	})
	system := testDirectBatchSystem(
		APIKindChat,
		OutputContractJSONSchema,
		TaskContentFilter,
	)
	base, _, err := BuildOpenAIBatchState(
		corpus,
		system,
		testBatchBuildOptions(),
	)
	require.NoError(t, err)

	t.Run("completed with active files", func(t *testing.T) {
		state := base
		state.Status = OpenAIBatchStateCompleted
		state.BatchID = "batch_legacy"
		state.ProviderStatus = "completed"
		state.InputFileID = "file_input"
		state.OutputFileID = "file_output"
		state.ResultsSHA256 = strings.Repeat("a", 64)
		state.CostCoverageRequests = state.RequestCount
		raw, marshalErr := json.Marshal(state)
		require.NoError(t, marshalErr)

		upgraded, _, readErr := ReadOpenAIBatchState(bytes.NewReader(raw))
		require.NoError(t, readErr)
		require.Equal(t, OpenAIBatchStateCleanupRequired, upgraded.Status)
		require.Equal(t, OpenAIBatchStateCompleted, upgraded.CleanupFinalStatus)
		require.Equal(t, "file_input", upgraded.InputFileID)
		require.Equal(t, "file_output", upgraded.OutputFileID)
	})

	t.Run("privacy cleanup without explicit target", func(t *testing.T) {
		state := base
		state.Status = OpenAIBatchStateCleanupRequired
		state.BatchID = "batch_legacy_privacy"
		state.ProviderStatus = "cancelled"
		state.InputFileID = "file_input"
		raw, marshalErr := json.Marshal(state)
		require.NoError(t, marshalErr)

		upgraded, _, readErr := ReadOpenAIBatchState(bytes.NewReader(raw))
		require.NoError(t, readErr)
		require.Equal(t, OpenAIBatchStateCleanupRequired, upgraded.Status)
		require.Equal(
			t,
			OpenAIBatchStatePrivacyBlocked,
			upgraded.CleanupFinalStatus,
		)
	})

	t.Run("unverifiable terminal failure with active input", func(t *testing.T) {
		state := base
		state.Status = OpenAIBatchStateProviderFailed
		state.BatchID = "batch_legacy_failed"
		state.ProviderStatus = "failed"
		state.InputFileID = "file_input"
		state.ActualCostUnknown = true
		raw, marshalErr := json.Marshal(state)
		require.NoError(t, marshalErr)

		upgraded, _, readErr := ReadOpenAIBatchState(bytes.NewReader(raw))
		require.NoError(t, readErr)
		require.Equal(t, OpenAIBatchStateCleanupRequired, upgraded.Status)
		require.Equal(
			t,
			OpenAIBatchStateProviderFailed,
			upgraded.CleanupFinalStatus,
		)
		require.Equal(
			t,
			OpenAIBatchTerminalErrorLegacyUnverifiable,
			upgraded.TerminalErrorCode,
		)
	})
}

func TestOpenAIBatchCleanupTargetMustAlreadyHaveTerminalEvidence(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testContentRecord("case-cleanup-evidence", LanguageEnglish),
	})
	system := testDirectBatchSystem(
		APIKindChat,
		OutputContractJSONSchema,
		TaskContentFilter,
	)
	state, _, err := BuildOpenAIBatchState(
		corpus,
		system,
		testBatchBuildOptions(),
	)
	require.NoError(t, err)
	state.Status = OpenAIBatchStateCleanupRequired
	state.CleanupFinalStatus = OpenAIBatchStateCompleted
	state.InputFileID = "file_input"
	require.ErrorContains(
		t,
		state.Validate(),
		"completed Batch state requires exact full cost and result coverage",
	)
}

func TestReconcileOpenAIBatchOutputUnorderedJoinAndHalfPrice(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testContentRecord("case-a", LanguageEnglish),
		testContentRecord("case-b", LanguageNonEnglish),
	})
	system := testDirectBatchSystem(
		APIKindChat,
		OutputContractJSONSchema,
		TaskContentFilter,
	)
	state, _, err := BuildOpenAIBatchState(
		corpus,
		system,
		testBatchBuildOptions(),
	)
	require.NoError(t, err)
	state.BatchID = "batch_test"
	state.InputFileID = "file_input"
	state.Status = OpenAIBatchStateSubmitted

	line := func(customID, id, content string) []byte {
		t.Helper()
		raw, marshalErr := json.Marshal(map[string]any{
			"id":        "batch_req_" + id,
			"custom_id": customID,
			"response": map[string]any{
				"status_code": 200,
				"request_id":  "req_outer_" + id,
				"body": map[string]any{
					"id":    "chatcmpl_" + id,
					"model": "gpt-5.4-nano-2026-03-17",
					"choices": []any{map[string]any{
						"message": map[string]any{"content": content},
					}},
					"usage": map[string]any{
						"prompt_tokens":     100,
						"completion_tokens": 20,
					},
				},
			},
			"error": nil,
		})
		require.NoError(t, marshalErr)
		return append(raw, '\n')
	}
	// Reverse provider order to prove custom_id, not line position, joins.
	output := append(
		line(
			state.Cases[1].CustomID,
			"b",
			`{"is_english":false,"confidence":0.99,"reason":"foreign"}`,
		),
		line(
			state.Cases[0].CustomID,
			"a",
			`{"is_english":true,"confidence":0.99,"reason":"english"}`,
		)...,
	)
	summary, err := ReconcileOpenAIBatchOutput(
		state,
		corpus,
		system,
		state.BatchID,
		output,
		nil,
		ProductionThresholds(),
	)
	require.NoError(t, err)
	require.Equal(t, 2, summary.Completed)
	require.Zero(t, summary.Failed)
	// Each request: 100*$1/M + 20*$2/M = 140 microUSD standard,
	// therefore 70 microUSD under Batch.
	require.Equal(t, int64(140), summary.CostMicroUSD)
	require.Equal(t, CostSourceManifestEstimate, summary.Results[0].Usage.CostSource)
	require.Equal(t, CostSourceManifestEstimate, summary.Results[1].Usage.CostSource)
	require.Equal(t, "case-a", summary.Results[0].CaseID)
	require.Equal(t, state.Cases[0].CustomID,
		summary.Results[0].ExecutionAudit.OpenAIBatch.CustomID)
	require.True(t, summary.Results[0].ExecutionAudit.Route.Verified)
	require.Equal(
		t,
		CurrentCostAccountingVersion,
		summary.Results[0].ExecutionAudit.CostAccountingVersion,
	)
	tampered := summary.Results[0]
	tampered.Usage.CostSource = ""
	require.ErrorContains(
		t,
		tampered.ValidateWithThresholds(ProductionThresholds()),
		"cost_source",
	)
}

func TestReconcileOpenAIBatchOutputFailsClosedOnCoverageAndRoute(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testContentRecord("case-only", LanguageEnglish),
	})
	system := testDirectBatchSystem(
		APIKindChat,
		OutputContractJSONSchema,
		TaskContentFilter,
	)
	state, _, err := BuildOpenAIBatchState(
		corpus,
		system,
		testBatchBuildOptions(),
	)
	require.NoError(t, err)
	state.BatchID = "batch_test"
	state.InputFileID = "file_input"
	state.Status = OpenAIBatchStateSubmitted

	_, err = ReconcileOpenAIBatchOutput(
		state,
		corpus,
		system,
		state.BatchID,
		nil,
		nil,
		ProductionThresholds(),
	)
	require.ErrorContains(t, err, "covers 0 custom IDs")

	raw, err := json.Marshal(map[string]any{
		"id":        "batch_req_wrong",
		"custom_id": state.Cases[0].CustomID,
		"response": map[string]any{
			"status_code": 200,
			"request_id":  "req_wrong",
			"body": map[string]any{
				"id":    "chatcmpl_wrong",
				"model": "gpt-5.6-terra",
				"choices": []any{map[string]any{"message": map[string]any{
					"content": `{"is_english":true,"confidence":0.99,"reason":"english"}`,
				}}},
				"usage": map[string]any{
					"prompt_tokens": 10, "completion_tokens": 10,
				},
			},
		},
		"error": nil,
	})
	require.NoError(t, err)
	summary, err := ReconcileOpenAIBatchOutput(
		state,
		corpus,
		system,
		state.BatchID,
		append(raw, '\n'),
		nil,
		ProductionThresholds(),
	)
	require.NoError(t, err)
	require.Equal(t, 1, summary.Failed)
	require.Equal(t, "route_model_mismatch", summary.Results[0].ErrorCode)
	require.False(t, summary.Results[0].ExecutionAudit.Route.Verified)
}

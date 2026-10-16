package llmeval

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeCompleter struct {
	calls int
	reply Completion
	err   error
}

func (f *fakeCompleter) Complete(
	context.Context,
	SystemConfig,
	string,
	PromptRequest,
) (Completion, error) {
	f.calls++
	return f.reply, f.err
}

type sequenceCompleter struct {
	replies []Completion
	calls   int
}

func (s *sequenceCompleter) Complete(
	context.Context,
	SystemConfig,
	string,
	PromptRequest,
) (Completion, error) {
	reply := s.replies[s.calls]
	s.calls++
	return reply, nil
}

func TestRunCorpusEnforcesCostCapBeforeCall(t *testing.T) {
	corpus, err := NewCorpus([]CorpusRecord{contentRecord("case-1", "Example")})
	require.NoError(t, err)
	system := testOpenRouterSystem(TaskContentFilter)
	client := &fakeCompleter{
		reply: Completion{
			Text: []byte(`{"is_english":true,"confidence":0.9,"reason":"clear"}`),
		},
	}

	summary, err := RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		client,
		RunOptions{
			MaxCostMicroUSD:      1,
			Thresholds:           ProductionThresholds(),
			EvaluatorBuildSHA256: testEvaluatorBuildSHA,
			ExecutionBinding:     testExecutionBinding(),
		},
	)
	require.NoError(t, err)
	require.True(t, summary.StoppedByCostCap)
	require.Zero(t, client.calls)
	require.Empty(t, summary.Results)
}

func TestRunCorpusRecordsCeiledSynchronousRequestTiming(t *testing.T) {
	corpus, err := NewCorpus([]CorpusRecord{contentRecord("case-1", "Example")})
	require.NoError(t, err)
	system := testOpenRouterSystem(TaskContentFilter)
	client := &fakeCompleter{
		reply: Completion{
			Text:             []byte(`{"is_english":true,"confidence":0.9,"reason":"clear"}`),
			ReturnedModel:    "test/model",
			ReturnedProvider: "Test Provider",
			RouteProof:       RouteProofRouterMetadata,
		},
	}
	var persisted ResultRecord

	summary, err := RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		client,
		RunOptions{
			Thresholds:           ProductionThresholds(),
			EvaluatorBuildSHA256: testEvaluatorBuildSHA,
			ExecutionBinding:     testExecutionBinding(),
			OnResult: func(result ResultRecord) error {
				persisted = result
				return nil
			},
		},
	)
	require.NoError(t, err)
	require.Len(t, summary.Results, 1)
	require.NotNil(t, summary.Results[0].RequestTiming)
	require.Positive(t, summary.Results[0].RequestTiming.ElapsedMS)
	require.Equal(t, system.RequestTimeoutMS, summary.Results[0].RequestTiming.DeadlineMS)
	require.Equal(t, CurrentCostAccountingVersion, summary.Results[0].ExecutionAudit.CostAccountingVersion)
	require.Equal(t, CostSourceUnavailable, summary.Results[0].Usage.CostSource)
	require.Equal(t, summary.Results[0].RequestTiming, persisted.RequestTiming)
}

func TestElapsedMillisecondsCeil(t *testing.T) {
	require.Equal(t, int64(1), elapsedMillisecondsCeil(0))
	require.Equal(t, int64(1), elapsedMillisecondsCeil(time.Nanosecond))
	require.Equal(t, int64(1), elapsedMillisecondsCeil(time.Millisecond))
	require.Equal(t, int64(2), elapsedMillisecondsCeil(time.Millisecond+time.Nanosecond))
}

func TestRunCorpusKeepsPrimaryLatencySeparateFromRouteAudit(t *testing.T) {
	corpus, err := NewCorpus([]CorpusRecord{contentRecord("case-1", "Example")})
	require.NoError(t, err)
	system := testOpenRouterSystem(TaskContentFilter)
	client := &fakeCompleter{
		reply: Completion{
			Text:             []byte(`{"is_english":true,"confidence":0.9,"reason":"clear"}`),
			ReturnedModel:    "test/model",
			ReturnedProvider: "Test Provider",
			RouteProof:       RouteProofRouterMetadata,
			RequestTiming: &RequestTiming{
				ElapsedMS: 1, DeadlineMS: system.RequestTimeoutMS,
				TotalElapsedMS: maxRequestElapsedMS,
				RouteAudit: &RouteAuditTiming{
					ElapsedMS: 1, DeadlineMS: OpenRouterGenerationAuditDeadlineMS,
					Attempts: 2, Succeeded: true,
				},
			},
		},
	}

	summary, err := RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		client,
		RunOptions{
			Thresholds:           ProductionThresholds(),
			EvaluatorBuildSHA256: testEvaluatorBuildSHA,
			ExecutionBinding:     testExecutionBinding(),
		},
	)
	require.NoError(t, err)
	require.Len(t, summary.Results, 1)
	timing := summary.Results[0].RequestTiming
	require.NotNil(t, timing)
	require.Equal(t, int64(1), timing.ElapsedMS)
	require.Equal(t, system.RequestTimeoutMS, timing.DeadlineMS)
	require.Positive(t, timing.TotalElapsedMS)
	require.Less(t, timing.TotalElapsedMS, maxRequestElapsedMS)
	require.Equal(t, int64(1), timing.RouteAudit.ElapsedMS)
	require.NotSame(t, client.reply.RequestTiming, timing)
	require.NotSame(t, client.reply.RequestTiming.RouteAudit, timing.RouteAudit)
}

func TestRunCorpusRejectsFreshRequestTimingDeadlineMismatch(t *testing.T) {
	corpus, err := NewCorpus([]CorpusRecord{contentRecord("case-1", "Example")})
	require.NoError(t, err)
	system := testOpenRouterSystem(TaskContentFilter)
	client := &fakeCompleter{
		reply: Completion{
			Text:             []byte(`{"is_english":true,"confidence":0.9,"reason":"clear"}`),
			ReturnedModel:    "test/model",
			ReturnedProvider: "Test Provider",
			RouteProof:       RouteProofRouterMetadata,
			RequestTiming: &RequestTiming{
				ElapsedMS:      7,
				DeadlineMS:     system.RequestTimeoutMS + 1,
				TotalElapsedMS: 20,
				RouteAudit: &RouteAuditTiming{
					ElapsedMS:  12,
					DeadlineMS: OpenRouterGenerationAuditDeadlineMS,
					Attempts:   2,
					Succeeded:  true,
				},
			},
		},
	}
	persisted := false

	summary, err := RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		client,
		RunOptions{
			Thresholds:           ProductionThresholds(),
			EvaluatorBuildSHA256: testEvaluatorBuildSHA,
			ExecutionBinding:     testExecutionBinding(),
			OnResult: func(ResultRecord) error {
				persisted = true
				return nil
			},
		},
	)
	require.ErrorContains(t, err, `case "case-1" client request timing`)
	require.ErrorContains(
		t,
		err,
		"deadline 60001 does not match manifest-bound request timeout 60000",
	)
	require.Equal(t, 1, client.calls)
	require.Equal(t, 1, summary.Requests)
	require.Empty(t, summary.Results)
	require.Zero(t, summary.Completed)
	require.False(t, persisted)
}

func TestRunCorpusRejectsRequestWithoutManifestDeadline(t *testing.T) {
	corpus, err := NewCorpus([]CorpusRecord{contentRecord("case-1", "Example")})
	require.NoError(t, err)
	system := testOpenRouterSystem(TaskContentFilter)
	system.RequestTimeoutMS = 0
	client := &fakeCompleter{}

	_, err = RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		client,
		RunOptions{
			Thresholds:           ProductionThresholds(),
			EvaluatorBuildSHA256: testEvaluatorBuildSHA,
			ExecutionBinding:     testExecutionBinding(),
		},
	)
	require.ErrorContains(t, err, "positive manifest-bound request_timeout_ms")
	require.Zero(t, client.calls)
}

func TestRunCorpusCostFuseRetainsCeilingWhenUsageIsUnavailable(t *testing.T) {
	corpus, err := NewCorpus([]CorpusRecord{
		contentRecord("case-1", "First"),
		contentRecord("case-2", "Other"),
	})
	require.NoError(t, err)
	system := testOpenRouterSystem(TaskContentFilter)
	firstPrompt, err := BuildPrompt(corpus.Records[0])
	require.NoError(t, err)
	firstCeiling := estimateRequestCeiling(system, firstPrompt, 0)
	require.Positive(t, firstCeiling)
	client := &fakeCompleter{
		reply: Completion{
			Text:             []byte(`{"is_english":true,"confidence":0.9,"reason":"clear"}`),
			ReturnedModel:    "test/model",
			ReturnedProvider: "Test Provider",
			RouteProof:       RouteProofRouterMetadata,
			// A live-valid response with missing or malformed usage reaches the
			// runner with zero observed usage. The action remains valid, but the
			// local spend fuse must retain the pre-call ceiling.
			Usage: Usage{},
		},
	}

	summary, err := RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		client,
		RunOptions{
			MaxCostMicroUSD:      firstCeiling,
			Thresholds:           ProductionThresholds(),
			EvaluatorBuildSHA256: testEvaluatorBuildSHA,
			ExecutionBinding:     testExecutionBinding(),
		},
	)
	require.NoError(t, err)
	require.Equal(t, 1, client.calls)
	require.Equal(t, 1, summary.Completed)
	require.True(t, summary.StoppedByCostCap)
	require.Zero(t, summary.CostMicroUSD, "reported usage remains observed/unknown")
	require.Equal(t, firstCeiling, summary.AccountedCostMicroUSD)
}

func TestRunCorpusCostFuseDistrustsPartialUsageWithPositiveCost(t *testing.T) {
	corpus, err := NewCorpus([]CorpusRecord{
		contentRecord("case-1", "First"),
		contentRecord("case-2", "Other"),
	})
	require.NoError(t, err)
	system := testOpenRouterSystem(TaskContentFilter)
	firstPrompt, err := BuildPrompt(corpus.Records[0])
	require.NoError(t, err)
	firstCeiling := estimateRequestCeiling(system, firstPrompt, 0)
	client := &fakeCompleter{
		reply: Completion{
			Text:             []byte(`{"is_english":true,"confidence":0.9,"reason":"clear"}`),
			ReturnedModel:    "test/model",
			ReturnedProvider: "Test Provider",
			RouteProof:       RouteProofRouterMetadata,
			Usage: Usage{
				InputTokens:        1,
				CostMicroUSD:       1,
				CostSource:         CostSourceProviderReported,
				AccountingComplete: true,
			},
		},
	}

	summary, err := RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		client,
		RunOptions{
			MaxCostMicroUSD:      firstCeiling,
			Thresholds:           ProductionThresholds(),
			EvaluatorBuildSHA256: testEvaluatorBuildSHA,
			ExecutionBinding:     testExecutionBinding(),
		},
	)
	require.NoError(t, err)
	require.Equal(t, 1, client.calls)
	require.True(t, summary.StoppedByCostCap)
	require.Equal(t, int64(1), summary.CostMicroUSD)
	require.Equal(t, firstCeiling, summary.AccountedCostMicroUSD)
}

func TestRunCorpusCostFuseRetainsObservedChargeAboveCeiling(t *testing.T) {
	corpus, err := NewCorpus([]CorpusRecord{
		contentRecord("case-1", "First"),
		contentRecord("case-2", "Other"),
	})
	require.NoError(t, err)
	system := testOpenRouterSystem(TaskContentFilter)
	firstPrompt, err := BuildPrompt(corpus.Records[0])
	require.NoError(t, err)
	firstCeiling := estimateRequestCeiling(system, firstPrompt, 0)
	observedCharge := firstCeiling + 1
	client := &fakeCompleter{
		reply: Completion{
			Text:             []byte(`{"is_english":true,"confidence":0.9,"reason":"clear"}`),
			ReturnedModel:    "test/model",
			ReturnedProvider: "Test Provider",
			RouteProof:       RouteProofRouterMetadata,
			Usage: Usage{
				InputTokens:  1,
				CostMicroUSD: observedCharge,
				CostSource:   CostSourceProviderReported,
				// A missing output count keeps accounting incomplete, but the
				// authenticated positive charge must still raise the fuse ledger.
			},
		},
	}

	summary, err := RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		client,
		RunOptions{
			MaxCostMicroUSD:      firstCeiling,
			Thresholds:           ProductionThresholds(),
			EvaluatorBuildSHA256: testEvaluatorBuildSHA,
			ExecutionBinding:     testExecutionBinding(),
		},
	)
	require.NoError(t, err)
	require.Equal(t, 1, client.calls)
	require.True(t, summary.StoppedByCostCap)
	require.Equal(t, observedCharge, summary.CostMicroUSD)
	require.Equal(t, observedCharge, summary.AccountedCostMicroUSD)
}

func TestRunCorpusResumeChargesPartialExistingUsageConservatively(t *testing.T) {
	corpus, err := NewCorpus([]CorpusRecord{
		contentRecord("case-1", "First"),
		contentRecord("case-2", "Other"),
	})
	require.NoError(t, err)
	system := testOpenRouterSystem(TaskContentFilter)
	firstPrompt, err := BuildPrompt(corpus.Records[0])
	require.NoError(t, err)
	firstCeiling := estimateRequestCeiling(system, firstPrompt, 0)
	existing := ParseCompletion(
		corpus.SHA256,
		corpus.Records[0],
		system,
		Completion{
			Text: []byte(`{"is_english":true,"confidence":0.9,"reason":"clear"}`),
		},
		ProductionThresholds(),
	)
	existing.EvaluatorBuildSHA256 = testEvaluatorBuildSHA
	existing.ExecutionAudit = testOpenRouterExecutionAudit()
	existing.Usage = Usage{
		InputTokens:        1,
		CostMicroUSD:       1,
		AccountingComplete: true,
	}
	client := &fakeCompleter{}

	summary, err := RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		client,
		RunOptions{
			MaxCostMicroUSD:      firstCeiling,
			Thresholds:           ProductionThresholds(),
			EvaluatorBuildSHA256: testEvaluatorBuildSHA,
			ExecutionBinding:     testExecutionBinding(),
			Existing:             []ResultRecord{existing},
		},
	)
	require.NoError(t, err)
	require.Zero(t, client.calls)
	require.Equal(t, 1, summary.Skipped)
	require.True(t, summary.StoppedByCostCap)
	require.Equal(t, int64(1), summary.CostMicroUSD)
	require.Equal(t, firstCeiling, summary.AccountedCostMicroUSD)
}

func TestRunCorpusUncappedResponsesLedgerUsesExplicitAllowance(t *testing.T) {
	corpus, err := NewCorpus([]CorpusRecord{
		contentRecord("case-1", "First"),
		contentRecord("case-2", "Other"),
	})
	require.NoError(t, err)
	system := SystemConfig{
		SystemID: "uncapped", Provider: "openai", Model: "gpt-5.6-luna",
		PromptVersion: "production-current", EvaluationLane: EvaluationLaneProductionFidelity,
		APIKind: APIKindResponses, OutputContract: OutputContractPromptOnly,
		BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY",
		ProviderEndpoint: "api.openai.com", Tasks: []Task{TaskContentFilter},
		InputUSDPerMillion: 1, OutputUSDPerMillion: 1,
		NoThinkLocation: NoThinkLocationNone, OmitMaxCompletionTokens: true,
		RequestTimeoutMS: 8_000,
	}
	client := &fakeCompleter{}
	_, err = RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		client,
		RunOptions{
			MaxCostMicroUSD:      math.MaxInt64,
			Thresholds:           ProductionThresholds(),
			EvaluatorBuildSHA256: testEvaluatorBuildSHA,
			ExecutionBinding:     ExecutionBinding{ManifestSHA256: testManifestSHA},
		},
	)
	require.ErrorContains(t, err, "positive output-token allowance")
	require.Zero(t, client.calls)

	const allowance = int64(10_000)
	firstPrompt, err := BuildPrompt(corpus.Records[0])
	require.NoError(t, err)
	firstCeiling := estimateRequestCeiling(system, firstPrompt, allowance)
	client.reply = Completion{
		Text:          []byte(`{"is_english":true,"confidence":0.9,"reason":"clear"}`),
		ReturnedModel: system.Model,
		RouteProof:    RouteProofDirectResponseModel,
	}
	summary, err := RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		client,
		RunOptions{
			MaxCostMicroUSD:              firstCeiling,
			UncappedOutputTokenAllowance: allowance,
			Thresholds:                   ProductionThresholds(),
			EvaluatorBuildSHA256:         testEvaluatorBuildSHA,
			ExecutionBinding:             ExecutionBinding{ManifestSHA256: testManifestSHA},
		},
	)
	require.NoError(t, err)
	require.Equal(t, 1, client.calls)
	require.True(t, summary.StoppedByCostCap)
	require.Equal(t, firstCeiling, summary.AccountedCostMicroUSD)
	require.Equal(
		t,
		allowance,
		summary.Results[0].ExecutionAudit.UncappedOutputTokenAllowance,
	)

	resumeClient := &fakeCompleter{}
	_, err = RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		resumeClient,
		RunOptions{
			MaxCostMicroUSD:              math.MaxInt64,
			UncappedOutputTokenAllowance: 120,
			Thresholds:                   ProductionThresholds(),
			EvaluatorBuildSHA256:         testEvaluatorBuildSHA,
			ExecutionBinding:             ExecutionBinding{ManifestSHA256: testManifestSHA},
			Existing:                     summary.Results,
		},
	)
	require.ErrorContains(t, err, "different uncapped output-token allowance")
	require.Zero(t, resumeClient.calls)

	overAllowance := summary.Results[0]
	overAllowance.Usage.OutputTokens = allowance + 1
	_, err = RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		resumeClient,
		RunOptions{
			MaxCostMicroUSD:              math.MaxInt64,
			UncappedOutputTokenAllowance: allowance,
			Thresholds:                   ProductionThresholds(),
			EvaluatorBuildSHA256:         testEvaluatorBuildSHA,
			ExecutionBinding:             ExecutionBinding{ManifestSHA256: testManifestSHA},
			Existing:                     []ResultRecord{overAllowance},
		},
	)
	require.ErrorContains(t, err, "exceeds its bound uncapped output-token allowance")
	require.Zero(t, resumeClient.calls)
}

func TestRunCorpusUncappedChatPersistsAllowanceAndDoesNotRepeat(t *testing.T) {
	corpus, err := NewCorpus([]CorpusRecord{
		contentRecord("case-1", "First"),
		contentRecord("case-2", "Other"),
	})
	require.NoError(t, err)
	system := testOpenRouterSystem(TaskContentFilter)
	system.OmitMaxCompletionTokens = true
	const allowance = int64(500)
	firstPrompt, err := BuildPrompt(corpus.Records[0])
	require.NoError(t, err)
	firstCeiling := estimateRequestCeiling(system, firstPrompt, allowance)
	client := &fakeCompleter{reply: Completion{
		Text:             []byte(`{"is_english":true,"confidence":0.9,"reason":"clear"}`),
		ReturnedModel:    "test/model",
		ReturnedProvider: "Test Provider",
		RouteProof:       RouteProofRouterMetadata,
	}}

	first, err := RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		client,
		RunOptions{
			MaxCostMicroUSD:              firstCeiling,
			UncappedOutputTokenAllowance: allowance,
			Thresholds:                   ProductionThresholds(),
			EvaluatorBuildSHA256:         testEvaluatorBuildSHA,
			ExecutionBinding:             testExecutionBinding(),
		},
	)
	require.NoError(t, err)
	require.Equal(t, 1, client.calls)
	require.Len(t, first.Results, 1)
	require.Equal(t, allowance, first.Results[0].ExecutionAudit.UncappedOutputTokenAllowance)

	resumeClient := &fakeCompleter{}
	resumed, err := RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		resumeClient,
		RunOptions{
			MaxCostMicroUSD:              firstCeiling,
			UncappedOutputTokenAllowance: allowance,
			Thresholds:                   ProductionThresholds(),
			EvaluatorBuildSHA256:         testEvaluatorBuildSHA,
			ExecutionBinding:             testExecutionBinding(),
			Existing:                     first.Results,
		},
	)
	require.NoError(t, err)
	require.Zero(t, resumeClient.calls)
	require.Equal(t, 1, resumed.Skipped)
	require.True(t, resumed.StoppedByCostCap)
}

func TestRunCorpusRetainsThenStopsOnOutputAboveAllowance(t *testing.T) {
	corpus, err := NewCorpus([]CorpusRecord{contentRecord("case-1", "First")})
	require.NoError(t, err)
	system := SystemConfig{
		SystemID: "uncapped", Provider: "openai", Model: "gpt-5.6-luna",
		PromptVersion: "production-current", EvaluationLane: EvaluationLaneProductionFidelity,
		APIKind: APIKindResponses, OutputContract: OutputContractPromptOnly,
		BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY",
		Tasks: []Task{TaskContentFilter}, InputUSDPerMillion: 1, OutputUSDPerMillion: 1,
		NoThinkLocation: NoThinkLocationNone, OmitMaxCompletionTokens: true,
		RequestTimeoutMS: 8_000,
	}
	const allowance = int64(120)
	client := &fakeCompleter{reply: Completion{
		Text:          []byte(`{"is_english":true,"confidence":0.9,"reason":"clear"}`),
		ReturnedModel: system.Model,
		RouteProof:    RouteProofDirectResponseModel,
		Usage: Usage{
			InputTokens:        10,
			OutputTokens:       allowance + 1,
			CostMicroUSD:       10,
			CostSource:         CostSourceManifestEstimate,
			AccountingComplete: true,
		},
	}}
	var persisted []ResultRecord
	summary, err := RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		client,
		RunOptions{
			MaxCostMicroUSD:              math.MaxInt64,
			UncappedOutputTokenAllowance: allowance,
			Thresholds:                   ProductionThresholds(),
			EvaluatorBuildSHA256:         testEvaluatorBuildSHA,
			ExecutionBinding:             ExecutionBinding{ManifestSHA256: testManifestSHA},
			OnResult: func(result ResultRecord) error {
				persisted = append(persisted, result)
				return nil
			},
		},
	)
	require.ErrorContains(t, err, "exceeded the bound uncapped output-token allowance")
	require.Equal(t, 1, client.calls)
	require.Len(t, summary.Results, 1)
	require.Len(t, persisted, 1)
	require.Equal(t, allowance, persisted[0].ExecutionAudit.UncappedOutputTokenAllowance)
}

func TestRunCorpusRejectsUnattestedProductionRecordBeforeCall(t *testing.T) {
	record := contentRecord("privacy-missing", "Example")
	record.Label.Provenance = LabelProvenanceOperatorOutcome
	record.Label.HumanReviewProof = nil
	record.Label.ReviewerCount = 0
	record.PrivacyAttestation = nil
	client := &fakeCompleter{}

	_, err := RunCorpus(
		context.Background(),
		Corpus{Records: []CorpusRecord{record}},
		testOpenRouterSystem(TaskContentFilter),
		"key",
		client,
		RunOptions{
			MaxCostMicroUSD:      math.MaxInt64,
			Thresholds:           ProductionThresholds(),
			EvaluatorBuildSHA256: testEvaluatorBuildSHA,
			ExecutionBinding:     testExecutionBinding(),
		},
	)
	require.ErrorContains(t, err, "privacy_attestation: required")
	require.Zero(t, client.calls)
}

func TestRunCorpusRejectsLegacyProductionRerankCaptureBeforeCall(t *testing.T) {
	record := testRerankRecord(
		"legacy-rerank-v1",
		MatcherRerankExpected{AcceptableTMDBIDs: []int64{101}},
	)
	record.MatcherRerank.Input.EffectiveMediaType = ""
	corpus := mustCorpus(t, []CorpusRecord{record})
	system := SystemConfig{
		SystemID: "production-matcher", Provider: "openai",
		Model: "gpt-5.4-nano", PromptVersion: "llmmatch-v1",
		EvaluationLane: EvaluationLaneProductionFidelity,
		APIKind:        APIKindChat, OutputContract: OutputContractJSONObject,
		BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY",
		Tasks: []Task{TaskMatcherRerank}, NoThinkLocation: NoThinkLocationUser,
		NoThinkFormat: NoThinkFormatDoubleNewline, AppendNoThink: true,
		RequestTimeoutMS: 60_000,
	}
	client := &fakeCompleter{}

	_, err := RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		client,
		RunOptions{
			MaxCostMicroUSD:      math.MaxInt64,
			Thresholds:           ProductionThresholds(),
			EvaluatorBuildSHA256: testEvaluatorBuildSHA,
			ExecutionBinding: ExecutionBinding{
				ManifestSHA256: testManifestSHA,
			},
		},
	)
	require.ErrorContains(t, err, "llmmatch-chat-rerank-v2 effective_media_type")
	require.Zero(t, client.calls)

	shadow := system
	shadow.EvaluationLane = EvaluationLaneShadowFidelity
	if err := validateRunCorpusSystemContract(corpus, shadow); err == nil {
		t.Fatal("legacy rerank capture should also fail shadow-fidelity preflight")
	} else {
		require.ErrorContains(t, err, "llmmatch-chat-rerank-v2 effective_media_type")
	}
}

func TestRunCorpusRejectsMissingSourceTitleBeforeCall(t *testing.T) {
	record := testRerankRecord(
		"missing-source-title",
		MatcherRerankExpected{AcceptableTMDBIDs: []int64{101}},
	)
	record.MatcherRerank.Input.ParsedTitle = ""
	corpus := mustCorpus(t, []CorpusRecord{record})
	system := SystemConfig{
		SystemID: "production-matcher", Provider: "openai",
		Model: "gpt-5.4-nano", PromptVersion: "llmmatch-v1",
		EvaluationLane: EvaluationLaneProductionFidelity,
		APIKind:        APIKindChat, OutputContract: OutputContractJSONObject,
		BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY",
		Tasks: []Task{TaskMatcherRerank}, NoThinkLocation: NoThinkLocationUser,
		NoThinkFormat: NoThinkFormatDoubleNewline, AppendNoThink: true,
		RequestTimeoutMS: 60_000, RequireSourceTitle: true,
	}
	client := &fakeCompleter{}

	_, err := RunCorpus(
		context.Background(), corpus, system, "key", client,
		RunOptions{
			MaxCostMicroUSD:      math.MaxInt64,
			Thresholds:           ProductionThresholds(),
			EvaluatorBuildSHA256: testEvaluatorBuildSHA,
			ExecutionBinding:     ExecutionBinding{ManifestSHA256: testManifestSHA},
		},
	)
	require.ErrorContains(t, err, "requires parsed_title")
	require.Zero(t, client.calls)
}

func TestEstimateRequestCeilingUsesHighestInputTier(t *testing.T) {
	system := testOpenRouterSystem(TaskContentFilter)
	system.InputUSDPerMillion = 0.10
	system.CachedInputUSDPerMillion = 0.01
	system.CacheWriteUSDPerMillion = 10
	system.OutputUSDPerMillion = 1
	prompt := PromptRequest{
		System:              "s",
		User:                "u",
		Schema:              map[string]any{},
		MaxCompletionTokens: 5,
	}

	inputTokenCeiling := int64(len(prompt.System) + len(prompt.User) + 2 + 256)
	want := system.EstimateCostWithCacheWriteMicroUSD(
		inputTokenCeiling,
		0,
		inputTokenCeiling,
		int64(prompt.MaxCompletionTokens),
	)
	require.Equal(t, want, estimateRequestCeiling(system, prompt, 0))
}

func TestRunCorpusResumesWithoutPayingTwice(t *testing.T) {
	corpus, err := NewCorpus([]CorpusRecord{
		contentRecord("case-1", "First"),
		contentRecord("case-2", "Second"),
	})
	require.NoError(t, err)
	system := testOpenRouterSystem(TaskContentFilter)
	contractSHA, err := RequestContractSHA256(
		corpus.Records[0],
		system,
		ProductionThresholds(),
	)
	require.NoError(t, err)
	existing := ResultRecord{
		SchemaVersion: SchemaVersion, CorpusSHA256: corpus.SHA256,
		RequestContractSHA256: contractSHA,
		EvaluatorBuildSHA256:  testEvaluatorBuildSHA,
		ExecutionAudit:        testOpenRouterExecutionAudit(),
		CaseID:                "case-1", Task: TaskContentFilter, System: system.Descriptor(),
		Status: ResultStatusOK,
		ContentFilter: &ContentFilterResult{
			Action: ContentFilterActionKeep, IsEnglish: boolPointer(true),
			Confidence: 0.9,
		},
	}
	client := &fakeCompleter{
		reply: Completion{
			Text: []byte(`{"is_english":true,"confidence":0.9,"reason":"clear"}`),
			Usage: Usage{
				RequestID: "new", InputTokens: 10, OutputTokens: 5,
				CostMicroUSD: 2, CostSource: CostSourceProviderReported,
			},
			ReturnedModel:    system.Model,
			ReturnedProvider: "Test Provider",
			RouteProof:       RouteProofRouterMetadata,
		},
	}

	summary, err := RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		client,
		RunOptions{
			MaxCostMicroUSD:      math.MaxInt64,
			Thresholds:           ProductionThresholds(),
			EvaluatorBuildSHA256: testEvaluatorBuildSHA,
			ExecutionBinding:     testExecutionBinding(),
			Existing:             []ResultRecord{existing},
		},
	)
	require.NoError(t, err)
	require.Equal(t, 1, summary.Skipped)
	require.Equal(t, 1, summary.Completed)
	require.Equal(t, 1, client.calls)
	require.Len(t, summary.Results, 2)
	require.Nil(t, summary.Results[0].RequestTiming, "legacy timing absence remains readable")
	require.NotNil(t, summary.Results[1].RequestTiming)
}

func TestRunCorpusStampsAndEnforcesCampaignRunBinding(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		contentRecord("campaign-case", "Campaign Example"),
	})
	system := testOpenRouterSystem(TaskContentFilter)
	plan := testCampaignPlan()
	campaign, err := plan.RunBinding(
		strings.Repeat("9", 64),
		"run-candidate-contentfilter",
	)
	require.NoError(t, err)
	campaign.SystemID = system.SystemID
	campaign.SystemManifestSHA256 = testManifestSHA
	campaign.Corpus.SHA256 = corpus.SHA256
	campaign.RouteSnapshot.SHA256 = testRouteSnapshotSHA

	binding := testExecutionBinding()
	binding.Campaign = &campaign
	client := &fakeCompleter{reply: Completion{
		Text:             []byte(`{"is_english":true,"confidence":0.9,"reason":"clear"}`),
		ReturnedModel:    system.Model,
		ReturnedProvider: binding.ExpectedProvider,
		RouteProof:       RouteProofRouterMetadata,
		Usage: Usage{
			RequestID:          "campaign-request",
			InputTokens:        10,
			OutputTokens:       5,
			CostMicroUSD:       2,
			CostSource:         CostSourceProviderReported,
			AccountingComplete: true,
		},
	}}

	summary, err := RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		client,
		RunOptions{
			MaxCostMicroUSD:      campaign.CostCapMicroUSD,
			Thresholds:           ProductionThresholds(),
			EvaluatorBuildSHA256: testEvaluatorBuildSHA,
			ExecutionBinding:     binding,
		},
	)
	require.NoError(t, err)
	require.Len(t, summary.Results, 1)
	stamped := summary.Results[0].ExecutionAudit.Campaign
	require.NotNil(t, stamped)
	require.Equal(t, campaign, *stamped)
	require.NotSame(t, binding.Campaign, stamped)
	require.NotSame(t, binding.Campaign.RouteSnapshot, stamped.RouteSnapshot)

	tamperedBinding := binding
	tamperedCampaign := campaign
	tamperedCampaign.CampaignSHA256 = strings.Repeat("8", 64)
	tamperedBinding.Campaign = &tamperedCampaign
	_, err = RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		client,
		RunOptions{
			MaxCostMicroUSD:      tamperedCampaign.CostCapMicroUSD,
			Thresholds:           ProductionThresholds(),
			EvaluatorBuildSHA256: testEvaluatorBuildSHA,
			ExecutionBinding:     tamperedBinding,
			Existing:             summary.Results,
		},
	)
	require.ErrorContains(t, err, "different campaign run binding")
}

func TestRunCorpusChargesBillableProviderFailure(t *testing.T) {
	corpus, err := NewCorpus([]CorpusRecord{contentRecord("case-1", "Example")})
	require.NoError(t, err)
	system := testOpenRouterSystem(TaskContentFilter)
	client := &fakeCompleter{
		reply: Completion{
			Usage: Usage{
				RequestID:    "charged-failure",
				InputTokens:  25,
				OutputTokens: 10,
				CostMicroUSD: 17,
				CostSource:   CostSourceProviderReported,
			},
			ReturnedModel:    system.Model,
			ReturnedProvider: "Test Provider",
			RouteProof:       RouteProofRouterMetadata,
		},
		err: &CallError{Code: "empty_content"},
	}

	summary, err := RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		client,
		RunOptions{
			MaxCostMicroUSD:      math.MaxInt64,
			Thresholds:           ProductionThresholds(),
			EvaluatorBuildSHA256: testEvaluatorBuildSHA,
			ExecutionBinding:     testExecutionBinding(),
		},
	)
	require.NoError(t, err)
	require.Equal(t, int64(17), summary.CostMicroUSD)
	require.Len(t, summary.Results, 1)
	require.Equal(t, ResultStatusError, summary.Results[0].Status)
	require.Equal(t, "empty_content", summary.Results[0].ErrorCode)
	require.Equal(t, "charged-failure", summary.Results[0].Usage.RequestID)
	require.Equal(t, int64(17), summary.Results[0].Usage.CostMicroUSD)
}

func TestRunCorpusRejectsResumeWhenRequestContractChanges(t *testing.T) {
	corpus, err := NewCorpus([]CorpusRecord{contentRecord("case-1", "Example")})
	require.NoError(t, err)
	system := testOpenRouterSystem(TaskContentFilter)
	thresholds := ProductionThresholds()
	existing := ParseCompletion(
		corpus.SHA256,
		corpus.Records[0],
		system,
		Completion{
			Text: []byte(`{"is_english":true,"confidence":0.9,"reason":"clear"}`),
		},
		thresholds,
	)
	existing.EvaluatorBuildSHA256 = testEvaluatorBuildSHA
	existing.ExecutionAudit = testOpenRouterExecutionAudit()

	tests := []struct {
		name       string
		system     SystemConfig
		thresholds DecisionThresholds
	}{
		{
			name: "provider endpoint",
			system: func() SystemConfig {
				changed := system
				changed.ProviderEndpoint = "changed/fp8"
				return changed
			}(),
			thresholds: thresholds,
		},
		{
			name:   "decision thresholds",
			system: system,
			thresholds: func() DecisionThresholds {
				changed := thresholds
				changed.ContentDropConfidence = 0.90
				return changed
			}(),
		},
		{
			name: "pricing",
			system: func() SystemConfig {
				changed := system
				changed.InputUSDPerMillion = 2
				return changed
			}(),
			thresholds: thresholds,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeCompleter{}
			_, err := RunCorpus(
				context.Background(),
				corpus,
				test.system,
				"key",
				client,
				RunOptions{
					MaxCostMicroUSD:      math.MaxInt64,
					Thresholds:           test.thresholds,
					EvaluatorBuildSHA256: testEvaluatorBuildSHA,
					ExecutionBinding:     testExecutionBinding(),
					Existing:             []ResultRecord{existing},
				},
			)
			require.ErrorContains(t, err, "different request contract")
			require.Zero(t, client.calls)
		})
	}
}

func TestRunCorpusRejectsResumeFromDifferentEvaluatorBuild(t *testing.T) {
	corpus, err := NewCorpus([]CorpusRecord{contentRecord("case-1", "Example")})
	require.NoError(t, err)
	system := testOpenRouterSystem(TaskContentFilter)
	existing := ParseCompletion(
		corpus.SHA256,
		corpus.Records[0],
		system,
		Completion{
			Text: []byte(`{"is_english":true,"confidence":0.9,"reason":"clear"}`),
		},
		ProductionThresholds(),
	)
	existing.EvaluatorBuildSHA256 = testEvaluatorBuildSHA
	existing.ExecutionAudit = testOpenRouterExecutionAudit()

	_, err = RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		&fakeCompleter{},
		RunOptions{
			MaxCostMicroUSD:      math.MaxInt64,
			Thresholds:           ProductionThresholds(),
			EvaluatorBuildSHA256: "2222222222222222222222222222222222222222222222222222222222222222",
			ExecutionBinding:     testExecutionBinding(),
			Existing:             []ResultRecord{existing},
		},
	)
	require.ErrorContains(t, err, "different evaluator build")
}

func TestRunCorpusRejectsExistingCostOverflow(t *testing.T) {
	corpus, err := NewCorpus([]CorpusRecord{
		contentRecord("case-1", "First"),
		contentRecord("case-2", "Second"),
	})
	require.NoError(t, err)
	system := testOpenRouterSystem(TaskContentFilter)
	results := make([]ResultRecord, 0, len(corpus.Records))
	for index, record := range corpus.Records {
		result := ParseCompletion(
			corpus.SHA256,
			record,
			system,
			Completion{
				Text: []byte(`{"is_english":true,"confidence":0.9,"reason":"clear"}`),
			},
			ProductionThresholds(),
		)
		result.EvaluatorBuildSHA256 = testEvaluatorBuildSHA
		result.ExecutionAudit = testOpenRouterExecutionAudit()
		result.Usage.RequestID = "request-" + record.CaseID
		if index == 0 {
			result.Usage.CostMicroUSD = math.MaxInt64
		} else {
			result.Usage.CostMicroUSD = 1
		}
		results = append(results, result)
	}

	_, err = RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		&fakeCompleter{},
		RunOptions{
			MaxCostMicroUSD:      math.MaxInt64,
			Thresholds:           ProductionThresholds(),
			EvaluatorBuildSHA256: testEvaluatorBuildSHA,
			ExecutionBinding:     testExecutionBinding(),
			Existing:             results,
		},
	)
	require.ErrorContains(t, err, "integer overflow")
}

func TestRunCorpusRejectsProjectedCostOverflowBeforeProviderCall(t *testing.T) {
	corpus, err := NewCorpus([]CorpusRecord{
		contentRecord("case-1", "First"),
		contentRecord("case-2", "Second"),
	})
	require.NoError(t, err)
	system := testOpenRouterSystem(TaskContentFilter)
	existing := ParseCompletion(
		corpus.SHA256,
		corpus.Records[0],
		system,
		Completion{
			Text: []byte(`{"is_english":true,"confidence":0.9,"reason":"clear"}`),
		},
		ProductionThresholds(),
	)
	existing.EvaluatorBuildSHA256 = testEvaluatorBuildSHA
	existing.ExecutionAudit = testOpenRouterExecutionAudit()
	existing.Usage.CostMicroUSD = math.MaxInt64
	existing.Usage.InputTokens = 1
	existing.Usage.OutputTokens = 1
	existing.Usage.AccountingComplete = true
	client := &fakeCompleter{}

	_, err = RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		client,
		RunOptions{
			MaxCostMicroUSD:      math.MaxInt64,
			Thresholds:           ProductionThresholds(),
			EvaluatorBuildSHA256: testEvaluatorBuildSHA,
			ExecutionBinding:     testExecutionBinding(),
			Existing:             []ResultRecord{existing},
		},
	)
	require.ErrorContains(t, err, "cost ceiling accounting: integer overflow")
	require.Zero(t, client.calls)
}

func TestRunCorpusLocksDirectAliasToFirstConcreteModelAcrossResume(t *testing.T) {
	corpus, err := NewCorpus([]CorpusRecord{
		contentRecord("case-1", "First"),
		contentRecord("case-2", "Second"),
	})
	require.NoError(t, err)
	system := testOpenRouterSystem(TaskContentFilter)
	system.Provider = "openai"
	system.Model = "gpt-5.4-nano"
	system.ProviderEndpoint = ""
	system.APIKeyEnv = "OPENAI_API_KEY"
	system.BaseURL = "https://api.openai.com/v1"
	system.ZDR = false
	binding := ExecutionBinding{ManifestSHA256: testManifestSHA}
	concreteModel := "gpt-5.4-nano-2026-07-15"

	existing := ParseCompletion(
		corpus.SHA256,
		corpus.Records[0],
		system,
		Completion{
			Text: []byte(`{"is_english":true,"confidence":0.9,"reason":"clear"}`),
		},
		ProductionThresholds(),
	)
	existing.EvaluatorBuildSHA256 = testEvaluatorBuildSHA
	existing.ExecutionAudit = ExecutionAudit{
		ManifestSHA256: testManifestSHA,
		Route: RouteAudit{
			ReturnedModel: concreteModel,
			Proof:         RouteProofDirectResponseModel,
			Verified:      true,
		},
	}

	client := &sequenceCompleter{replies: []Completion{{
		Text:          []byte(`{"is_english":true,"confidence":0.9,"reason":"clear"}`),
		ReturnedModel: "gpt-5.4-nano-2026-07-22",
		RouteProof:    RouteProofDirectResponseModel,
	}}}
	summary, err := RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		client,
		RunOptions{
			MaxCostMicroUSD:      math.MaxInt64,
			Thresholds:           ProductionThresholds(),
			EvaluatorBuildSHA256: testEvaluatorBuildSHA,
			ExecutionBinding:     binding,
			Existing:             []ResultRecord{existing},
		},
	)
	require.NoError(t, err)
	require.Equal(t, 1, client.calls)
	require.Len(t, summary.Results, 2)
	require.Equal(t, ResultStatusError, summary.Results[1].Status)
	require.Equal(t, "route_model_mismatch", summary.Results[1].ErrorCode)
	require.False(t, summary.Results[1].ExecutionAudit.Route.Verified)
	require.Equal(
		t,
		"gpt-5.4-nano-2026-07-22",
		summary.Results[1].ExecutionAudit.Route.ReturnedModel,
	)
}

func TestRunCorpusRejectsWrongFirstDirectModel(t *testing.T) {
	corpus, err := NewCorpus([]CorpusRecord{
		contentRecord("case-1", "Example"),
	})
	require.NoError(t, err)
	system := testOpenRouterSystem(TaskContentFilter)
	system.Provider = "openai"
	system.Model = "gpt-5.4-nano"
	system.ProviderEndpoint = ""
	system.APIKeyEnv = "OPENAI_API_KEY"
	system.BaseURL = "https://api.openai.com/v1"
	system.ZDR = false
	client := &fakeCompleter{reply: Completion{
		Text: []byte(
			`{"is_english":true,"confidence":0.9,"reason":"clear"}`,
		),
		ReturnedModel: "gpt-5.6-terra",
		RouteProof:    RouteProofDirectResponseModel,
	}}

	summary, err := RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		client,
		RunOptions{
			MaxCostMicroUSD:      math.MaxInt64,
			Thresholds:           ProductionThresholds(),
			EvaluatorBuildSHA256: testEvaluatorBuildSHA,
			ExecutionBinding: ExecutionBinding{
				ManifestSHA256: testManifestSHA,
			},
		},
	)
	require.NoError(t, err)
	require.Len(t, summary.Results, 1)
	require.Equal(t, ResultStatusError, summary.Results[0].Status)
	require.Equal(t, "route_model_mismatch", summary.Results[0].ErrorCode)
	require.False(t, summary.Results[0].ExecutionAudit.Route.Verified)
}

func TestDirectModelMatchesOnlyExactOrDatedResolution(t *testing.T) {
	require.True(t, directModelMatches(
		"gpt-5.4-nano",
		"gpt-5.4-nano-2026-03-17",
	))
	require.True(t, directModelMatches("gpt-5.6-luna", "GPT-5.6-LUNA"))
	require.False(t, directModelMatches(
		"gpt-5.4-nano",
		"gpt-5.4-nano-not-a-date",
	))
	require.False(t, directModelMatches("gpt-5.4-nano", "gpt-5.6-terra"))
}

func TestRunCorpusBindsOpenRouterAliasToSnapshotResolvedModel(t *testing.T) {
	corpus, err := NewCorpus([]CorpusRecord{
		contentRecord("case-1", "Example"),
	})
	require.NoError(t, err)
	system := testOpenRouterSystem(TaskContentFilter)
	system.Model = "inclusionai/ling-3.0-flash:free"
	resolved := "inclusionai/ling-3.0-flash-20260723:free"
	binding := testExecutionBinding()
	binding.ExpectedModel = resolved
	client := &fakeCompleter{
		reply: Completion{
			Text: []byte(
				`{"is_english":true,"confidence":0.9,"reason":"clear"}`,
			),
			ReturnedModel:    strings.ToUpper(resolved),
			ReturnedProvider: "Test Provider",
			RouteProof:       RouteProofRouterMetadata,
		},
	}

	summary, err := RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		client,
		RunOptions{
			MaxCostMicroUSD:      math.MaxInt64,
			Thresholds:           ProductionThresholds(),
			EvaluatorBuildSHA256: testEvaluatorBuildSHA,
			ExecutionBinding:     binding,
		},
	)
	require.NoError(t, err)
	require.Len(t, summary.Results, 1)
	require.Equal(t, ResultStatusOK, summary.Results[0].Status)
	require.True(t, summary.Results[0].ExecutionAudit.Route.Verified)
	require.Equal(
		t,
		strings.ToUpper(resolved),
		summary.Results[0].ExecutionAudit.Route.ReturnedModel,
	)

	client.reply.ReturnedModel = system.Model
	summary, err = RunCorpus(
		context.Background(),
		corpus,
		system,
		"key",
		client,
		RunOptions{
			MaxCostMicroUSD:      math.MaxInt64,
			Thresholds:           ProductionThresholds(),
			EvaluatorBuildSHA256: testEvaluatorBuildSHA,
			ExecutionBinding:     binding,
		},
	)
	require.NoError(t, err)
	require.Equal(t, ResultStatusError, summary.Results[0].Status)
	require.Equal(t, "route_model_mismatch", summary.Results[0].ErrorCode)
}

func contentRecord(id, title string) CorpusRecord {
	record := testRecord(id, TaskContentFilter)
	record.SliceIDs = []string{"natural"}
	record.GroupID = id
	record.ContentFilter = &ContentFilterCase{
		Input:    ContentFilterInput{Title: title},
		Expected: ContentFilterExpected{Language: LanguageEnglish},
	}
	return record
}

func testOpenRouterSystem(tasks ...Task) SystemConfig {
	return SystemConfig{
		SystemID: "test", Provider: "openrouter", Model: "test/model",
		PromptVersion: "v1", EvaluationLane: EvaluationLaneNormalizedStrict,
		APIKind: APIKindChat, OutputContract: OutputContractJSONSchema,
		BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY",
		ProviderEndpoint: "test/fp8", Tasks: tasks,
		InputUSDPerMillion: 1, OutputUSDPerMillion: 1,
		StructuredOutputs: true, ZDR: true, NoThinkLocation: NoThinkLocationNone,
		RequestTimeoutMS: 60_000,
	}
}

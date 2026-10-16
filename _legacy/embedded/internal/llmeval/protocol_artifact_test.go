package llmeval

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

type checkedProtocolSmoke struct {
	Status string `json:"status"`
	Corpus struct {
		SHA256        string `json:"sha256"`
		Cases         int    `json:"cases"`
		CasesPerTask  int    `json:"cases_per_task"`
		LabelStrength string `json:"label_strength"`
	} `json:"corpus"`
	Systems []struct {
		SystemID string         `json:"system_id"`
		Lane     EvaluationLane `json:"lane"`
		Tasks    []struct {
			Task          Task    `json:"task"`
			Cases         int     `json:"cases"`
			Correct       int     `json:"correct"`
			SchemaErrors  int     `json:"schema_errors"`
			RuntimeErrors int     `json:"runtime_errors"`
			CostUSD       float64 `json:"cost_usd"`
		} `json:"tasks"`
	} `json:"systems"`
	ResolvedRouteValidation struct {
		Scope               string  `json:"scope"`
		SystemID            string  `json:"system_id"`
		Tasks               int     `json:"tasks"`
		Cases               int     `json:"cases"`
		Status              string  `json:"status"`
		Correct             int     `json:"correct"`
		RouteVerifiedCases  int     `json:"route_verified_cases"`
		RequestedModel      string  `json:"requested_model"`
		ReturnedModel       string  `json:"returned_model"`
		ReturnedProvider    string  `json:"returned_provider"`
		RouteProof          string  `json:"route_proof"`
		RouteVerified       bool    `json:"route_verified"`
		RouteSnapshotSHA256 string  `json:"route_snapshot_sha256"`
		InputTokens         int64   `json:"input_tokens"`
		CachedInputTokens   int64   `json:"cached_input_tokens"`
		OutputTokens        int64   `json:"output_tokens"`
		CostUSD             float64 `json:"cost_usd"`
	} `json:"resolved_route_validation"`
}

type checkedRouteSnapshotSummary struct {
	Status         string `json:"status"`
	ManifestSHA256 string `json:"manifest_sha256"`
	ZDRFeedURL     string `json:"zdr_feed_url"`
	ZDRFeedSHA256  string `json:"zdr_feed_response_sha256"`
	Routes         []struct {
		SystemID                    string  `json:"system_id"`
		ModelEndpointsURL           string  `json:"model_endpoints_url"`
		ProviderEndpoint            string  `json:"provider_endpoint"`
		QuantizationMetadata        string  `json:"quantization_metadata"`
		ResolvedModel               string  `json:"resolved_model"`
		ModelResponseSHA256         string  `json:"model_response_sha256"`
		MatchedEndpoints            int     `json:"matched_endpoints"`
		ZDRListed                   bool    `json:"zdr_listed"`
		InputUSDPerMillion          float64 `json:"input_usd_per_million"`
		CachedInputUSDPerMillion    float64 `json:"cached_input_usd_per_million"`
		OutputUSDPerMillion         float64 `json:"output_usd_per_million"`
		SupportsResponseFormat      bool    `json:"supports_response_format"`
		AdvertisesStructuredOutputs bool    `json:"advertises_structured_outputs"`
	} `json:"routes"`
}

func TestCheckedInProtocolSmokeIsExplicitlyNonPromotional(t *testing.T) {
	raw, err := os.ReadFile(opsFixture(
		t,
		"../../ops/llm-eval/protocol-smoke-observation-2026-07-24.json",
	))
	require.NoError(t, err)

	var observation checkedProtocolSmoke
	require.NoError(t, json.Unmarshal(raw, &observation))
	require.Equal(t, "contract_smoke_not_quality_benchmark", observation.Status)
	require.Equal(t, "synthetic", observation.Corpus.LabelStrength)
	require.Equal(t, 8, observation.Corpus.Cases)
	require.Equal(t, 2, observation.Corpus.CasesPerTask)

	corpusFile, err := os.Open(opsFixture(t, "../../ops/llm-eval/protocol-smoke.jsonl"))
	require.NoError(t, err)
	defer corpusFile.Close()
	corpus, err := ReadCorpus(corpusFile)
	require.NoError(t, err)
	require.Equal(t, corpus.SHA256, observation.Corpus.SHA256)
	require.Len(t, corpus.Records, observation.Corpus.Cases)

	// The checked-in fixture has to survive the same gates a real scoring run
	// applies, or the documented protocol-smoke procedure breaks while the
	// suite stays green — which is exactly how the cross-tier fixture shipped.
	require.NoError(t, ValidateGoldCorpus(corpus))
	require.NoError(t, rejectCrossTierJunkPurge(corpus.Records))

	manifest, err := LoadSystemManifest(opsFixture(t, "../../ops/llm-eval/first-wave-models.json"))
	require.NoError(t, err)
	systems := make(map[string]SystemConfig, len(manifest.Systems))
	for _, system := range manifest.Systems {
		systems[system.SystemID] = system
	}

	totalCases := 0
	totalCorrect := 0
	for _, observedSystem := range observation.Systems {
		system, exists := systems[observedSystem.SystemID]
		require.True(t, exists, observedSystem.SystemID)
		require.Equal(t, system.EvaluationLane, observedSystem.Lane)
		for _, task := range observedSystem.Tasks {
			require.True(t, system.SupportsTask(task.Task), observedSystem.SystemID)
			require.Equal(t, observation.Corpus.CasesPerTask, task.Cases)
			require.LessOrEqual(t, task.Correct, task.Cases)
			require.Zero(t, task.SchemaErrors)
			require.Zero(t, task.RuntimeErrors)
			require.GreaterOrEqual(t, task.CostUSD, 0.0)
			totalCases += task.Cases
			totalCorrect += task.Correct
		}
	}
	require.Equal(t, 24, totalCases)
	require.Equal(t, 22, totalCorrect)

	routeValidation := observation.ResolvedRouteValidation
	require.Equal(
		t,
		"full_eight_case_transport_parse_score_and_route_binding_check",
		routeValidation.Scope,
	)
	require.Equal(t, "openrouter-ling-3-0-flash-free-novita", routeValidation.SystemID)
	require.Equal(t, 4, routeValidation.Tasks)
	require.Equal(t, 8, routeValidation.Cases)
	require.Equal(t, "ok", routeValidation.Status)
	require.Equal(t, routeValidation.Cases, routeValidation.Correct)
	require.Equal(t, routeValidation.Cases, routeValidation.RouteVerifiedCases)
	require.Equal(t, "inclusionai/ling-3.0-flash:free", routeValidation.RequestedModel)
	require.Equal(
		t,
		"inclusionai/ling-3.0-flash-20260723:free",
		routeValidation.ReturnedModel,
	)
	require.Equal(t, "Novita", routeValidation.ReturnedProvider)
	require.Equal(t, "router_metadata", routeValidation.RouteProof)
	require.True(t, routeValidation.RouteVerified)
	require.Regexp(t, `^[a-f0-9]{64}$`, routeValidation.RouteSnapshotSHA256)
	require.Positive(t, routeValidation.InputTokens)
	require.GreaterOrEqual(t, routeValidation.CachedInputTokens, int64(0))
	require.LessOrEqual(t, routeValidation.CachedInputTokens, routeValidation.InputTokens)
	require.Positive(t, routeValidation.OutputTokens)
	require.Zero(t, routeValidation.CostUSD)
}

func TestCheckedInRouteSummaryCoversEveryOpenRouterSystem(t *testing.T) {
	raw, err := os.ReadFile(opsFixture(
		t,
		"../../ops/llm-eval/route-snapshot-summary-2026-07-24.json",
	))
	require.NoError(t, err)

	var snapshot checkedRouteSnapshotSummary
	require.NoError(t, json.Unmarshal(raw, &snapshot))
	require.Equal(t, "point_in_time_observation", snapshot.Status)
	require.Equal(t, "https://openrouter.ai/api/v1/endpoints/zdr", snapshot.ZDRFeedURL)
	require.Regexp(t, `^[a-f0-9]{64}$`, snapshot.ZDRFeedSHA256)

	manifestRaw, err := os.ReadFile(opsFixture(t, "../../ops/llm-eval/first-wave-models.json"))
	require.NoError(t, err)
	require.Equal(t, sha256Hex(manifestRaw), snapshot.ManifestSHA256)
	manifest, err := LoadSystemManifest(opsFixture(t, "../../ops/llm-eval/first-wave-models.json"))
	require.NoError(t, err)
	openRouter := make(map[string]SystemConfig)
	for _, system := range manifest.Systems {
		if system.Provider == "openrouter" {
			openRouter[system.SystemID] = system
		}
	}
	require.Len(t, snapshot.Routes, len(openRouter))

	seen := make(map[string]struct{}, len(snapshot.Routes))
	for _, route := range snapshot.Routes {
		system, exists := openRouter[route.SystemID]
		require.True(t, exists, route.SystemID)
		require.Equal(t, system.ProviderEndpoint, route.ProviderEndpoint)
		require.Equal(
			t,
			"https://openrouter.ai/api/v1/models/"+system.Model+"/endpoints",
			route.ModelEndpointsURL,
		)
		require.NotEmpty(t, route.ResolvedModel)
		require.Regexp(t, `^[a-f0-9]{64}$`, route.ModelResponseSHA256)
		require.NotEmpty(t, route.QuantizationMetadata)
		require.Equal(t, 1, route.MatchedEndpoints)
		require.Equal(t, system.ZDR, route.ZDRListed)
		require.Equal(t, system.InputUSDPerMillion, route.InputUSDPerMillion)
		require.Equal(
			t,
			system.CachedInputUSDPerMillion,
			route.CachedInputUSDPerMillion,
		)
		require.Equal(t, system.OutputUSDPerMillion, route.OutputUSDPerMillion)
		if system.APIKind == APIKindChat &&
			system.OutputContract == OutputContractJSONSchema {
			require.True(
				t,
				route.SupportsResponseFormat,
				"%s cannot accept the configured JSON Schema request",
				route.SystemID,
			)
		}
		_, duplicate := seen[route.SystemID]
		require.False(t, duplicate, route.SystemID)
		seen[route.SystemID] = struct{}{}
	}

	var schemaRequestedWithoutNativeAdvertisement []string
	for _, route := range snapshot.Routes {
		system := openRouter[route.SystemID]
		if system.APIKind == APIKindChat &&
			system.OutputContract == OutputContractJSONSchema &&
			!route.AdvertisesStructuredOutputs {
			schemaRequestedWithoutNativeAdvertisement = append(
				schemaRequestedWithoutNativeAdvertisement,
				route.SystemID,
			)
		}
	}
	require.ElementsMatch(
		t,
		[]string{
			"openrouter-llama-3-1-8b-deepinfra-fp8",
			"openrouter-mistral-nemo-deepinfra-fp8",
		},
		schemaRequestedWithoutNativeAdvertisement,
	)
}

// TestCheckedInCorpusPlanV2IsTheJunkPurgeDispositionPlan pins the plan revision
// that carries the disposition contract. v1 is kept and still validated by the
// test below: the deterministic selection split hashes plan_id, so v1 must
// never be edited in place, only superseded.
func TestCheckedInCorpusPlanV2IsTheJunkPurgeDispositionPlan(t *testing.T) {
	file, err := os.Open(opsFixture(t, "../../ops/llm-eval/corpus-plan-v2.json"))
	require.NoError(t, err)
	defer file.Close()

	plan, planSHA256, err := ReadCorpusPlan(file)
	require.NoError(t, err)
	require.Equal(
		t,
		"d221c20363c47b0105db6e2107ff5b6d941396ca878dfb889fddab8db7be8950",
		planSHA256,
	)
	require.Equal(t, "bitagent-llm-corpus-v2-2026-07-26", plan.PlanID)
	require.Equal(t, "bitagent-llm-corpus-v1-2026-07-24", plan.SupersedesPlanID)
	require.NotEmpty(t, plan.SupersedesReason)
	require.Len(t, plan.Tasks, len(orderedTasks))

	var junk *CorpusPlanTask
	for i := range plan.Tasks {
		if plan.Tasks[i].Task == TaskJunkPurge {
			junk = &plan.Tasks[i]
		}
	}
	require.NotNil(t, junk)

	// Quotas name dispositions, not provenance.
	require.Contains(t, junk.RequiredUnionCounts, "human_verified_keep_worthy")
	require.Contains(t, junk.RequiredUnionCounts, "representative_delete")
	require.NotContains(t, junk.RequiredUnionCounts, "human_verified_real_movie_or_tv")
	require.NotContains(t, junk.RequiredUnionCounts, "representative_junk")

	// Tiers are declared up front, not bolted on after adjudication starts.
	for _, tier := range []string{
		string(GoldTierAResolvableKeep),
		string(GoldTierBHardKeep),
		string(GoldTierCRepresentativeDelete),
	} {
		require.Contains(t, junk.GoldTiers, tier)
		require.NotEmpty(t, junk.GoldTiers[tier].Bounds)
	}
	require.Contains(t, junk.TierReportingRule, "never blended")
	require.Contains(t, junk.HarmDenominator, "keep")

	// The reviewer answers a class question under the v2 policy.
	require.Contains(t, junk.GoldContract, "CONTENT CLASS")
	require.Contains(t, junk.GoldContract, "content-junk-review-v2")
	require.NotContains(t, junk.GoldContract, "real_mangled")
	require.NotContains(t, junk.GoldContract, "real_absent")
}

func TestCheckedInCorpusPlanHasEveryTaskAndFailsClosed(t *testing.T) {
	file, err := os.Open(opsFixture(t, "../../ops/llm-eval/corpus-plan-v1.json"))
	require.NoError(t, err)
	defer file.Close()

	plan, planSHA256, err := ReadCorpusPlan(file)
	require.NoError(t, err)
	require.Equal(
		t,
		"ea7a58aeeff2579f2f6f328a7147aadf1db64fd0cc8c22671c6ade3849cf4ece",
		planSHA256,
	)
	require.Equal(
		t,
		"validated_evaluator_audited_capture_export_and_gold_workflow_data_collection_pending",
		plan.Status,
	)
	require.False(t, plan.Executable)
	require.NotEmpty(t, plan.NonExecutableReason)
	require.Equal(t, "group_id", plan.Selection.Unit)
	require.Equal(
		t,
		CorpusSplitAlgorithmGroupSHA256V2,
		plan.Selection.AlgorithmID,
	)
	require.Equal(
		t,
		CorpusPrimarySuiteSlicePrefix,
		plan.Selection.PrimarySuiteSlicePrefix,
	)
	require.Contains(t, plan.Selection.SplitAlgorithm, "SHA-256")
	require.Len(t, plan.Tasks, len(orderedTasks))

	expectedHoldout := map[Task]int{
		TaskMatcherExtract: 4000,
		TaskMatcherRerank:  4000,
		TaskContentFilter:  4000,
		TaskJunkPurge:      5000,
	}
	expectedMaximumReview := map[Task]int{
		TaskMatcherExtract: 7240,
		TaskMatcherRerank:  7240,
		TaskContentFilter:  8540,
		TaskJunkPurge:      9740,
	}
	seen := make(map[Task]struct{}, len(plan.Tasks))
	totalMaximumReview := 0
	for _, task := range plan.Tasks {
		_, duplicate := seen[task.Task]
		require.False(t, duplicate, task.Task)
		seen[task.Task] = struct{}{}
		require.Equal(t, 1200, task.DevelopmentTarget, task.Task)
		require.Equal(t, expectedHoldout[task.Task], task.HoldoutTarget, task.Task)
		require.NotEmpty(t, task.GoldContract, task.Task)
		require.NotEmpty(t, task.ExportRequirement, task.Task)

		allocationTotal := 0
		for _, count := range task.HoldoutAllocation {
			allocationTotal += count
		}
		require.Equal(t, task.HoldoutTarget, allocationTotal, task.Task)
		taskMaximumReview := 0
		developmentAllocationTotal := 0
		for suite, count := range task.DevelopmentAllocation {
			require.Equal(t, 600, count, task.Task)
			require.Equal(
				t,
				120,
				task.DevelopmentReplacementReserve[suite],
				task.Task,
			)
			developmentAllocationTotal += count
			taskMaximumReview += count +
				task.DevelopmentReplacementReserve[suite]
		}
		require.Equal(
			t,
			task.DevelopmentTarget,
			developmentAllocationTotal,
			task.Task,
		)
		require.Equal(
			t,
			map[string]int{
				GoldQuotaNaturalPrimaryHarmGroups: 1197,
				GoldQuotaNaturalTaskSuccessGroups: 299,
				GoldQuotaSafetyPrimaryHarmGroups:  1197,
				GoldQuotaSafetyTaskSuccessGroups:  299,
			},
			task.RequiredGoldEligibleGroups,
			task.Task,
		)
		for suite, maximum := range task.ReserveMaximum {
			require.GreaterOrEqual(
				t,
				maximum,
				task.HoldoutAllocation[suite]+
					task.ReplacementReserve[suite],
				task.Task,
			)
			taskMaximumReview += maximum
		}
		require.Equal(
			t,
			expectedMaximumReview[task.Task],
			taskMaximumReview,
			task.Task,
		)
		totalMaximumReview += taskMaximumReview
	}
	require.Equal(t, 32760, totalMaximumReview)
	for _, task := range orderedTasks {
		_, exists := seen[task]
		require.True(t, exists, task)
	}

	require.True(t, plan.Readiness.SamplingContract)
	require.True(t, plan.Readiness.MachinePlanValidator)
	require.True(t, plan.Readiness.DeterministicSplitFreeze)
	require.True(t, plan.Readiness.StrictCorpusSchema)
	require.True(t, plan.Readiness.BlindedTwoReviewerWorkflow)
	require.True(t, plan.Readiness.IndependentAdjudicationWorkflow)
	require.True(t, plan.Readiness.GoldPromotionWorkflow)
	require.True(t, plan.Readiness.PostReviewReplacementWorkflow)
	require.True(t, plan.Readiness.ProductionSourceExporter)
	require.False(t, plan.Readiness.ProductionCorpusFrozen)
	require.False(t, plan.Readiness.IndependentReviewComplete)
	require.False(t, plan.Readiness.PaidOpenRouterComparisonComplete)
	require.False(t, plan.Readiness.PromotionReady)
}

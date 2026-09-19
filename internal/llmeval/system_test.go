package llmeval

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadSystemManifestRejectsAmbiguousOrOversizedJSON(t *testing.T) {
	raw, err := os.ReadFile(opsFixture(t, "../../ops/llm-eval/first-wave-models.json"))
	require.NoError(t, err)

	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}

	duplicate := strings.Replace(
		string(raw),
		`"schema_version": 1`,
		`"schema_version": 1, "schema_version": 1`,
		1,
	)
	_, err = LoadSystemManifest(write("duplicate.json", duplicate))
	require.ErrorContains(t, err, "duplicate object key")

	_, err = LoadSystemManifest(write("trailing.json", string(raw)+"{}"))
	require.ErrorContains(t, err, "multiple")

	_, err = LoadSystemManifest(write(
		"oversized.json",
		strings.Repeat(" ", maxSystemManifestBytes+1),
	))
	require.ErrorContains(t, err, "exceeds")
}

func TestHistoricalBakeoffManifestCompatibilityIsExactAndReadOnly(t *testing.T) {
	path := opsFixture(t, "../../ops/llm-eval/open-model-feasibility-v2-2026-07-28.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	_, err = LoadSystemManifest(path)
	require.ErrorContains(t, err, "explicit recognized quantization")

	manifest, digest, err :=
		DecodeOpenModelBakeoffRegistrySystemManifest(raw)
	require.NoError(t, err)
	require.Equal(t, legacyOpenModelFeasibilityManifestSHA256, digest)

	t.Run("changed bytes lose the legacy exception", func(t *testing.T) {
		changed := append(append([]byte(nil), raw...), ' ')
		_, changedDigest, err :=
			DecodeOpenModelBakeoffRegistrySystemManifest(changed)
		require.Empty(t, changedDigest)
		require.ErrorContains(t, err, "explicit recognized quantization")
	})

	t.Run("changed known row is rejected", func(t *testing.T) {
		changed := manifest
		changed.Systems = append([]SystemConfig(nil), manifest.Systems...)
		for i := range changed.Systems {
			if changed.Systems[i].SystemID ==
				"or-nano-azure-content-shadow-20260728" {
				changed.Systems[i].Variant = "changed-history"
				break
			}
		}
		require.ErrorContains(
			t,
			validateOpenModelBakeoffRegistrySystemManifest(
				changed,
				legacyOpenModelFeasibilityManifestSHA256,
			),
			"parsed content changed from its immutable historical contract",
		)
	})

	t.Run("otherwise valid nonlegacy change is rejected", func(t *testing.T) {
		changed := manifest
		changed.Systems = append([]SystemConfig(nil), manifest.Systems...)
		changed.Systems[1].Variant += "-changed"
		require.NoError(t, changed.Systems[1].Validate())
		require.ErrorContains(
			t,
			validateOpenModelBakeoffRegistrySystemManifest(
				changed,
				legacyOpenModelFeasibilityManifestSHA256,
			),
			"parsed content changed from its immutable historical contract",
		)
	})

	t.Run("otherwise valid pricing note change is rejected", func(t *testing.T) {
		changed := manifest
		changed.PricingNotes = append(
			[]PricingNote(nil),
			manifest.PricingNotes...,
		)
		changed.PricingNotes[0].Note += " changed"
		require.ErrorContains(
			t,
			validateOpenModelBakeoffRegistrySystemManifest(
				changed,
				legacyOpenModelFeasibilityManifestSHA256,
			),
			"parsed content changed from its immutable historical contract",
		)
	})

	t.Run("extra provider-only row is rejected", func(t *testing.T) {
		changed := manifest
		changed.Systems = append([]SystemConfig(nil), manifest.Systems...)
		var extra SystemConfig
		for _, system := range manifest.Systems {
			if system.SystemID == legacyNanoAzureContentShadowSystemID {
				extra = system
				break
			}
		}
		require.Equal(t, legacyNanoAzureContentShadowSystemID, extra.SystemID)
		extra.SystemID = "unexpected-provider-only-shadow"
		changed.Systems = append(changed.Systems, extra)
		require.ErrorContains(
			t,
			validateOpenModelBakeoffRegistrySystemManifest(
				changed,
				legacyOpenModelFeasibilityManifestSHA256,
			),
			"parsed content changed from its immutable historical contract",
		)
	})

	t.Run("claimed legacy digest cannot relax another manifest", func(t *testing.T) {
		changed := manifest
		changed.Systems = append([]SystemConfig(nil), manifest.Systems...)
		changed.Systems = changed.Systems[1:]
		require.ErrorContains(
			t,
			validateOpenModelBakeoffRegistrySystemManifest(
				changed,
				legacyOpenModelFeasibilityManifestSHA256,
			),
			"parsed content changed from its immutable historical contract",
		)
	})
}

func TestSystemConfigRejectsLocalRoutes(t *testing.T) {
	system := SystemConfig{
		SystemID: "local", Provider: "openrouter", Model: "test/model",
		PromptVersion: "v1", EvaluationLane: EvaluationLaneNormalizedStrict,
		APIKind: APIKindChat, OutputContract: OutputContractJSONSchema,
		BaseURL: "http://127.0.0.1:11434/v1", APIKeyEnv: "OPENROUTER_API_KEY",
		ProviderEndpoint: "test/fp8", Tasks: []Task{TaskJunkPurge},
		StructuredOutputs: true, ZDR: true, NoThinkLocation: NoThinkLocationNone,
	}
	require.ErrorContains(t, system.Validate(), "absolute https")
}

func TestSystemConfigRequiresPinnedPrivateOpenRouterRoute(t *testing.T) {
	system := SystemConfig{
		SystemID: "nemo", Provider: "openrouter", Model: "mistralai/mistral-nemo",
		PromptVersion: "v1", EvaluationLane: EvaluationLaneNormalizedStrict,
		APIKind: APIKindChat, OutputContract: OutputContractJSONSchema,
		BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY",
		Tasks: []Task{TaskMatcherExtract}, StructuredOutputs: true,
		NoThinkLocation: NoThinkLocationNone,
	}
	require.ErrorContains(t, system.Validate(), "pin provider_endpoint")
	system.ProviderEndpoint = "dekallm/fp8"
	require.ErrorContains(t, system.Validate(), "enforce zdr outside")
	system.ZDR = true
	require.NoError(t, system.Validate())
}

func TestEstimateCostMicroUSD(t *testing.T) {
	system := SystemConfig{
		InputUSDPerMillion:       1,
		CachedInputUSDPerMillion: 0.1,
		OutputUSDPerMillion:      5,
		BillingMultiplier:        1.055,
	}
	// 900 regular input + 100 cached + 100 output = $0.00141 before fee.
	require.Equal(t, int64(1488), system.EstimateCostMicroUSD(1000, 100, 100))
}

func TestEstimateCostPreservesPositiveSubMicroCharge(t *testing.T) {
	system := SystemConfig{InputUSDPerMillion: 0.01}
	require.Equal(t, int64(1), system.EstimateCostMicroUSD(1, 0, 0))
}

func TestEstimateCostWithCacheWriteMicroUSD(t *testing.T) {
	system := SystemConfig{
		InputUSDPerMillion:       1,
		CachedInputUSDPerMillion: 0.1,
		CacheWriteUSDPerMillion:  1.25,
		OutputUSDPerMillion:      5,
	}
	// 700 regular + 100 cache-read + 200 cache-write + 100 output = $0.00146.
	require.Equal(
		t,
		int64(1460),
		system.EstimateCostWithCacheWriteMicroUSD(1000, 100, 200, 100),
	)
}

func TestSystemConfigRejectsNonFinitePrices(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*SystemConfig)
	}{
		{
			name: "nan input price",
			mutate: func(system *SystemConfig) {
				system.InputUSDPerMillion = math.NaN()
			},
		},
		{
			name: "infinite output price",
			mutate: func(system *SystemConfig) {
				system.OutputUSDPerMillion = math.Inf(1)
			},
		},
		{
			name: "infinite billing multiplier",
			mutate: func(system *SystemConfig) {
				system.BillingMultiplier = math.Inf(1)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			system := testOpenRouterSystem(TaskContentFilter)
			test.mutate(&system)
			require.ErrorContains(t, system.Validate(), "finite and non-negative")
		})
	}
}

func TestEstimateCostSaturatesInsteadOfWrapping(t *testing.T) {
	system := SystemConfig{
		InputUSDPerMillion:  math.MaxFloat64,
		OutputUSDPerMillion: math.MaxFloat64,
		BillingMultiplier:   math.MaxFloat64,
	}
	require.Equal(
		t,
		int64(math.MaxInt64),
		system.EstimateCostWithCacheWriteMicroUSD(1, 0, 0, 1),
	)
}

func TestProductionFidelityContractIsExplicit(t *testing.T) {
	system := SystemConfig{
		SystemID: "matcher-prod", Provider: "openai", Model: "gpt-5.4-nano",
		PromptVersion: "v4", EvaluationLane: EvaluationLaneProductionFidelity,
		APIKind: APIKindChat, OutputContract: OutputContractJSONObject,
		BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY",
		Tasks: []Task{TaskMatcherExtract}, NoThinkLocation: NoThinkLocationUser,
		NoThinkFormat: NoThinkFormatDoubleNewline, AppendNoThink: true,
	}
	require.NoError(t, system.Validate())

	system.NoThinkFormat = NoThinkFormatNone
	require.ErrorContains(t, system.Validate(), "explicit no_think_format")
	system.NoThinkFormat = NoThinkFormatDoubleNewline

	system.OutputContract = OutputContractJSONSchema
	system.StructuredOutputs = true
	require.ErrorContains(t, system.Validate(), "cannot silently normalize")
}

func TestShadowFidelityRequiresPinnedDeployedWireContract(t *testing.T) {
	for _, task := range []Task{
		TaskMatcherExtract,
		TaskMatcherRerank,
		TaskContentFilter,
		TaskJunkPurge,
	} {
		t.Run(string(task), func(t *testing.T) {
			system := testShadowFidelitySystem(task)
			require.NoError(t, system.Validate())
			require.True(t, system.UsesDeployedWireContract())
		})
	}

	require.True(t, SystemConfig{
		EvaluationLane: EvaluationLaneProductionFidelity,
	}.UsesDeployedWireContract())
	require.False(t, SystemConfig{
		EvaluationLane: EvaluationLaneNormalizedStrict,
	}.UsesDeployedWireContract())

	tests := []struct {
		name   string
		mutate func(*SystemConfig)
		want   string
	}{
		{
			name: "openrouter only",
			mutate: func(system *SystemConfig) {
				system.Provider = "openai"
				system.BaseURL = "https://api.openai.com/v1"
			},
			want: "openrouter zdr chat route",
		},
		{
			name: "zdr required",
			mutate: func(system *SystemConfig) {
				system.ZDR = false
			},
			want: "enforce zdr",
		},
		{
			name: "chat only",
			mutate: func(system *SystemConfig) {
				system.APIKind = APIKindResponses
			},
			want: "responses api is currently supported only",
		},
		{
			name: "unknown quantization rejected",
			mutate: func(system *SystemConfig) {
				system.ProviderEndpoint = "deepinfra/unknown"
			},
			want: "explicit recognized quantization",
		},
		{
			name: "provider-only endpoint rejected",
			mutate: func(system *SystemConfig) {
				system.ProviderEndpoint = "deepinfra"
			},
			want: "explicit recognized quantization",
		},
		{
			name: "missing provider rejected",
			mutate: func(system *SystemConfig) {
				system.ProviderEndpoint = "/fp8"
			},
			want: "explicit recognized quantization",
		},
		{
			name: "matcher controls cannot drift",
			mutate: func(system *SystemConfig) {
				system.OmitMaxCompletionTokens = true
			},
			want: "production matcher request controls",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			system := testShadowFidelitySystem(TaskMatcherExtract)
			test.mutate(&system)
			require.ErrorContains(t, system.Validate(), test.want)
		})
	}
	for _, endpoint := range []string{"amazon-bedrock", "azure"} {
		t.Run("provider-only endpoint "+endpoint, func(t *testing.T) {
			system := testShadowFidelitySystem(TaskContentFilter)
			system.ProviderEndpoint = endpoint
			require.ErrorContains(
				t,
				system.Validate(),
				"explicit recognized quantization",
			)

			system.EvaluationLane = EvaluationLanePromptOnlyExperimental
			require.NoError(t, system.Validate())
			require.False(t, system.UsesDeployedWireContract())
		})
	}

	content := testShadowFidelitySystem(TaskContentFilter)
	content.OmitMaxCompletionTokens = false
	require.ErrorContains(
		t,
		content.Validate(),
		"production content-filter request controls",
	)

	junk := testShadowFidelitySystem(TaskJunkPurge)
	junk.ReasoningEffort = ""
	require.ErrorContains(
		t,
		junk.Validate(),
		"production junk-purge request controls",
	)
}

func testShadowFidelitySystem(task Task) SystemConfig {
	system := SystemConfig{
		SystemID:         "shadow-" + string(task),
		Provider:         "openrouter",
		Model:            "test/open-model",
		PromptVersion:    "production",
		EvaluationLane:   EvaluationLaneShadowFidelity,
		APIKind:          APIKindChat,
		BaseURL:          "https://openrouter.ai/api/v1",
		APIKeyEnv:        "OPENROUTER_API_KEY",
		ProviderEndpoint: "deepinfra/fp8",
		Tasks:            []Task{task},
		ZDR:              true,
		RequestTimeoutMS: 60_000,
	}
	switch task {
	case TaskMatcherExtract, TaskMatcherRerank:
		system.OutputContract = OutputContractJSONObject
		system.NoThinkLocation = NoThinkLocationUser
		system.NoThinkFormat = NoThinkFormatDoubleNewline
		system.AppendNoThink = true
	case TaskContentFilter:
		system.OutputContract = OutputContractPromptOnly
		system.NoThinkLocation = NoThinkLocationSystem
		system.NoThinkFormat = NoThinkFormatSingleNewline
		system.OmitMaxCompletionTokens = true
	case TaskJunkPurge:
		system.OutputContract = OutputContractPromptOnly
		system.NoThinkLocation = NoThinkLocationSystem
		system.NoThinkFormat = NoThinkFormatSingleNewline
		system.MaxCompletionTokensOverride = 512
		system.ReasoningEffort = "low"
	}
	return system
}

func TestNonZDRRouteRequiresRedactedReferenceLane(t *testing.T) {
	system := SystemConfig{
		SystemID: "nvidia-ref", Provider: "openrouter",
		Model:          "nvidia/nemotron-3-embed-1b:free",
		PromptVersion:  MatcherSpecialistAlgorithmID,
		EvaluationLane: EvaluationLaneSpecialist,
		APIKind:        APIKindEmbedding, OutputContract: OutputContractNative,
		BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY",
		ProviderEndpoint: "nvidia", Tasks: []Task{TaskMatcherRerank},
		NoThinkLocation: NoThinkLocationNone,
	}
	require.ErrorContains(t, system.Validate(), "outside the redacted-reference lane")
	system.EvaluationLane = EvaluationLaneRedactedReference
	require.NoError(t, system.Validate())
}

func TestCompletionTokenControlsAreExclusive(t *testing.T) {
	system := SystemConfig{
		SystemID: "content-prod", Provider: "openai", Model: "gpt-5.6-luna",
		PromptVersion: "v2", EvaluationLane: EvaluationLaneProductionFidelity,
		APIKind: APIKindResponses, OutputContract: OutputContractPromptOnly,
		BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY",
		Tasks: []Task{TaskContentFilter}, NoThinkLocation: NoThinkLocationNone,
		OmitMaxCompletionTokens: true, MaxCompletionTokensOverride: 120,
	}
	require.ErrorContains(t, system.Validate(), "mutually exclusive")
	system.MaxCompletionTokensOverride = 0
	require.NoError(t, system.Validate())
}

func TestGPT5SystemsMustOmitTemperature(t *testing.T) {
	temperature := 0.0
	seed := int64(20260724)
	system := SystemConfig{
		SystemID: "gpt5-control", Provider: "openrouter", Model: "openai/gpt-5-nano",
		PromptVersion: "v1", EvaluationLane: EvaluationLaneNormalizedStrict,
		APIKind: APIKindChat, OutputContract: OutputContractJSONSchema,
		BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY",
		ProviderEndpoint: "azure", Tasks: []Task{TaskJunkPurge},
		StructuredOutputs: true, ZDR: true, NoThinkLocation: NoThinkLocationNone,
		Temperature: &temperature, Seed: &seed,
	}
	require.ErrorContains(t, system.Validate(), "temperature must be omitted for gpt-5")

	system.Temperature = nil
	require.NoError(t, system.Validate())
}

func TestSystemConfigRejectsInvalidRequestTimeout(t *testing.T) {
	system := SystemConfig{
		SystemID: "timeout", Provider: "openai", Model: "gpt-5.4-nano",
		PromptVersion: "v1", EvaluationLane: EvaluationLaneProductionFidelity,
		APIKind: APIKindChat, OutputContract: OutputContractJSONObject,
		BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY",
		Tasks: []Task{TaskMatcherExtract}, NoThinkLocation: NoThinkLocationUser,
		NoThinkFormat: NoThinkFormatDoubleNewline, AppendNoThink: true,
	}
	system.RequestTimeoutMS = -1
	require.ErrorContains(t, system.Validate(), "request_timeout_ms")
	system.RequestTimeoutMS = 600_001
	require.ErrorContains(t, system.Validate(), "request_timeout_ms")
	system.RequestTimeoutMS = 60_000
	require.NoError(t, system.Validate())
}

func TestCheckedInFirstWaveManifestIsRunnable(t *testing.T) {
	manifest, err := LoadSystemManifest(opsFixture(t, "../../ops/llm-eval/first-wave-models.json"))
	require.NoError(t, err)
	require.NotEmpty(t, manifest.PricingNotes)

	byID := make(map[string]SystemConfig, len(manifest.Systems))
	for _, system := range manifest.Systems {
		byID[system.SystemID] = system
	}
	matcher := byID["openai-gpt-5-4-nano-production-fidelity"]
	require.Equal(t, APIKindChat, matcher.APIKind)
	require.Equal(t, OutputContractJSONObject, matcher.OutputContract)
	require.Equal(t, NoThinkLocationUser, matcher.NoThinkLocation)
	require.Equal(t, NoThinkFormatDoubleNewline, matcher.NoThinkFormat)
	require.Empty(t, matcher.ReasoningEffort)
	require.False(t, matcher.OmitMaxCompletionTokens)
	require.Zero(t, matcher.MaxCompletionTokensOverride)

	content := byID["openai-gpt-5-6-luna-production-fidelity"]
	require.Equal(t, APIKindResponses, content.APIKind)
	require.Equal(t, OutputContractPromptOnly, content.OutputContract)
	require.Equal(t, NoThinkLocationNone, content.NoThinkLocation)
	require.True(t, content.OmitMaxCompletionTokens)

	junk := byID["openai-gpt-5-6-terra-production-fidelity-low"]
	require.Equal(t, APIKindChat, junk.APIKind)
	require.Equal(t, OutputContractPromptOnly, junk.OutputContract)
	require.Equal(t, NoThinkLocationSystem, junk.NoThinkLocation)
	require.Equal(t, NoThinkFormatSingleNewline, junk.NoThinkFormat)
	require.Equal(t, "low", junk.ReasoningEffort)
	require.Equal(t, 512, junk.MaxCompletionTokensOverride)

	strict := byID["openai-gpt-5-4-nano-normalized-strict"]
	require.Equal(t, EvaluationLaneNormalizedStrict, strict.EvaluationLane)
	require.Equal(t, OutputContractJSONSchema, strict.OutputContract)
	require.True(t, strict.StructuredOutputs)

	freeLing := byID["openrouter-ling-3-0-flash-free-novita"]
	require.Equal(t, EvaluationLanePromptOnlyExperimental, freeLing.EvaluationLane)
	require.Equal(t, OutputContractPromptOnly, freeLing.OutputContract)
	require.False(t, freeLing.StructuredOutputs)
	require.True(t, freeLing.ZDR)

	nemo := byID["openrouter-mistral-nemo-deepinfra-fp8"]
	require.Equal(t, "mistralai/mistral-nemo", nemo.Model)
	require.Equal(t, "deepinfra/fp8", nemo.ProviderEndpoint)
	require.Equal(t, 0.019, nemo.InputUSDPerMillion)
	require.Equal(t, 0.03, nemo.OutputUSDPerMillion)

	llama := byID["openrouter-llama-3-1-8b-deepinfra-fp8"]
	require.Equal(t, "meta-llama/llama-3.1-8b-instruct", llama.Model)
	require.Equal(t, "deepinfra/fp8", llama.ProviderEndpoint)
	require.Equal(t, EvaluationLaneNormalizedStrict, llama.EvaluationLane)
	require.True(t, llama.ZDR)

	oss20 := byID["openrouter-gpt-oss-20b-deepinfra-bf16-low"]
	require.Equal(t, "deepinfra/bf16", oss20.ProviderEndpoint)
	require.Equal(t, "low", oss20.ReasoningEffort)
	require.Equal(t, 0.03, oss20.InputUSDPerMillion)
	require.Equal(t, 0.14, oss20.OutputUSDPerMillion)

	oss120 := byID["openrouter-gpt-oss-120b-coreweave-fp4-low"]
	require.Equal(t, "coreweave/fp4", oss120.ProviderEndpoint)
	require.Equal(t, "low", oss120.ReasoningEffort)
	require.Equal(t, 0.04, oss120.InputUSDPerMillion)
	require.Equal(t, 0.04, oss120.CachedInputUSDPerMillion)
	require.Equal(t, 0.14, oss120.OutputUSDPerMillion)

	deepseek := byID["openrouter-deepseek-v4-flash-deepinfra-fp4-no-reasoning"]
	require.Equal(t, "deepseek/deepseek-v4-flash", deepseek.Model)
	require.Equal(t, "deepinfra/fp4", deepseek.ProviderEndpoint)
	require.Equal(t, "none", deepseek.ReasoningEffort)
	require.Equal(t, 0.018, deepseek.CachedInputUSDPerMillion)

	for _, staleID := range []string{
		"openrouter-mistral-nemo-dekallm-fp8",
		"openrouter-gpt-oss-20b-dekallm-bf16-low",
		"openrouter-gpt-oss-120b-dekallm-bf16-low",
		"openrouter-gpt-oss-120b-deepinfra-bf16-low",
	} {
		_, exists := byID[staleID]
		require.False(t, exists, "stale route %q remains registered", staleID)
	}
}

func TestCurrentProductionFidelityManifestBindsLiveTimeout(t *testing.T) {
	manifest, err := LoadSystemManifest(opsFixture(
		t,
		"../../ops/llm-eval/current-production-fidelity-2026-07-27.json",
	))
	require.NoError(t, err)
	require.Len(t, manifest.Systems, 1)
	system := manifest.Systems[0]
	require.Equal(t, EvaluationLaneProductionFidelity, system.EvaluationLane)
	require.Equal(t, int64(60_000), system.RequestTimeoutMS)
	require.Equal(t, "gpt-5.4-nano", system.Model)
	require.Equal(
		t,
		[]Task{TaskMatcherExtract, TaskMatcherRerank},
		system.Tasks,
	)
}

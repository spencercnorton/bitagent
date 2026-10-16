package llmeval

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadCampaignPlanIsStrictAndBindsExactBytes(t *testing.T) {
	plan := testCampaignPlan()
	raw, err := json.Marshal(plan)
	require.NoError(t, err)

	decoded, digest, err := ReadCampaignPlan(bytes.NewReader(raw))
	require.NoError(t, err)
	require.Equal(t, plan.CampaignID, decoded.CampaignID)
	require.Equal(t, sha256Hex(raw), digest)

	_, whitespaceDigest, err := ReadCampaignPlan(bytes.NewReader(append(raw, '\n')))
	require.NoError(t, err)
	require.NotEqual(t, digest, whitespaceDigest)

	unknown := bytes.Replace(raw, []byte(`"shadow_only":true`), []byte(`"shadow_only":true,"deploy":true`), 1)
	_, _, err = ReadCampaignPlan(bytes.NewReader(unknown))
	require.ErrorContains(t, err, "unknown field")

	duplicate := bytes.Replace(raw, []byte(`"campaign_id":`), []byte(`"campaign_id":"duplicate","campaign_id":`), 1)
	_, _, err = ReadCampaignPlan(bytes.NewReader(duplicate))
	require.ErrorContains(t, err, "duplicate object key")

	_, _, err = ReadCampaignPlan(bytes.NewReader(append(raw, []byte(`{}`)...)))
	require.ErrorContains(t, err, "multiple top-level values")
}

func TestCampaignPlanValidatesAndBuildsDeterministicMatrix(t *testing.T) {
	plan := testCampaignPlan()
	require.NoError(t, plan.Validate())

	matrix := plan.ExecutionMatrix()
	require.Equal(t, []CampaignExecution{
		{
			RunID: "run-deployed-control-contentfilter", Stage: CampaignStageDevelopment,
			Task: TaskContentFilter, SystemID: "deployed-control", Role: CampaignSystemRoleControl,
			CostCapMicroUSD: 250_000,
		},
		{
			RunID: "run-normalized-control-contentfilter", Stage: CampaignStageDevelopment,
			Task: TaskContentFilter, SystemID: "normalized-control", Role: CampaignSystemRoleControl,
			CostCapMicroUSD: 200_000,
		},
		{
			RunID: "run-candidate-contentfilter", Stage: CampaignStageDevelopment,
			Task: TaskContentFilter, SystemID: "candidate", Role: CampaignSystemRoleCandidate,
			CostCapMicroUSD: 250_000,
		},
	}, matrix)

	refs := plan.ArtifactReferences()
	require.Len(t, refs, 5)
	require.Equal(t, "historical_baselines[0].artifact", refs[0].Name)
	require.Empty(t, refs[0].Path)
}

func TestCampaignRunBindingIsCompleteDeterministicAndPathFree(t *testing.T) {
	plan := testCampaignPlan()
	planSHA := strings.Repeat("9", 64)
	binding, err := plan.RunBinding(planSHA, "run-candidate-contentfilter")
	require.NoError(t, err)
	require.NoError(t, binding.Validate())
	require.Equal(t, plan.CampaignID, binding.CampaignID)
	require.Equal(t, planSHA, binding.CampaignSHA256)
	require.Equal(t, CampaignStageDevelopment, binding.Stage)
	require.Equal(t, "candidate", binding.SystemID)
	require.Equal(t, TaskContentFilter, binding.Task)
	require.Equal(t, CampaignSystemRoleCandidate, binding.Role)
	require.Empty(t, binding.ControlKind)
	require.Equal(t, int64(250_000), binding.CostCapMicroUSD)
	require.Equal(t, plan.TaskArtifacts[0].Corpus.ArtifactID, binding.Corpus.ArtifactID)
	require.NotNil(t, binding.GoldClosure)
	require.NotNil(t, binding.PrivacySidecar)
	require.NotNil(t, binding.RouteSnapshot)
	require.True(t, binding.ExactEndpointEvidence)
	require.Equal(t, plan.Runs[2].Outputs, binding.Outputs)

	raw, err := json.Marshal(binding)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "/var/tmp")
	require.NotContains(t, string(raw), `"path"`)

	again, err := plan.RunBinding(planSHA, "run-candidate-contentfilter")
	require.NoError(t, err)
	require.Equal(t, binding, again)

	_, err = plan.RunBinding("bad", "run-candidate-contentfilter")
	require.ErrorContains(t, err, "campaign_sha256")
	_, err = plan.RunBinding(planSHA, "missing-run")
	require.ErrorContains(t, err, "absent from the campaign plan")
}

func TestCampaignComparisonMatrixIsCompleteUniqueAndResolvable(t *testing.T) {
	plan := testCampaignPlan()
	require.NoError(t, plan.Validate())

	comparison, err := plan.ComparisonForRuns(
		"run-deployed-control-contentfilter",
		"run-candidate-contentfilter",
	)
	require.NoError(t, err)
	require.Equal(
		t,
		"comparisons/candidate-vs-deployed.json",
		comparison.OutputPath,
	)
	_, err = plan.ComparisonForRuns(
		"run-candidate-contentfilter",
		"run-deployed-control-contentfilter",
	)
	require.ErrorContains(t, err, "absent from the campaign comparison matrix")

	tests := []struct {
		name string
		edit func(*CampaignPlan)
		want string
	}{
		{
			name: "missing dual-control cell",
			edit: func(plan *CampaignPlan) {
				plan.Comparisons = plan.Comparisons[:1]
			},
			want: "matrix is incomplete",
		},
		{
			name: "duplicate pair",
			edit: func(plan *CampaignPlan) {
				plan.Comparisons[1].ControlRunID =
					plan.Comparisons[0].ControlRunID
			},
			want: "duplicates control/candidate pair",
		},
		{
			name: "comparison output collision",
			edit: func(plan *CampaignPlan) {
				plan.Comparisons[1].OutputPath =
					plan.Comparisons[0].OutputPath
			},
			want: "collides with comparisons[0].output_path",
		},
		{
			name: "wrong run roles",
			edit: func(plan *CampaignPlan) {
				plan.Comparisons[0].ControlRunID =
					"run-candidate-contentfilter"
			},
			want: "must identify one control run and one candidate run",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bad := testCampaignPlan()
			test.edit(&bad)
			require.ErrorContains(t, bad.Validate(), test.want)
		})
	}
}

func TestCampaignComparisonCardinalityRejectedBeforeMaterialization(
	t *testing.T,
) {
	const controlCount = 5_000
	const candidateCount = 5_000
	plan := CampaignPlan{
		Comparisons: []CampaignComparison{{
			ComparisonID: "cardinality-sentinel",
		}},
		Systems: make([]CampaignSystemMatrix, 0, controlCount),
		Runs:    make([]CampaignRun, 0, controlCount+candidateCount),
	}
	systemRoles := make(
		map[string]CampaignSystemRole,
		controlCount+candidateCount,
	)
	for index := 0; index < controlCount; index++ {
		systemID := "control-" + strconv.Itoa(index)
		kind := CampaignControlKindNormalized
		if index%2 == 0 {
			kind = CampaignControlKindDeployed
		}
		plan.Systems = append(plan.Systems, CampaignSystemMatrix{
			SystemID:    systemID,
			Role:        CampaignSystemRoleControl,
			ControlKind: kind,
		})
		plan.Runs = append(plan.Runs, CampaignRun{
			RunID:    "run-" + systemID,
			SystemID: systemID,
			Task:     TaskContentFilter,
		})
		systemRoles[systemID] = CampaignSystemRoleControl
	}
	for index := 0; index < candidateCount; index++ {
		systemID := "candidate-" + strconv.Itoa(index)
		plan.Runs = append(plan.Runs, CampaignRun{
			RunID:    "run-" + systemID,
			SystemID: systemID,
			Task:     TaskContentFilter,
		})
		systemRoles[systemID] = CampaignSystemRoleCandidate
	}

	err := plan.validateComparisons(systemRoles)
	require.ErrorContains(
		t,
		err,
		"comparisons matrix requires more than the 100000-entry limit",
	)
}

func TestCampaignDerivedCheckpointPathsAreGloballyReserved(t *testing.T) {
	checkpoint, err := CampaignCheckpointRelativePath(
		testCampaignPlan().Runs[0].Outputs.ResultsPath,
	)
	require.NoError(t, err)

	tests := []struct {
		name string
		edit func(*CampaignPlan)
		want string
	}{
		{
			name: "cross-run output",
			edit: func(plan *CampaignPlan) {
				plan.Runs[1].Outputs.ScorePath = checkpoint
			},
			want: "derived checkpoint path collides with runs[1].outputs.score_path",
		},
		{
			name: "comparison output",
			edit: func(plan *CampaignPlan) {
				plan.Comparisons[1].OutputPath = checkpoint
			},
			want: "derived checkpoint path collides with comparisons[1].output_path",
		},
		{
			name: "summary output",
			edit: func(plan *CampaignPlan) {
				plan.SummaryPath = checkpoint
			},
			want: "derived checkpoint path collides with summary_path",
		},
		{
			name: "bound corpus input",
			edit: func(plan *CampaignPlan) {
				plan.TaskArtifacts[0].Corpus.Path = filepath.Join(
					plan.OutputRoot,
					checkpoint,
				)
			},
			want: "derived checkpoint path collides with task_artifacts[0].corpus",
		},
		{
			name: "bound gold input",
			edit: func(plan *CampaignPlan) {
				plan.TaskArtifacts[0].GoldClosure.Path = filepath.Join(
					plan.OutputRoot,
					checkpoint,
				)
			},
			want: "derived checkpoint path collides with task_artifacts[0].gold_closure",
		},
		{
			name: "bound privacy input",
			edit: func(plan *CampaignPlan) {
				plan.TaskArtifacts[0].PrivacySidecar.Path = filepath.Join(
					plan.OutputRoot,
					checkpoint,
				)
			},
			want: "derived checkpoint path collides with task_artifacts[0].privacy_sidecar",
		},
		{
			name: "bound route input",
			edit: func(plan *CampaignPlan) {
				plan.RouteSnapshots[0].Snapshot.Path = filepath.Join(
					plan.OutputRoot,
					checkpoint,
				)
			},
			want: "derived checkpoint path collides with route_snapshots[0].snapshot",
		},
		{
			name: "bound baseline input",
			edit: func(plan *CampaignPlan) {
				plan.HistoricalBaselines[0].Artifact.Path = filepath.Join(
					plan.OutputRoot,
					checkpoint,
				)
			},
			want: "derived checkpoint path collides with historical_baselines[0].artifact",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bad := testCampaignPlan()
			test.edit(&bad)
			require.ErrorContains(t, bad.Validate(), test.want)
		})
	}
}

func TestCampaignRunBindingRejectsTampering(t *testing.T) {
	binding, err := testCampaignPlan().RunBinding(strings.Repeat("9", 64), "run-deployed-control-contentfilter")
	require.NoError(t, err)

	tests := []struct {
		name string
		edit func(*CampaignRunBinding)
		want string
	}{
		{name: "not shadow", edit: func(b *CampaignRunBinding) { b.ShadowOnly = false }, want: "shadow_only"},
		{name: "wrong manifest", edit: func(b *CampaignRunBinding) { b.SystemManifestSHA256 = strings.Repeat("x", 64) }, want: "system_manifest_sha256"},
		{name: "candidate kind", edit: func(b *CampaignRunBinding) {
			b.Role = CampaignSystemRoleCandidate
		}, want: "candidate run binding must not declare control_kind"},
		{name: "absolute output", edit: func(b *CampaignRunBinding) { b.Outputs.ResultsPath = "/tmp/results.jsonl" }, want: "clean relative file path"},
		{name: "exact route without snapshot", edit: func(b *CampaignRunBinding) {
			b.ExactEndpointEvidence = true
			b.RouteSnapshot = nil
		}, want: "requires route_snapshot"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bad := binding
			test.edit(&bad)
			require.ErrorContains(t, bad.Validate(), test.want)
		})
	}
}

func TestCampaignPlanRejectsUnsafeOrIncomparableDeclarations(t *testing.T) {
	tests := []struct {
		name string
		edit func(*CampaignPlan)
		want string
	}{
		{
			name: "non shadow",
			edit: func(plan *CampaignPlan) { plan.ShadowOnly = false },
			want: "shadow_only must be true",
		},
		{
			name: "unsafe deploy action",
			edit: func(plan *CampaignPlan) { plan.AllowedActions[3] = CampaignAction("deploy") },
			want: "promotion and deploy actions are forbidden",
		},
		{
			name: "superseded corpus plan",
			edit: func(plan *CampaignPlan) { plan.CorpusPlanID = SupersededCorpusPlanV1ID },
			want: "superseded",
		},
		{
			name: "missing control",
			edit: func(plan *CampaignPlan) {
				for i := 0; i < 2; i++ {
					plan.Systems[i].Role = CampaignSystemRoleCandidate
					plan.Systems[i].ControlKind = ""
				}
			},
			want: "has no control system",
		},
		{
			name: "missing candidate",
			edit: func(plan *CampaignPlan) {
				plan.Systems[2].Role = CampaignSystemRoleControl
				plan.Systems[2].ControlKind = CampaignControlKindNormalized
			},
			want: "has no candidate system",
		},
		{
			name: "formal matrix missing normalized control",
			edit: func(plan *CampaignPlan) { plan.Systems[1].ControlKind = CampaignControlKindDeployed },
			want: "requires both deployed and normalized controls",
		},
		{
			name: "duplicate system ID",
			edit: func(plan *CampaignPlan) { plan.Systems[1].SystemID = plan.Systems[0].SystemID },
			want: "duplicate system_id",
		},
		{
			name: "undeclared run cell",
			edit: func(plan *CampaignPlan) { plan.Runs[0].Task = TaskJunkPurge },
			want: "absent from the declared matrix",
		},
		{
			name: "cost cap sum",
			edit: func(plan *CampaignPlan) { plan.TotalCostCapMicroUSD = 600_000 },
			want: "sum of per-run cost caps exceeds",
		},
		{
			name: "wrong task metric",
			edit: func(plan *CampaignPlan) { plan.EffectivenessGates[0].HarmUtility.HarmMetric = CampaignJunkPurgeHarm },
			want: "requires harm metric",
		},
		{
			name: "severity one errors allowed",
			edit: func(plan *CampaignPlan) { plan.EffectivenessGates[0].HarmUtility.MaxSeverityOneErrors = 1 },
			want: "max_severity_one_errors must equal 0",
		},
		{
			name: "absolute harm rate too loose",
			edit: func(plan *CampaignPlan) { plan.EffectivenessGates[0].HarmUtility.MaxHarmRate = 0.002 },
			want: "max_harm_rate must be at most",
		},
		{
			name: "utility floor omitted",
			edit: func(plan *CampaignPlan) { plan.EffectivenessGates[0].HarmUtility.MinUtilityRate = 0 },
			want: "min_utility_rate must be positive",
		},
		{
			name: "wrong inference confidence",
			edit: func(plan *CampaignPlan) { plan.EffectivenessGates[0].Inference.ConfidenceLevel = 0.90 },
			want: "inference must use confidence",
		},
		{
			name: "bootstrap replicates not preregistered",
			edit: func(plan *CampaignPlan) { plan.EffectivenessGates[0].Inference.BootstrapReplicates = 0 },
			want: "bootstrap replicates",
		},
		{
			name: "bootstrap seed not preregistered",
			edit: func(plan *CampaignPlan) { plan.EffectivenessGates[0].Inference.BootstrapSeed = 0 },
			want: "bootstrap seed",
		},
		{
			name: "harm noninferiority too loose",
			edit: func(plan *CampaignPlan) { plan.EffectivenessGates[0].HarmUtility.MaxHarmUCBDeltaVsControl = 0.003 },
			want: "max_harm_ucb_delta_vs_control",
		},
		{
			name: "utility noninferiority too loose",
			edit: func(plan *CampaignPlan) { plan.EffectivenessGates[0].HarmUtility.MinUtilityLCBDeltaVsControl = -0.02 },
			want: "min_utility_lcb_delta_vs_control",
		},
		{
			name: "first pass schema threshold too low",
			edit: func(plan *CampaignPlan) {
				plan.EffectivenessGates[0].SchemaReliability.MinFirstPassSchemaValidRate = 0.994
			},
			want: "min_first_pass_schema_valid_rate",
		},
		{
			name: "final schema threshold below one",
			edit: func(plan *CampaignPlan) { plan.EffectivenessGates[0].SchemaReliability.MinFinalSchemaValidRate = 0.999 },
			want: "min_final_schema_valid_rate must equal 1",
		},
		{
			name: "more than one schema repair",
			edit: func(plan *CampaignPlan) { plan.EffectivenessGates[0].SchemaReliability.MaxSchemaRepairsPerCase = 2 },
			want: "max_schema_repairs_per_case",
		},
		{
			name: "call error UCB too loose",
			edit: func(plan *CampaignPlan) { plan.EffectivenessGates[0].SchemaReliability.MaxCallErrorUCB = 0.006 },
			want: "max_call_error_ucb",
		},
		{
			name: "unordered latency",
			edit: func(plan *CampaignPlan) { plan.EffectivenessGates[0].Latency.MaxP95MS = 20 },
			want: "p50 <= p95 <= p99",
		},
		{
			name: "task deadline drift",
			edit: func(plan *CampaignPlan) { plan.EffectivenessGates[0].Latency.DeadlineMS = 9_000 },
			want: "deadline_ms for task",
		},
		{
			name: "p99 exceeds deadline",
			edit: func(plan *CampaignPlan) { plan.EffectivenessGates[0].Latency.MaxP99MS = 8_001 },
			want: "must not exceed deadline_ms",
		},
		{
			name: "relative latency too slow",
			edit: func(plan *CampaignPlan) { plan.EffectivenessGates[0].Latency.MaxP95RelativeSlowdownVsControl = 0.21 },
			want: "max_p95_relative_slowdown_vs_control",
		},
		{
			name: "timeout UCB too loose",
			edit: func(plan *CampaignPlan) { plan.EffectivenessGates[0].Latency.MaxTimeoutUCB = 0.006 },
			want: "max_timeout_ucb",
		},
		{
			name: "harmful repeat flip allowed",
			edit: func(plan *CampaignPlan) { plan.EffectivenessGates[0].Stability.MaxHarmfulRepeatFlips = 1 },
			want: "max_harmful_repeat_flips must equal 0",
		},
		{
			name: "final action flip UCB too loose",
			edit: func(plan *CampaignPlan) { plan.EffectivenessGates[0].Stability.MaxFinalActionFlipUCB = 0.02 },
			want: "max_final_action_flip_ucb",
		},
		{
			name: "calibration gate omitted for confidence task",
			edit: func(plan *CampaignPlan) {
				plan.EffectivenessGates[0].Calibration = nil
			},
			want: "requires a calibration gate",
		},
		{
			name: "selective risk gate omitted for confidence task",
			edit: func(plan *CampaignPlan) {
				plan.EffectivenessGates[0].SelectiveRisk = nil
			},
			want: "requires a selective_risk gate",
		},
		{
			name: "brier threshold too loose",
			edit: func(plan *CampaignPlan) { plan.EffectivenessGates[0].Calibration.MaxBrierScore = 0.251 },
			want: "max_brier_score must be at most",
		},
		{
			name: "coverage regresses incumbent",
			edit: func(plan *CampaignPlan) { plan.EffectivenessGates[0].SelectiveRisk.MinCoverageRatioVsIncumbent = 0.99 },
			want: "min_coverage_ratio_vs_incumbent",
		},
		{
			name: "critical strata omitted",
			edit: func(plan *CampaignPlan) { plan.EffectivenessGates[0].CriticalStrata = nil },
			want: "critical_strata must not be empty",
		},
		{
			name: "critical stratum harm too loose",
			edit: func(plan *CampaignPlan) { plan.EffectivenessGates[0].CriticalStrata[0].MaxHarmRate = 0.002 },
			want: "critical_strata[0].max_harm_rate must be at most",
		},
		{
			name: "cost per safe action omitted",
			edit: func(plan *CampaignPlan) { plan.EffectivenessGates[0].Cost.MaxCostPerCorrectSafeActionMicroUSD = 0 },
			want: "cost limits must be positive",
		},
		{
			name: "missing historical baseline",
			edit: func(plan *CampaignPlan) { plan.HistoricalBaselines = nil },
			want: "historical_baselines must not be empty",
		},
		{
			name: "historical baseline marked promotable",
			edit: func(plan *CampaignPlan) { plan.HistoricalBaselines[0].PromotionEligible = true },
			want: "historical artifacts must be nonpromotable",
		},
		{
			name: "duplicate artifact ID globally",
			edit: func(plan *CampaignPlan) {
				plan.RouteSnapshots[0].Snapshot.ArtifactID = plan.TaskArtifacts[0].Corpus.ArtifactID
			},
			want: "duplicate artifact_id",
		},
		{
			name: "output traversal",
			edit: func(plan *CampaignPlan) { plan.Runs[0].Outputs.ResultsPath = "../results.jsonl" },
			want: "must remain below output_root",
		},
		{
			name: "output collision",
			edit: func(plan *CampaignPlan) { plan.Runs[1].Outputs.ResultsPath = plan.Runs[0].Outputs.ResultsPath },
			want: "collides with",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := testCampaignPlan()
			test.edit(&plan)
			err := plan.Validate()
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestCampaignProspectiveShadowMinimumsAreStageSpecific(t *testing.T) {
	plan := testCampaignPlan()
	plan.Stage = CampaignStageProspectiveShadow
	require.ErrorContains(t, plan.Validate(), "prospective_shadow is required")

	plan.ProspectiveShadow = &CampaignProspectiveShadow{
		MinimumDurationDays: CampaignMinShadowDays,
		MinimumCases:        CampaignMinShadowCases,
	}
	require.NoError(t, plan.Validate())

	plan.ProspectiveShadow.MinimumCases--
	require.ErrorContains(t, plan.Validate(), "at least 7 days and 10000 cases")

	plan = testCampaignPlan()
	plan.ProspectiveShadow = &CampaignProspectiveShadow{
		MinimumDurationDays: CampaignMinShadowDays,
		MinimumCases:        CampaignMinShadowCases,
	}
	require.ErrorContains(t, plan.Validate(), "only valid for prospective_shadow stage")
}

func TestCampaignRepeatStageRequiresCompleteIndependentMatrix(t *testing.T) {
	plan := testCampaignPlan()
	plan.Stage = CampaignStageRepeat
	for i := range plan.Runs {
		plan.Runs[i].RepeatIndex = 1
	}
	require.ErrorContains(t, plan.Validate(), "at least two runs")

	firstPass := append([]CampaignRun(nil), plan.Runs...)
	for _, first := range firstPass {
		second := first
		second.RunID += "-repeat-2"
		second.RepeatIndex = 2
		second.Outputs = CampaignRunOutputs{
			ResultsPath:         first.SystemID + "-repeat-2/results.jsonl",
			AttemptEvidencePath: first.SystemID + "-repeat-2/attempts.json",
			ScorePath:           first.SystemID + "-repeat-2/score.json",
		}
		plan.Runs = append(plan.Runs, second)
	}
	plan.Comparisons = append(plan.Comparisons,
		CampaignComparison{
			ComparisonID:   "compare-candidate-vs-deployed-contentfilter-repeat-2",
			ControlRunID:   "run-deployed-control-contentfilter-repeat-2",
			CandidateRunID: "run-candidate-contentfilter-repeat-2",
			OutputPath: "comparisons/" +
				"candidate-vs-deployed-repeat-2.json",
		},
		CampaignComparison{
			ComparisonID:   "compare-candidate-vs-normalized-contentfilter-repeat-2",
			ControlRunID:   "run-normalized-control-contentfilter-repeat-2",
			CandidateRunID: "run-candidate-contentfilter-repeat-2",
			OutputPath: "comparisons/" +
				"candidate-vs-normalized-repeat-2.json",
		},
	)
	plan.TotalCostCapMicroUSD = 1_400_000
	require.NoError(t, plan.Validate())
}

func TestCampaignExecutablePreflightRequiresEveryArtifactPath(t *testing.T) {
	plan := testCampaignPlan()
	require.NoError(t, plan.Validate())
	require.ErrorContains(t, plan.ValidateExecutableArtifacts(), "task_artifacts[0].corpus.path is required")

	plan.TaskArtifacts[0].Corpus.Path = "/var/tmp/bitagent-llmeval/corpus.jsonl"
	require.ErrorContains(t, plan.ValidateExecutableArtifacts(), "task_artifacts[0].gold_closure.path is required")
	plan.TaskArtifacts[0].GoldClosure.Path = "/var/tmp/bitagent-llmeval/gold.json"
	require.ErrorContains(t, plan.ValidateExecutableArtifacts(), "task_artifacts[0].privacy_sidecar.path is required")
	plan.TaskArtifacts[0].PrivacySidecar.Path = "/var/tmp/bitagent-llmeval/privacy.json"
	require.ErrorContains(t, plan.ValidateExecutableArtifacts(), "historical_baselines[0].artifact.path is required")
	plan.HistoricalBaselines[0].Artifact.Path = "/var/tmp/bitagent-llmeval/baseline.json"
	require.NoError(t, plan.ValidateExecutableArtifacts())
}

func TestCampaignPlanValidatesExactManifestCorpusAndWorktreeBindings(t *testing.T) {
	plan := testCampaignPlan()
	manifest := testCampaignManifest()
	manifestRaw, err := json.Marshal(manifest)
	require.NoError(t, err)
	manifestSHA := sha256Hex(manifestRaw)
	plan.SystemManifestSHA256 = manifestSHA

	corpusPlan, corpusSHA := readCheckedInCampaignCorpusPlan(t)
	plan.CorpusPlanID = corpusPlan.PlanID
	plan.CorpusPlanSHA256 = corpusSHA
	require.NoError(t, plan.ValidateBindings(
		manifest,
		manifestSHA,
		corpusPlan,
		corpusSHA,
		"/source/bitagent",
	))

	t.Run("manifest hash", func(t *testing.T) {
		err := plan.ValidateBindings(manifest, strings.Repeat("f", 64), corpusPlan, corpusSHA, "/source/bitagent")
		require.ErrorContains(t, err, "system manifest bytes")
	})

	t.Run("manifest task mismatch", func(t *testing.T) {
		bad := manifest
		bad.Systems = append([]SystemConfig(nil), manifest.Systems...)
		bad.Systems[1].Tasks = []Task{TaskJunkPurge}
		err := plan.ValidateBindings(bad, manifestSHA, corpusPlan, corpusSHA, "/source/bitagent")
		require.ErrorContains(t, err, "does not support task")
	})

	t.Run("manifest timeout missing", func(t *testing.T) {
		bad := manifest
		bad.Systems = append([]SystemConfig(nil), manifest.Systems...)
		bad.Systems[1].RequestTimeoutMS = 0
		err := plan.ValidateBindings(bad, manifestSHA, corpusPlan, corpusSHA, "/source/bitagent")
		require.ErrorContains(t, err, "request_timeout_ms must equal")
	})

	t.Run("latency gate exceeds timeout", func(t *testing.T) {
		bad := plan
		bad.EffectivenessGates = append([]CampaignEffectivenessGate(nil), plan.EffectivenessGates...)
		bad.EffectivenessGates[0].Latency.MaxP99MS = 9_000
		err := bad.ValidateBindings(manifest, manifestSHA, corpusPlan, corpusSHA, "/source/bitagent")
		require.ErrorContains(t, err, "must not exceed deadline_ms")
	})

	t.Run("required safety count slice omitted", func(t *testing.T) {
		bad := plan
		bad.EffectivenessGates = append([]CampaignEffectivenessGate(nil), plan.EffectivenessGates...)
		bad.EffectivenessGates[0].CriticalStrata = append(
			[]CampaignCriticalStratumGate(nil),
			plan.EffectivenessGates[0].CriticalStrata[:1]...,
		)
		err := bad.ValidateBindings(manifest, manifestSHA, corpusPlan, corpusSHA, "/source/bitagent")
		require.ErrorContains(t, err, "missing corpus-plan-required safety count slice \"latin_script_non_english\"")
	})

	t.Run("required safety count under minimum", func(t *testing.T) {
		bad := plan
		bad.EffectivenessGates = append([]CampaignEffectivenessGate(nil), plan.EffectivenessGates...)
		bad.EffectivenessGates[0].CriticalStrata = append(
			[]CampaignCriticalStratumGate(nil),
			plan.EffectivenessGates[0].CriticalStrata...,
		)
		bad.EffectivenessGates[0].CriticalStrata[0].MinimumCases = 1_999
		err := bad.ValidateBindings(manifest, manifestSHA, corpusPlan, corpusSHA, "/source/bitagent")
		require.ErrorContains(t, err, "minimum_cases 1999 is below corpus-plan-required safety count 2000")
	})

	t.Run("direct route snapshot rejected", func(t *testing.T) {
		bad := plan
		bad.RouteSnapshots = append(append([]CampaignRouteSnapshot(nil), plan.RouteSnapshots...), CampaignRouteSnapshot{
			SystemID: "deployed-control",
			Snapshot: CampaignArtifactBinding{
				ArtifactID: "route-control",
				Path:       "/var/tmp/bitagent-llmeval/route-control.json",
				SHA256:     strings.Repeat("8", 64),
			},
		})
		err := bad.ValidateBindings(manifest, manifestSHA, corpusPlan, corpusSHA, "/source/bitagent")
		require.ErrorContains(t, err, "non-OpenRouter")
	})

	t.Run("formal route must carry exact endpoint evidence", func(t *testing.T) {
		bad := plan
		bad.RouteSnapshots = append([]CampaignRouteSnapshot(nil), plan.RouteSnapshots...)
		bad.RouteSnapshots[0].ExactEndpointEvidence = false
		err := bad.ValidateBindings(manifest, manifestSHA, corpusPlan, corpusSHA, "/source/bitagent")
		require.ErrorContains(t, err, "requires exact_endpoint_evidence")
	})

	t.Run("formal route must pin quantization", func(t *testing.T) {
		badManifest := manifest
		badManifest.Systems = append([]SystemConfig(nil), manifest.Systems...)
		badManifest.Systems[2].ProviderEndpoint = "azure"
		badManifestRaw, marshalErr := json.Marshal(badManifest)
		require.NoError(t, marshalErr)
		badManifestSHA := sha256Hex(badManifestRaw)
		bad := plan
		bad.SystemManifestSHA256 = badManifestSHA
		err := bad.ValidateBindings(badManifest, badManifestSHA, corpusPlan, corpusSHA, "/source/bitagent")
		require.ErrorContains(t, err, "explicit recognized quantization")
	})

	t.Run("protocol screen permits non-exact provider-only route", func(t *testing.T) {
		protocolManifest := manifest
		protocolManifest.Systems = append([]SystemConfig(nil), manifest.Systems...)
		protocolManifest.Systems[2].ProviderEndpoint = "azure"
		protocolManifestRaw, marshalErr := json.Marshal(protocolManifest)
		require.NoError(t, marshalErr)
		protocolManifestSHA := sha256Hex(protocolManifestRaw)
		protocol := plan
		protocol.Stage = CampaignStageProtocolScreen
		protocol.SystemManifestSHA256 = protocolManifestSHA
		protocol.RouteSnapshots = append([]CampaignRouteSnapshot(nil), plan.RouteSnapshots...)
		protocol.RouteSnapshots[0].ExactEndpointEvidence = false
		require.NoError(t, protocol.ValidateBindings(protocolManifest, protocolManifestSHA, corpusPlan, corpusSHA, "/source/bitagent"))

		protocol.RouteSnapshots[0].ExactEndpointEvidence = true
		err := protocol.ValidateBindings(protocolManifest, protocolManifestSHA, corpusPlan, corpusSHA, "/source/bitagent")
		require.ErrorContains(t, err, "cannot claim exact_endpoint_evidence")
	})

	t.Run("openrouter missing route snapshot", func(t *testing.T) {
		bad := testCampaignPlan()
		bad.SystemManifestSHA256 = manifestSHA
		bad.CorpusPlanID = corpusPlan.PlanID
		bad.CorpusPlanSHA256 = corpusSHA
		bad.RouteSnapshots = nil
		err := bad.ValidateBindings(manifest, manifestSHA, corpusPlan, corpusSHA, "/source/bitagent")
		require.ErrorContains(t, err, "requires exactly one route snapshot")
	})

	t.Run("worktree output", func(t *testing.T) {
		bad := plan
		bad.OutputRoot = "/source/bitagent/private-results"
		err := bad.ValidateBindings(manifest, manifestSHA, corpusPlan, corpusSHA, "/source/bitagent")
		require.ErrorContains(t, err, "outside the Git worktree")
	})
}

func TestCampaignEffectivenessSafetyBindingsRequireEveryNamedSlice(t *testing.T) {
	corpusPlan, _ := readCheckedInCampaignCorpusPlan(t)
	var matcherTask CorpusPlanTask
	for _, taskPlan := range corpusPlan.Tasks {
		if taskPlan.Task == TaskMatcherExtract {
			matcherTask = taskPlan
			break
		}
	}
	require.NotEmpty(t, matcherTask.RequiredSafetySlices)

	strata := make([]CampaignCriticalStratumGate, 0, len(matcherTask.RequiredSafetySlices))
	for _, sliceID := range matcherTask.RequiredSafetySlices {
		strata = append(strata, CampaignCriticalStratumGate{
			SliceID:      sliceID,
			MinimumCases: 1,
		})
	}
	plan := CampaignPlan{EffectivenessGates: []CampaignEffectivenessGate{{
		Task:           TaskMatcherExtract,
		CriticalStrata: strata,
	}}}
	require.NoError(t, plan.validateEffectivenessSafetyBindings(corpusPlan))

	omitted := matcherTask.RequiredSafetySlices[len(matcherTask.RequiredSafetySlices)-1]
	plan.EffectivenessGates[0].CriticalStrata = plan.EffectivenessGates[0].CriticalStrata[:len(strata)-1]
	require.ErrorContains(
		t,
		plan.validateEffectivenessSafetyBindings(corpusPlan),
		"missing corpus-plan-required safety slice \""+omitted+"\"",
	)
}

func testCampaignPlan() CampaignPlan {
	hash := func(value byte) string { return strings.Repeat(string(value), 64) }
	gold := CampaignArtifactBinding{ArtifactID: "gold-contentfilter", SHA256: hash('b')}
	privacy := CampaignArtifactBinding{ArtifactID: "privacy-contentfilter", SHA256: hash('c')}
	effectivenessGate := testContentFilterCampaignGate()
	effectivenessGate.CriticalStrata = []CampaignCriticalStratumGate{
		{SliceID: "hard_must_keep_english", MinimumCases: 2_000, MaxSeverityOneErrors: 0, MaxHarmRate: CampaignMaxAbsoluteHarmRate, MinUtilityRate: 0.8},
		{SliceID: "latin_script_non_english", MinimumCases: 500, MaxSeverityOneErrors: 0, MaxHarmRate: CampaignMaxAbsoluteHarmRate, MinUtilityRate: 0.8},
	}
	return CampaignPlan{
		SchemaVersion:        CampaignSchemaVersion,
		CampaignID:           "campaign:test-2026-07-28",
		BenchmarkEpoch:       "2026-07-28",
		ProtocolVersion:      CampaignProtocolVersion,
		ShadowOnly:           true,
		Stage:                CampaignStageDevelopment,
		CorpusPlanID:         "bitagent-llm-corpus-v2-test",
		CorpusPlanSHA256:     hash('d'),
		SystemManifestSHA256: hash('e'),
		OutputRoot:           "/var/tmp/bitagent-llmeval/campaign-test",
		SummaryPath:          "campaign-summary.json",
		AllowedActions: []CampaignAction{
			CampaignActionValidate,
			CampaignActionRunShadow,
			CampaignActionScore,
			CampaignActionCompare,
		},
		TotalCostCapMicroUSD: 700_000,
		TaskArtifacts: []CampaignTaskArtifacts{
			{
				Task:           TaskContentFilter,
				Corpus:         CampaignArtifactBinding{ArtifactID: "corpus-contentfilter", SHA256: hash('a')},
				GoldClosure:    &gold,
				PrivacySidecar: &privacy,
			},
		},
		Systems: []CampaignSystemMatrix{
			{SystemID: "deployed-control", Role: CampaignSystemRoleControl, ControlKind: CampaignControlKindDeployed, Tasks: []Task{TaskContentFilter}},
			{SystemID: "normalized-control", Role: CampaignSystemRoleControl, ControlKind: CampaignControlKindNormalized, Tasks: []Task{TaskContentFilter}},
			{SystemID: "candidate", Role: CampaignSystemRoleCandidate, Tasks: []Task{TaskContentFilter}},
		},
		RouteSnapshots: []CampaignRouteSnapshot{
			{
				SystemID: "candidate", ExactEndpointEvidence: true,
				Snapshot: CampaignArtifactBinding{
					ArtifactID: "route-candidate",
					Path:       "/var/tmp/bitagent-llmeval/route-candidate.json",
					SHA256:     hash('f'),
				},
			},
		},
		Runs: []CampaignRun{
			{
				RunID: "run-deployed-control-contentfilter", SystemID: "deployed-control", Task: TaskContentFilter,
				CostCapMicroUSD: 250_000,
				Outputs: CampaignRunOutputs{
					ResultsPath:         "deployed-control/results.jsonl",
					AttemptEvidencePath: "deployed-control/attempts.json",
					ScorePath:           "deployed-control/score.json",
				},
			},
			{
				RunID: "run-normalized-control-contentfilter", SystemID: "normalized-control", Task: TaskContentFilter,
				CostCapMicroUSD: 200_000,
				Outputs: CampaignRunOutputs{
					ResultsPath:         "normalized-control/results.jsonl",
					AttemptEvidencePath: "normalized-control/attempts.json",
					ScorePath:           "normalized-control/score.json",
				},
			},
			{
				RunID: "run-candidate-contentfilter", SystemID: "candidate", Task: TaskContentFilter,
				CostCapMicroUSD: 250_000,
				Outputs: CampaignRunOutputs{
					ResultsPath:         "candidate/results.jsonl",
					AttemptEvidencePath: "candidate/attempts.json",
					ScorePath:           "candidate/score.json",
				},
			},
		},
		Comparisons: []CampaignComparison{
			{
				ComparisonID:   "compare-candidate-vs-deployed-contentfilter",
				ControlRunID:   "run-deployed-control-contentfilter",
				CandidateRunID: "run-candidate-contentfilter",
				OutputPath:     "comparisons/candidate-vs-deployed.json",
			},
			{
				ComparisonID:   "compare-candidate-vs-normalized-contentfilter",
				ControlRunID:   "run-normalized-control-contentfilter",
				CandidateRunID: "run-candidate-contentfilter",
				OutputPath:     "comparisons/candidate-vs-normalized.json",
			},
		},
		HistoricalBaselines: []CampaignHistoricalBaseline{
			{
				BaselineID:      "baseline-contentfilter-2026-07-27",
				Task:            TaskContentFilter,
				SystemID:        "historical-control",
				BenchmarkEpoch:  "2026-07-27",
				ProtocolVersion: "legacy-reference-v1",
				Artifact:        CampaignArtifactBinding{ArtifactID: "baseline-report", SHA256: hash('1')},
			},
		},
		EffectivenessGates: []CampaignEffectivenessGate{effectivenessGate},
	}
}

func testContentFilterCampaignGate() CampaignEffectivenessGate {
	return CampaignEffectivenessGate{
		Task: TaskContentFilter,
		Inference: CampaignInferenceGate{
			ConfidenceLevel:         CampaignConfidenceLevel,
			ProportionBoundMethod:   CampaignProportionBoundMethod,
			ControlDeltaBoundMethod: CampaignControlBoundMethod,
			BoundSemantics:          CampaignBoundSemantics,
			BootstrapReplicates:     DefaultComparisonOptions().BootstrapReplicates,
			BootstrapSeed:           DefaultComparisonOptions().BootstrapSeed,
		},
		HarmUtility: CampaignHarmUtilityGate{
			MaxSeverityOneErrors: 0,
			HarmMetric:           CampaignContentFilterHarm, MaxHarmRate: CampaignMaxAbsoluteHarmRate,
			UtilityMetric: CampaignContentFilterUtility, MinUtilityRate: 0.84,
			MaxHarmUCBDeltaVsControl:    CampaignMaxHarmUCBDelta,
			MinUtilityLCBDeltaVsControl: CampaignMinUtilityLCBDelta,
		},
		SchemaReliability: CampaignSchemaReliabilityGate{
			MinFirstPassSchemaValidRate: CampaignMinFirstPassValidRate,
			MinFinalSchemaValidRate:     1,
			MinAnsweredRate:             1,
			MaxSchemaRepairsPerCase:     1,
			MaxCallErrorUCB:             CampaignMaxTimeoutErrorUCB,
		},
		Latency: CampaignLatencyGate{
			DeadlineMS: 8_000,
			MaxP50MS:   1_000, MaxP95MS: 4_000, MaxP99MS: 8_000,
			MaxP95RelativeSlowdownVsControl: CampaignMaxLatencySlowdown,
			MaxTimeoutUCB:                   CampaignMaxTimeoutErrorUCB,
		},
		Stability: CampaignStabilityGate{
			MinRepeatAgreement: 0.98, MaxUtilityDrift: 0.02, MaxHarmDrift: 0.005,
			MaxHarmfulRepeatFlips: 0,
			MaxFinalActionFlipUCB: CampaignMaxFinalActionFlipUCB,
		},
		Calibration: &CampaignCalibrationGate{
			Metric: CampaignCalibrationMetricECE, MaxError: 0.1,
			MaxBrierScore: CampaignMaxBrierScore,
		},
		SelectiveRisk: &CampaignSelectiveRiskGate{
			MaxSelectiveRisk:            0.01,
			MaxAURC:                     0.05,
			MinCoverageRatioVsIncumbent: 1,
		},
		CriticalStrata: []CampaignCriticalStratumGate{
			{SliceID: "must_keep_english", MinimumCases: 100, MaxSeverityOneErrors: 0, MaxHarmRate: CampaignMaxAbsoluteHarmRate, MinUtilityRate: 0.8},
		},
		Cost: CampaignCostEffectivenessGate{
			MaxCostPer1000CasesMicroUSD:         100_000,
			MaxProjectedMonthlyMicroUSD:         5_000_000,
			MaxCostRatioVsControl:               1,
			MaxCostPerCorrectSafeActionMicroUSD: 1_000,
		},
	}
}

func testMatcherExtractCampaignGate() CampaignEffectivenessGate {
	return CampaignEffectivenessGate{
		Task: TaskMatcherExtract,
		Inference: CampaignInferenceGate{
			ConfidenceLevel:         CampaignConfidenceLevel,
			ProportionBoundMethod:   CampaignProportionBoundMethod,
			ControlDeltaBoundMethod: CampaignControlBoundMethod,
			BoundSemantics:          CampaignBoundSemantics,
			BootstrapReplicates:     DefaultComparisonOptions().BootstrapReplicates,
			BootstrapSeed:           DefaultComparisonOptions().BootstrapSeed,
		},
		HarmUtility: CampaignHarmUtilityGate{
			MaxSeverityOneErrors:        0,
			HarmMetric:                  CampaignMatcherExtractHarm,
			MaxHarmRate:                 CampaignMaxAbsoluteHarmRate,
			MaxHarmUCBDeltaVsControl:    CampaignMaxHarmUCBDelta,
			UtilityMetric:               CampaignMatcherExtractUtility,
			MinUtilityRate:              0.90,
			MinUtilityLCBDeltaVsControl: CampaignMinUtilityLCBDelta,
		},
		SchemaReliability: CampaignSchemaReliabilityGate{
			MinFirstPassSchemaValidRate: CampaignMinFirstPassValidRate,
			MinFinalSchemaValidRate:     1,
			MinAnsweredRate:             1,
			MaxSchemaRepairsPerCase:     1,
			MaxCallErrorUCB:             CampaignMaxTimeoutErrorUCB,
		},
		Latency: CampaignLatencyGate{
			DeadlineMS:                      CampaignTaskDeadlineMS(TaskMatcherExtract),
			MaxP50MS:                        5_000,
			MaxP95MS:                        30_000,
			MaxP99MS:                        48_000,
			MaxP95RelativeSlowdownVsControl: CampaignMaxLatencySlowdown,
			MaxTimeoutUCB:                   CampaignMaxTimeoutErrorUCB,
		},
		Stability: CampaignStabilityGate{
			MinRepeatAgreement:    0.98,
			MaxUtilityDrift:       0.02,
			MaxHarmDrift:          0.005,
			MaxHarmfulRepeatFlips: 0,
			MaxFinalActionFlipUCB: CampaignMaxFinalActionFlipUCB,
		},
		CriticalStrata: []CampaignCriticalStratumGate{
			{
				SliceID:              "must_extract",
				MinimumCases:         100,
				MaxSeverityOneErrors: 0,
				MaxHarmRate:          CampaignMaxAbsoluteHarmRate,
				MinUtilityRate:       0.80,
			},
		},
		Cost: CampaignCostEffectivenessGate{
			MaxCostPer1000CasesMicroUSD:         100_000,
			MaxProjectedMonthlyMicroUSD:         5_000_000,
			MaxCostRatioVsControl:               1,
			MaxCostPerCorrectSafeActionMicroUSD: 1_000,
		},
	}
}

func TestCampaignEffectivenessConfidenceGateApplicability(t *testing.T) {
	matcher := testMatcherExtractCampaignGate()
	require.NoError(t, matcher.Validate())
	raw, err := json.Marshal(matcher)
	require.NoError(t, err)
	require.NotContains(t, string(raw), `"calibration"`)
	require.NotContains(t, string(raw), `"selective_risk"`)

	t.Run("matcher rejects fabricated calibration", func(t *testing.T) {
		gate := matcher
		gate.Calibration = &CampaignCalibrationGate{
			Metric: CampaignCalibrationMetricECE, MaxError: 0.1,
			MaxBrierScore: CampaignMaxBrierScore,
		}
		require.ErrorContains(
			t,
			gate.Validate(),
			"must omit calibration and selective_risk",
		)
	})

	t.Run("matcher rejects fabricated selective risk", func(t *testing.T) {
		gate := matcher
		gate.SelectiveRisk = &CampaignSelectiveRiskGate{
			MaxSelectiveRisk: 0.01, MaxAURC: 0.05,
			MinCoverageRatioVsIncumbent: 1,
		}
		require.ErrorContains(
			t,
			gate.Validate(),
			"must omit calibration and selective_risk",
		)
	})

	t.Run("confidence task requires calibration", func(t *testing.T) {
		gate := testContentFilterCampaignGate()
		gate.Calibration = nil
		require.ErrorContains(t, gate.Validate(), "requires a calibration gate")
	})

	t.Run("confidence task requires selective risk", func(t *testing.T) {
		gate := testContentFilterCampaignGate()
		gate.SelectiveRisk = nil
		require.ErrorContains(t, gate.Validate(), "requires a selective_risk gate")
	})
}

func testCampaignManifest() SystemManifest {
	deployed := SystemConfig{
		SystemID: "deployed-control", Provider: "openai", Model: "gpt-5.6-luna",
		PromptVersion: "v2", EvaluationLane: EvaluationLaneProductionFidelity,
		APIKind: APIKindResponses, OutputContract: OutputContractPromptOnly,
		BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY",
		ProviderEndpoint: "api.openai.com", Tasks: []Task{TaskContentFilter},
		InputUSDPerMillion: 1, OutputUSDPerMillion: 6,
		NoThinkLocation: NoThinkLocationNone, OmitMaxCompletionTokens: true,
		RequestTimeoutMS: 8_000,
	}
	normalized := testDirectBatchSystem(APIKindChat, OutputContractJSONSchema, TaskContentFilter)
	normalized.SystemID = "normalized-control"
	normalized.RequestTimeoutMS = 8_000
	candidate := testOpenRouterSystem(TaskContentFilter)
	candidate.SystemID = "candidate"
	candidate.RequestTimeoutMS = 8_000
	return SystemManifest{
		SchemaVersion: SchemaVersion,
		Systems:       []SystemConfig{deployed, normalized, candidate},
	}
}

func readCheckedInCampaignCorpusPlan(t *testing.T) (CorpusPlan, string) {
	t.Helper()
	path := opsFixture(t, filepath.Join("..", "..", "ops", "llm-eval", "corpus-plan-v2.json"))
	file, err := os.Open(path)
	require.NoError(t, err)
	defer file.Close()
	plan, digest, err := ReadCorpusPlan(file)
	require.NoError(t, err)
	return plan, digest
}

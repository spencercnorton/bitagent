package llmeval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	CampaignSchemaVersion         = 1
	CampaignProtocolVersion       = "bitagent-llmeval-campaign-v1"
	SupersededCorpusPlanV1ID      = "bitagent-llm-corpus-v1-2026-07-24"
	maxCampaignPlanBytes          = 4 << 20
	maxCampaignRuns               = 10_000
	maxCampaignComparisons        = 100_000
	maxCampaignCostCapMicroUSD    = int64(1_000_000_000_000)
	CampaignCalibrationMetricECE  = "expected_calibration_error"
	CampaignMatcherExtractHarm    = "unsafe_identity_rate"
	CampaignMatcherExtractUtility = "exact_extraction_rate"
	CampaignMatcherRerankHarm     = "wrong_attachment_rate"
	CampaignMatcherRerankUtility  = "correct_attachment_rate"
	CampaignContentFilterHarm     = "english_rejection_rate"
	CampaignContentFilterUtility  = "balanced_accuracy"
	CampaignJunkPurgeHarm         = "keep_deletion_rate"
	CampaignJunkPurgeUtility      = "delete_recall"
	CampaignConfidenceLevel       = 0.95
	CampaignMaxHarmUCBDelta       = 0.0025
	CampaignMinUtilityLCBDelta    = -0.01
	CampaignMinFirstPassValidRate = 0.995
	CampaignMaxTimeoutErrorUCB    = 0.005
	CampaignMaxLatencySlowdown    = 0.20
	CampaignMaxFinalActionFlipUCB = 0.01
	CampaignMaxAbsoluteHarmRate   = 0.001
	CampaignMaxBrierScore         = 0.25
	CampaignMinShadowDays         = 7
	CampaignMinShadowCases        = 10_000
	CampaignProportionBoundMethod = "one_sided_wilson_score"
	CampaignControlBoundMethod    = "paired_bootstrap_percentile"
	CampaignBoundSemantics        = "harm_failure_flip_upper_utility_lower"
)

// CampaignStage is one pre-registered phase. A phase gets a new immutable
// campaign plan rather than silently reusing outputs selected in an earlier
// phase.
type CampaignStage string

const (
	CampaignStageProtocolScreen    CampaignStage = "protocol_screen"
	CampaignStageDevelopment       CampaignStage = "development"
	CampaignStageHoldout           CampaignStage = "holdout"
	CampaignStageRepeat            CampaignStage = "repeat"
	CampaignStageProspectiveShadow CampaignStage = "prospective_shadow"
)

type CampaignSystemRole string

const (
	CampaignSystemRoleControl   CampaignSystemRole = "control"
	CampaignSystemRoleCandidate CampaignSystemRole = "candidate"
)

type CampaignControlKind string

const (
	CampaignControlKindDeployed   CampaignControlKind = "deployed"
	CampaignControlKindNormalized CampaignControlKind = "normalized"
)

type CampaignAction string

const (
	CampaignActionValidate  CampaignAction = "validate_plan"
	CampaignActionRunShadow CampaignAction = "run_shadow"
	CampaignActionScore     CampaignAction = "score"
	CampaignActionCompare   CampaignAction = "compare"
)

// CampaignPlan is a source-text-free, immutable execution declaration. It
// binds all inputs by SHA-256 and only permits shadow evaluation actions.
type CampaignPlan struct {
	SchemaVersion        int                          `json:"schema_version"`
	CampaignID           string                       `json:"campaign_id"`
	BenchmarkEpoch       string                       `json:"benchmark_epoch"`
	ProtocolVersion      string                       `json:"protocol_version"`
	ShadowOnly           bool                         `json:"shadow_only"`
	Stage                CampaignStage                `json:"stage"`
	CorpusPlanID         string                       `json:"corpus_plan_id"`
	CorpusPlanSHA256     string                       `json:"corpus_plan_sha256"`
	SystemManifestSHA256 string                       `json:"system_manifest_sha256"`
	OutputRoot           string                       `json:"output_root"`
	SummaryPath          string                       `json:"summary_path"`
	AllowedActions       []CampaignAction             `json:"allowed_actions"`
	TotalCostCapMicroUSD int64                        `json:"total_cost_cap_micro_usd"`
	TaskArtifacts        []CampaignTaskArtifacts      `json:"task_artifacts"`
	Systems              []CampaignSystemMatrix       `json:"systems"`
	RouteSnapshots       []CampaignRouteSnapshot      `json:"route_snapshots,omitempty"`
	Runs                 []CampaignRun                `json:"runs"`
	Comparisons          []CampaignComparison         `json:"comparisons"`
	HistoricalBaselines  []CampaignHistoricalBaseline `json:"historical_baselines"`
	EffectivenessGates   []CampaignEffectivenessGate  `json:"effectiveness_gates"`
	ProspectiveShadow    *CampaignProspectiveShadow   `json:"prospective_shadow,omitempty"`
}

// CampaignArtifactBinding names an artifact without exposing its contents.
// Path may be omitted in a portable plan; if present the CLI hashes the exact
// local bytes during preflight.
type CampaignArtifactBinding struct {
	ArtifactID string `json:"artifact_id"`
	Path       string `json:"path,omitempty"`
	SHA256     string `json:"sha256"`
}

type CampaignTaskArtifacts struct {
	Task           Task                     `json:"task"`
	Corpus         CampaignArtifactBinding  `json:"corpus"`
	GoldClosure    *CampaignArtifactBinding `json:"gold_closure,omitempty"`
	PrivacySidecar *CampaignArtifactBinding `json:"privacy_sidecar,omitempty"`
}

// CampaignSystemMatrix explicitly freezes which systems participate in which
// tasks and whether each is a control or a candidate.
type CampaignSystemMatrix struct {
	SystemID    string              `json:"system_id"`
	Role        CampaignSystemRole  `json:"role"`
	ControlKind CampaignControlKind `json:"control_kind,omitempty"`
	Tasks       []Task              `json:"tasks"`
}

type CampaignRouteSnapshot struct {
	SystemID              string                  `json:"system_id"`
	ExactEndpointEvidence bool                    `json:"exact_endpoint_evidence"`
	Snapshot              CampaignArtifactBinding `json:"snapshot"`
}

type CampaignRun struct {
	RunID           string             `json:"run_id"`
	SystemID        string             `json:"system_id"`
	Task            Task               `json:"task"`
	RepeatIndex     int                `json:"repeat_index,omitempty"`
	CostCapMicroUSD int64              `json:"cost_cap_micro_usd"`
	Outputs         CampaignRunOutputs `json:"outputs"`
}

// All output paths are relative to OutputRoot. This makes a single external
// artifact root enforceable before any provider call.
type CampaignRunOutputs struct {
	ResultsPath         string `json:"results_path"`
	AttemptEvidencePath string `json:"attempt_evidence_path"`
	ScorePath           string `json:"score_path"`
}

// CampaignComparison preregisters one pairwise report. The matrix is complete:
// every candidate run is compared with every same-task/same-repeat control run,
// including at least one deployed and one normalized control.
type CampaignComparison struct {
	ComparisonID   string `json:"comparison_id"`
	ControlRunID   string `json:"control_run_id"`
	CandidateRunID string `json:"candidate_run_id"`
	OutputPath     string `json:"output_path"`
}

// CampaignBoundArtifact is the path-free identity stamped into execution
// evidence. Local paths remain confined to the preflight plan and never enter
// result rows that may later be shared for review or promotion.
type CampaignBoundArtifact struct {
	ArtifactID string `json:"artifact_id"`
	SHA256     string `json:"sha256"`
}

// CampaignRunBinding is the complete, path-free reconstruction key for one
// pre-registered run. New campaign executions stamp this value into every
// result row; resume and promotion then compare it to a binding derived from
// the exact campaign-plan bytes instead of trusting a separately supplied
// run ID or report label.
type CampaignRunBinding struct {
	SchemaVersion         int                    `json:"schema_version"`
	CampaignID            string                 `json:"campaign_id"`
	CampaignSHA256        string                 `json:"campaign_sha256"`
	BenchmarkEpoch        string                 `json:"benchmark_epoch"`
	ProtocolVersion       string                 `json:"protocol_version"`
	ShadowOnly            bool                   `json:"shadow_only"`
	Stage                 CampaignStage          `json:"stage"`
	RunID                 string                 `json:"run_id"`
	RepeatIndex           int                    `json:"repeat_index,omitempty"`
	SystemID              string                 `json:"system_id"`
	Task                  Task                   `json:"task"`
	Role                  CampaignSystemRole     `json:"role"`
	ControlKind           CampaignControlKind    `json:"control_kind,omitempty"`
	CostCapMicroUSD       int64                  `json:"cost_cap_micro_usd"`
	SystemManifestSHA256  string                 `json:"system_manifest_sha256"`
	CorpusPlanID          string                 `json:"corpus_plan_id"`
	CorpusPlanSHA256      string                 `json:"corpus_plan_sha256"`
	Corpus                CampaignBoundArtifact  `json:"corpus"`
	GoldClosure           *CampaignBoundArtifact `json:"gold_closure,omitempty"`
	PrivacySidecar        *CampaignBoundArtifact `json:"privacy_sidecar,omitempty"`
	RouteSnapshot         *CampaignBoundArtifact `json:"route_snapshot,omitempty"`
	ExactEndpointEvidence bool                   `json:"exact_endpoint_evidence,omitempty"`
	Outputs               CampaignRunOutputs     `json:"outputs"`
}

type CampaignHistoricalBaseline struct {
	BaselineID        string                  `json:"baseline_id"`
	Task              Task                    `json:"task"`
	SystemID          string                  `json:"system_id"`
	BenchmarkEpoch    string                  `json:"benchmark_epoch"`
	ProtocolVersion   string                  `json:"protocol_version"`
	PromotionEligible bool                    `json:"promotion_eligible"`
	Artifact          CampaignArtifactBinding `json:"artifact"`
}

// CampaignEffectivenessGate pre-registers all six independent decision
// dimensions. A cheap system cannot pass by trading hidden harm or timeouts
// for an attractive aggregate accuracy number.
type CampaignEffectivenessGate struct {
	Task              Task                          `json:"task"`
	Inference         CampaignInferenceGate         `json:"inference"`
	HarmUtility       CampaignHarmUtilityGate       `json:"harm_utility"`
	SchemaReliability CampaignSchemaReliabilityGate `json:"schema_reliability"`
	Latency           CampaignLatencyGate           `json:"latency"`
	Stability         CampaignStabilityGate         `json:"stability"`
	// Calibration and selective risk require a normalized confidence value.
	// Matcher-extract deliberately has no confidence in its result contract,
	// so those gates must be omitted instead of populated with invented data.
	Calibration    *CampaignCalibrationGate      `json:"calibration,omitempty"`
	SelectiveRisk  *CampaignSelectiveRiskGate    `json:"selective_risk,omitempty"`
	CriticalStrata []CampaignCriticalStratumGate `json:"critical_strata"`
	Cost           CampaignCostEffectivenessGate `json:"cost"`
}

// CampaignInferenceGate freezes the directional uncertainty semantics. Harm,
// failure, and flip rates use one-sided upper bounds; utility uses a one-sided
// lower bound. Control deltas are paired on identical cases.
type CampaignInferenceGate struct {
	ConfidenceLevel         float64 `json:"confidence_level"`
	ProportionBoundMethod   string  `json:"proportion_bound_method"`
	ControlDeltaBoundMethod string  `json:"control_delta_bound_method"`
	BoundSemantics          string  `json:"bound_semantics"`
	BootstrapReplicates     int     `json:"bootstrap_replicates"`
	BootstrapSeed           uint64  `json:"bootstrap_seed"`
}

type CampaignHarmUtilityGate struct {
	MaxSeverityOneErrors        int     `json:"max_severity_one_errors"`
	HarmMetric                  string  `json:"harm_metric"`
	MaxHarmRate                 float64 `json:"max_harm_rate"`
	MaxHarmUCBDeltaVsControl    float64 `json:"max_harm_ucb_delta_vs_control"`
	UtilityMetric               string  `json:"utility_metric"`
	MinUtilityRate              float64 `json:"min_utility_rate"`
	MinUtilityLCBDeltaVsControl float64 `json:"min_utility_lcb_delta_vs_control"`
}

type CampaignSchemaReliabilityGate struct {
	MinFirstPassSchemaValidRate float64 `json:"min_first_pass_schema_valid_rate"`
	MinFinalSchemaValidRate     float64 `json:"min_final_schema_valid_rate"`
	MinAnsweredRate             float64 `json:"min_answered_rate"`
	MaxSchemaRepairsPerCase     int     `json:"max_schema_repairs_per_case"`
	MaxCallErrorUCB             float64 `json:"max_call_error_ucb"`
}

type CampaignLatencyGate struct {
	DeadlineMS                      int64   `json:"deadline_ms"`
	MaxP50MS                        int64   `json:"max_p50_ms"`
	MaxP95MS                        int64   `json:"max_p95_ms"`
	MaxP99MS                        int64   `json:"max_p99_ms"`
	MaxP95RelativeSlowdownVsControl float64 `json:"max_p95_relative_slowdown_vs_control"`
	MaxTimeoutUCB                   float64 `json:"max_timeout_ucb"`
}

type CampaignStabilityGate struct {
	MinRepeatAgreement    float64 `json:"min_repeat_agreement"`
	MaxUtilityDrift       float64 `json:"max_utility_drift"`
	MaxHarmDrift          float64 `json:"max_harm_drift"`
	MaxHarmfulRepeatFlips int     `json:"max_harmful_repeat_flips"`
	MaxFinalActionFlipUCB float64 `json:"max_final_action_flip_ucb"`
}

type CampaignCalibrationGate struct {
	Metric        string  `json:"metric"`
	MaxError      float64 `json:"max_error"`
	MaxBrierScore float64 `json:"max_brier_score"`
}

type CampaignSelectiveRiskGate struct {
	MaxSelectiveRisk            float64 `json:"max_selective_risk"`
	MaxAURC                     float64 `json:"max_aurc"`
	MinCoverageRatioVsIncumbent float64 `json:"min_coverage_ratio_vs_incumbent"`
}

type CampaignCriticalStratumGate struct {
	SliceID              string  `json:"slice_id"`
	MinimumCases         int     `json:"minimum_cases"`
	MaxSeverityOneErrors int     `json:"max_severity_one_errors"`
	MaxHarmRate          float64 `json:"max_harm_rate"`
	MinUtilityRate       float64 `json:"min_utility_rate"`
}

type CampaignCostEffectivenessGate struct {
	MaxCostPer1000CasesMicroUSD         int64   `json:"max_cost_per_1000_cases_micro_usd"`
	MaxProjectedMonthlyMicroUSD         int64   `json:"max_projected_monthly_micro_usd"`
	MaxCostRatioVsControl               float64 `json:"max_cost_ratio_vs_control"`
	MaxCostPerCorrectSafeActionMicroUSD int64   `json:"max_cost_per_correct_safe_action_micro_usd"`
}

type CampaignProspectiveShadow struct {
	MinimumDurationDays int `json:"minimum_duration_days"`
	MinimumCases        int `json:"minimum_cases"`
}

// CampaignExecution is the deterministic, source-free matrix emitted by the
// offline CLI preflight.
type CampaignExecution struct {
	RunID           string             `json:"run_id"`
	Stage           CampaignStage      `json:"stage"`
	Task            Task               `json:"task"`
	SystemID        string             `json:"system_id"`
	Role            CampaignSystemRole `json:"role"`
	RepeatIndex     int                `json:"repeat_index,omitempty"`
	CostCapMicroUSD int64              `json:"cost_cap_micro_usd"`
}

// CampaignArtifactReference lets the CLI verify exact bytes without coupling
// the internal contract package to the filesystem.
type CampaignArtifactReference struct {
	Name   string
	Path   string
	SHA256 string
}

func ReadCampaignPlan(r io.Reader) (CampaignPlan, string, error) {
	if r == nil {
		return CampaignPlan{}, "", fmt.Errorf("campaign plan reader is nil")
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxCampaignPlanBytes+1))
	if err != nil {
		return CampaignPlan{}, "", fmt.Errorf("read campaign plan: %w", err)
	}
	if len(raw) == 0 {
		return CampaignPlan{}, "", fmt.Errorf("campaign plan is empty")
	}
	if len(raw) > maxCampaignPlanBytes {
		return CampaignPlan{}, "", fmt.Errorf("campaign plan exceeds the %d-byte limit", maxCampaignPlanBytes)
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return CampaignPlan{}, "", fmt.Errorf("decode campaign plan: %w", err)
	}
	var plan CampaignPlan
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return CampaignPlan{}, "", fmt.Errorf("decode campaign plan: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return CampaignPlan{}, "", fmt.Errorf("decode campaign plan: multiple JSON values")
		}
		return CampaignPlan{}, "", fmt.Errorf("decode campaign plan: trailing data: %w", err)
	}
	if err := plan.Validate(); err != nil {
		return CampaignPlan{}, "", err
	}
	return plan, sha256Hex(raw), nil
}

func (p CampaignPlan) Validate() error {
	if p.SchemaVersion != CampaignSchemaVersion {
		return fmt.Errorf("campaign schema_version: got %d, want %d", p.SchemaVersion, CampaignSchemaVersion)
	}
	if err := validateIdentifier("campaign_id", p.CampaignID); err != nil {
		return err
	}
	if _, err := time.Parse("2006-01-02", p.BenchmarkEpoch); err != nil {
		return fmt.Errorf("benchmark_epoch: must be YYYY-MM-DD")
	}
	if p.ProtocolVersion != CampaignProtocolVersion {
		return fmt.Errorf("protocol_version: got %q, want %q", p.ProtocolVersion, CampaignProtocolVersion)
	}
	if !p.ShadowOnly {
		return fmt.Errorf("shadow_only must be true")
	}
	if !p.Stage.valid() {
		return fmt.Errorf("stage %q is unsupported", p.Stage)
	}
	if p.Stage == CampaignStageProspectiveShadow {
		if p.ProspectiveShadow == nil {
			return fmt.Errorf("prospective_shadow is required for prospective_shadow stage")
		}
		if p.ProspectiveShadow.MinimumDurationDays < CampaignMinShadowDays ||
			p.ProspectiveShadow.MinimumCases < CampaignMinShadowCases {
			return fmt.Errorf("prospective_shadow requires at least %d days and %d cases", CampaignMinShadowDays, CampaignMinShadowCases)
		}
	} else if p.ProspectiveShadow != nil {
		return fmt.Errorf("prospective_shadow is only valid for prospective_shadow stage")
	}
	if err := validateIdentifier("corpus_plan_id", p.CorpusPlanID); err != nil {
		return err
	}
	if p.CorpusPlanID == SupersededCorpusPlanV1ID {
		return fmt.Errorf("corpus_plan_id %q is superseded and cannot start a campaign", p.CorpusPlanID)
	}
	if err := validateSHA256("corpus_plan_sha256", p.CorpusPlanSHA256); err != nil {
		return err
	}
	if err := validateSHA256("system_manifest_sha256", p.SystemManifestSHA256); err != nil {
		return err
	}
	if err := validateAbsoluteCampaignPath("output_root", p.OutputRoot); err != nil {
		return err
	}
	if filepath.Clean(p.OutputRoot) == string(filepath.Separator) {
		return fmt.Errorf("output_root must not be the filesystem root")
	}
	if err := validateRelativeOutputPath("summary_path", p.SummaryPath); err != nil {
		return err
	}
	if err := validateCampaignActions(p.AllowedActions); err != nil {
		return err
	}
	if p.TotalCostCapMicroUSD <= 0 || p.TotalCostCapMicroUSD > maxCampaignCostCapMicroUSD {
		return fmt.Errorf("total_cost_cap_micro_usd must be positive and at most %d", maxCampaignCostCapMicroUSD)
	}
	if len(p.Systems) == 0 {
		return fmt.Errorf("systems must not be empty")
	}
	if len(p.Runs) == 0 || len(p.Runs) > maxCampaignRuns {
		return fmt.Errorf("runs must contain 1..%d entries", maxCampaignRuns)
	}

	taskSet, err := p.validateTaskArtifactsAndGates()
	if err != nil {
		return err
	}
	systemRoles, matrix, err := p.validateSystemMatrix(taskSet)
	if err != nil {
		return err
	}
	if err := p.validateRouteSnapshots(systemRoles); err != nil {
		return err
	}
	if err := p.validateRuns(matrix); err != nil {
		return err
	}
	if err := p.validateComparisons(systemRoles); err != nil {
		return err
	}
	if err := p.validateHistoricalBaselines(taskSet); err != nil {
		return err
	}
	if err := p.validateGlobalArtifactIDs(); err != nil {
		return err
	}
	return p.validateOutputPaths()
}

// ValidateExecutableArtifacts upgrades a portable, hash-only campaign into an
// executable preflight. Every promotion-relevant input must resolve to exact
// local bytes; portable lint is intentionally a separate CLI mode.
func (p CampaignPlan) ValidateExecutableArtifacts() error {
	if err := p.Validate(); err != nil {
		return err
	}
	for i, entry := range p.TaskArtifacts {
		if err := validateCampaignArtifact(
			fmt.Sprintf("task_artifacts[%d].corpus", i),
			entry.Corpus,
			true,
		); err != nil {
			return err
		}
		if entry.GoldClosure == nil || entry.PrivacySidecar == nil {
			return fmt.Errorf("task_artifacts[%d]: executable preflight requires gold_closure and privacy_sidecar", i)
		}
		if err := validateCampaignArtifact(
			fmt.Sprintf("task_artifacts[%d].gold_closure", i),
			*entry.GoldClosure,
			true,
		); err != nil {
			return err
		}
		if err := validateCampaignArtifact(
			fmt.Sprintf("task_artifacts[%d].privacy_sidecar", i),
			*entry.PrivacySidecar,
			true,
		); err != nil {
			return err
		}
	}
	for i, route := range p.RouteSnapshots {
		if err := validateCampaignArtifact(
			fmt.Sprintf("route_snapshots[%d].snapshot", i),
			route.Snapshot,
			true,
		); err != nil {
			return err
		}
	}
	for i, baseline := range p.HistoricalBaselines {
		if err := validateCampaignArtifact(
			fmt.Sprintf("historical_baselines[%d].artifact", i),
			baseline.Artifact,
			true,
		); err != nil {
			return err
		}
	}
	return nil
}

// ValidateBindings binds the plan to exact locally supplied contracts and
// rejects output placement inside the current source worktree.
func (p CampaignPlan) ValidateBindings(
	manifest SystemManifest,
	manifestSHA256 string,
	corpusPlan CorpusPlan,
	corpusPlanSHA256 string,
	worktreeRoot string,
) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if err := manifest.Validate(); err != nil {
		return fmt.Errorf("system manifest: %w", err)
	}
	if err := corpusPlan.Validate(); err != nil {
		return fmt.Errorf("corpus plan: %w", err)
	}
	if corpusPlan.PlanID == SupersededCorpusPlanV1ID {
		return fmt.Errorf("corpus plan %q is superseded and cannot start a campaign", corpusPlan.PlanID)
	}
	if p.SystemManifestSHA256 != manifestSHA256 {
		return fmt.Errorf("system manifest bytes do not match system_manifest_sha256")
	}
	if p.CorpusPlanID != corpusPlan.PlanID || p.CorpusPlanSHA256 != corpusPlanSHA256 {
		return fmt.Errorf("corpus plan identity does not match campaign binding")
	}
	if p.Stage.formal() {
		if err := p.validateEffectivenessSafetyBindings(corpusPlan); err != nil {
			return err
		}
	}
	if worktreeRoot != "" {
		inside, err := campaignPathWithin(worktreeRoot, p.OutputRoot)
		if err != nil {
			return fmt.Errorf("output_root: %w", err)
		}
		if inside {
			return fmt.Errorf("output_root must be outside the Git worktree")
		}
	}

	manifestSystems := make(map[string]SystemConfig, len(manifest.Systems))
	for _, system := range manifest.Systems {
		manifestSystems[system.SystemID] = system
	}
	gates := make(map[Task]CampaignEffectivenessGate, len(p.EffectivenessGates))
	for _, gate := range p.EffectivenessGates {
		gates[gate.Task] = gate
	}
	referenced := make(map[string]struct{}, len(p.Systems))
	for i, planned := range p.Systems {
		system, exists := manifestSystems[planned.SystemID]
		if !exists {
			return fmt.Errorf("systems[%d]: system_id %q is absent from the bound manifest", i, planned.SystemID)
		}
		if planned.Role == CampaignSystemRoleControl {
			switch planned.ControlKind {
			case CampaignControlKindDeployed:
				if !system.UsesDeployedWireContract() {
					return fmt.Errorf("systems[%d]: deployed control %q must use a deployed wire-contract lane", i, planned.SystemID)
				}
			case CampaignControlKindNormalized:
				if system.EvaluationLane != EvaluationLaneNormalizedStrict {
					return fmt.Errorf("systems[%d]: normalized control %q must use normalized_strict lane", i, planned.SystemID)
				}
			}
		}
		referenced[planned.SystemID] = struct{}{}
		for _, task := range planned.Tasks {
			if !system.SupportsTask(task) {
				return fmt.Errorf("systems[%d]: manifest system %q does not support task %q", i, planned.SystemID, task)
			}
			if system.RequestTimeoutMS != gates[task].Latency.DeadlineMS {
				return fmt.Errorf("systems[%d]: manifest system %q request_timeout_ms must equal task %q deadline %d", i, planned.SystemID, task, gates[task].Latency.DeadlineMS)
			}
		}
	}
	routes := make(map[string]CampaignRouteSnapshot, len(p.RouteSnapshots))
	for _, route := range p.RouteSnapshots {
		system := manifestSystems[route.SystemID]
		if system.Provider != "openrouter" {
			return fmt.Errorf("route snapshot for non-OpenRouter system %q is not permitted", route.SystemID)
		}
		_, quantization := splitOpenRouterProviderEndpoint(system.ProviderEndpoint)
		if p.Stage.formal() {
			if quantization == "" || quantization == "unknown" {
				return fmt.Errorf("formal OpenRouter system %q requires an explicit recognized quantization", route.SystemID)
			}
			if !route.ExactEndpointEvidence {
				return fmt.Errorf("formal OpenRouter system %q requires exact_endpoint_evidence", route.SystemID)
			}
		} else if (quantization == "" || quantization == "unknown") && route.ExactEndpointEvidence {
			return fmt.Errorf("protocol-screen provider-only route %q cannot claim exact_endpoint_evidence", route.SystemID)
		}
		routes[route.SystemID] = route
	}
	for systemID := range referenced {
		system := manifestSystems[systemID]
		if system.Provider == "openrouter" {
			if _, exists := routes[systemID]; !exists {
				return fmt.Errorf("openrouter system %q requires exactly one route snapshot", systemID)
			}
		}
	}
	return nil
}

// validateEffectivenessSafetyBindings prevents a formal campaign from
// declaring a narrower safety gate than the corpus contract it binds. The
// promotion path consumes these exact campaign gates, so enforcing coverage
// here also prevents a later promotion decision from silently dropping a
// preregistered safety slice.
func (p CampaignPlan) validateEffectivenessSafetyBindings(corpusPlan CorpusPlan) error {
	corpusTasks := make(map[Task]CorpusPlanTask, len(corpusPlan.Tasks))
	for _, taskPlan := range corpusPlan.Tasks {
		corpusTasks[taskPlan.Task] = taskPlan
	}

	for gateIndex, gate := range p.EffectivenessGates {
		taskPlan, exists := corpusTasks[gate.Task]
		if !exists {
			return fmt.Errorf(
				"effectiveness_gates[%d]: task %q is absent from the bound corpus plan",
				gateIndex,
				gate.Task,
			)
		}

		strata := make(map[string]CampaignCriticalStratumGate, len(gate.CriticalStrata))
		for _, stratum := range gate.CriticalStrata {
			strata[stratum.SliceID] = stratum
		}
		for _, sliceID := range taskPlan.RequiredSafetySlices {
			if _, exists := strata[sliceID]; !exists {
				return fmt.Errorf(
					"effectiveness_gates[%d].critical_strata: task %q is missing corpus-plan-required safety slice %q",
					gateIndex,
					gate.Task,
					sliceID,
				)
			}
		}

		countedSlices := make([]string, 0, len(taskPlan.RequiredSafetyCounts))
		for sliceID := range taskPlan.RequiredSafetyCounts {
			countedSlices = append(countedSlices, sliceID)
		}
		sort.Strings(countedSlices)
		for _, sliceID := range countedSlices {
			requiredCases := taskPlan.RequiredSafetyCounts[sliceID]
			stratum, exists := strata[sliceID]
			if !exists {
				return fmt.Errorf(
					"effectiveness_gates[%d].critical_strata: task %q is missing corpus-plan-required safety count slice %q",
					gateIndex,
					gate.Task,
					sliceID,
				)
			}
			if stratum.MinimumCases < requiredCases {
				return fmt.Errorf(
					"effectiveness_gates[%d].critical_strata: task %q slice %q minimum_cases %d is below corpus-plan-required safety count %d",
					gateIndex,
					gate.Task,
					sliceID,
					stratum.MinimumCases,
					requiredCases,
				)
			}
		}
	}
	return nil
}

func (p CampaignPlan) ExecutionMatrix() []CampaignExecution {
	roles := make(map[string]CampaignSystemRole, len(p.Systems))
	for _, system := range p.Systems {
		roles[system.SystemID] = system.Role
	}
	matrix := make([]CampaignExecution, 0, len(p.Runs))
	for _, run := range p.Runs {
		matrix = append(matrix, CampaignExecution{
			RunID:           run.RunID,
			Stage:           p.Stage,
			Task:            run.Task,
			SystemID:        run.SystemID,
			Role:            roles[run.SystemID],
			RepeatIndex:     run.RepeatIndex,
			CostCapMicroUSD: run.CostCapMicroUSD,
		})
	}
	sort.Slice(matrix, func(i, j int) bool {
		left, right := matrix[i], matrix[j]
		if taskOrder(left.Task) != taskOrder(right.Task) {
			return taskOrder(left.Task) < taskOrder(right.Task)
		}
		if left.Role != right.Role {
			return campaignRoleOrder(left.Role) < campaignRoleOrder(right.Role)
		}
		if left.SystemID != right.SystemID {
			return left.SystemID < right.SystemID
		}
		if left.RepeatIndex != right.RepeatIndex {
			return left.RepeatIndex < right.RepeatIndex
		}
		return left.RunID < right.RunID
	})
	return matrix
}

func (p CampaignPlan) ArtifactReferences() []CampaignArtifactReference {
	refs := make([]CampaignArtifactReference, 0, len(p.TaskArtifacts)*3+len(p.RouteSnapshots)+len(p.HistoricalBaselines))
	appendRef := func(name string, artifact *CampaignArtifactBinding) {
		if artifact == nil {
			return
		}
		refs = append(refs, CampaignArtifactReference{Name: name, Path: artifact.Path, SHA256: artifact.SHA256})
	}
	for i := range p.TaskArtifacts {
		artifacts := &p.TaskArtifacts[i]
		appendRef(fmt.Sprintf("task_artifacts[%d].corpus", i), &artifacts.Corpus)
		appendRef(fmt.Sprintf("task_artifacts[%d].gold_closure", i), artifacts.GoldClosure)
		appendRef(fmt.Sprintf("task_artifacts[%d].privacy_sidecar", i), artifacts.PrivacySidecar)
	}
	for i := range p.RouteSnapshots {
		appendRef(fmt.Sprintf("route_snapshots[%d].snapshot", i), &p.RouteSnapshots[i].Snapshot)
	}
	for i := range p.HistoricalBaselines {
		appendRef(fmt.Sprintf("historical_baselines[%d].artifact", i), &p.HistoricalBaselines[i].Artifact)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs
}

// RunBinding derives the exact row-level execution binding for runID. The
// caller must supply the digest returned by ReadCampaignPlan so formatting and
// other byte-level plan changes produce a distinct campaign identity.
func (p CampaignPlan) RunBinding(planSHA256, runID string) (CampaignRunBinding, error) {
	if err := p.Validate(); err != nil {
		return CampaignRunBinding{}, err
	}
	if err := validateSHA256("campaign_sha256", planSHA256); err != nil {
		return CampaignRunBinding{}, err
	}
	var selected *CampaignRun
	for i := range p.Runs {
		if p.Runs[i].RunID == runID {
			selected = &p.Runs[i]
			break
		}
	}
	if selected == nil {
		return CampaignRunBinding{}, fmt.Errorf("run_id %q is absent from the campaign plan", runID)
	}
	var plannedSystem *CampaignSystemMatrix
	for i := range p.Systems {
		if p.Systems[i].SystemID == selected.SystemID {
			plannedSystem = &p.Systems[i]
			break
		}
	}
	var taskArtifacts *CampaignTaskArtifacts
	for i := range p.TaskArtifacts {
		if p.TaskArtifacts[i].Task == selected.Task {
			taskArtifacts = &p.TaskArtifacts[i]
			break
		}
	}
	if plannedSystem == nil || taskArtifacts == nil {
		return CampaignRunBinding{}, fmt.Errorf("run_id %q has incomplete campaign bindings", runID)
	}

	binding := CampaignRunBinding{
		SchemaVersion:        CampaignSchemaVersion,
		CampaignID:           p.CampaignID,
		CampaignSHA256:       planSHA256,
		BenchmarkEpoch:       p.BenchmarkEpoch,
		ProtocolVersion:      p.ProtocolVersion,
		ShadowOnly:           p.ShadowOnly,
		Stage:                p.Stage,
		RunID:                selected.RunID,
		RepeatIndex:          selected.RepeatIndex,
		SystemID:             selected.SystemID,
		Task:                 selected.Task,
		Role:                 plannedSystem.Role,
		ControlKind:          plannedSystem.ControlKind,
		CostCapMicroUSD:      selected.CostCapMicroUSD,
		SystemManifestSHA256: p.SystemManifestSHA256,
		CorpusPlanID:         p.CorpusPlanID,
		CorpusPlanSHA256:     p.CorpusPlanSHA256,
		Corpus:               campaignBoundArtifact(taskArtifacts.Corpus),
		GoldClosure:          campaignOptionalBoundArtifact(taskArtifacts.GoldClosure),
		PrivacySidecar:       campaignOptionalBoundArtifact(taskArtifacts.PrivacySidecar),
		Outputs:              selected.Outputs,
	}
	for _, route := range p.RouteSnapshots {
		if route.SystemID == selected.SystemID {
			bound := campaignBoundArtifact(route.Snapshot)
			binding.RouteSnapshot = &bound
			binding.ExactEndpointEvidence = route.ExactEndpointEvidence
			break
		}
	}
	if err := binding.Validate(); err != nil {
		return CampaignRunBinding{}, fmt.Errorf("derived run binding: %w", err)
	}
	return binding, nil
}

func (b CampaignRunBinding) Validate() error {
	if b.SchemaVersion != CampaignSchemaVersion {
		return fmt.Errorf("campaign run binding schema_version: got %d, want %d", b.SchemaVersion, CampaignSchemaVersion)
	}
	if err := validateIdentifier("campaign_id", b.CampaignID); err != nil {
		return err
	}
	if err := validateSHA256("campaign_sha256", b.CampaignSHA256); err != nil {
		return err
	}
	if _, err := time.Parse("2006-01-02", b.BenchmarkEpoch); err != nil {
		return fmt.Errorf("benchmark_epoch: must be YYYY-MM-DD")
	}
	if b.ProtocolVersion != CampaignProtocolVersion {
		return fmt.Errorf("protocol_version: got %q, want %q", b.ProtocolVersion, CampaignProtocolVersion)
	}
	if !b.ShadowOnly {
		return fmt.Errorf("shadow_only must be true")
	}
	if !b.Stage.valid() {
		return fmt.Errorf("stage %q is unsupported", b.Stage)
	}
	if err := validateIdentifier("run_id", b.RunID); err != nil {
		return err
	}
	if b.Stage == CampaignStageRepeat {
		if b.RepeatIndex <= 0 {
			return fmt.Errorf("repeat_index must be positive in repeat stage")
		}
	} else if b.RepeatIndex != 0 {
		return fmt.Errorf("repeat_index is only valid in repeat stage")
	}
	if err := validateIdentifier("system_id", b.SystemID); err != nil {
		return err
	}
	if harm, _ := campaignTaskMetrics(b.Task); harm == "" {
		return fmt.Errorf("task %q is unsupported", b.Task)
	}
	if b.Role == CampaignSystemRoleControl {
		if b.ControlKind != CampaignControlKindDeployed && b.ControlKind != CampaignControlKindNormalized {
			return fmt.Errorf("control run binding requires deployed or normalized control_kind")
		}
	} else if b.Role != CampaignSystemRoleCandidate {
		return fmt.Errorf("role %q is unsupported", b.Role)
	} else if b.ControlKind != "" {
		return fmt.Errorf("candidate run binding must not declare control_kind")
	}
	if b.CostCapMicroUSD <= 0 || b.CostCapMicroUSD > maxCampaignCostCapMicroUSD {
		return fmt.Errorf("cost_cap_micro_usd must be positive and at most %d", maxCampaignCostCapMicroUSD)
	}
	if err := validateSHA256("system_manifest_sha256", b.SystemManifestSHA256); err != nil {
		return err
	}
	if err := validateIdentifier("corpus_plan_id", b.CorpusPlanID); err != nil {
		return err
	}
	if err := validateSHA256("corpus_plan_sha256", b.CorpusPlanSHA256); err != nil {
		return err
	}
	if err := validateCampaignBoundArtifact("corpus", b.Corpus); err != nil {
		return err
	}
	for name, artifact := range map[string]*CampaignBoundArtifact{
		"gold_closure":    b.GoldClosure,
		"privacy_sidecar": b.PrivacySidecar,
		"route_snapshot":  b.RouteSnapshot,
	} {
		if artifact != nil {
			if err := validateCampaignBoundArtifact(name, *artifact); err != nil {
				return err
			}
		}
	}
	if b.Stage.formal() && (b.GoldClosure == nil || b.PrivacySidecar == nil) {
		return fmt.Errorf("formal run binding requires gold_closure and privacy_sidecar")
	}
	if b.ExactEndpointEvidence && b.RouteSnapshot == nil {
		return fmt.Errorf("exact_endpoint_evidence requires route_snapshot")
	}
	seenOutputs := make(map[string]struct{}, 3)
	for name, path := range map[string]string{
		"outputs.results_path":          b.Outputs.ResultsPath,
		"outputs.attempt_evidence_path": b.Outputs.AttemptEvidencePath,
		"outputs.score_path":            b.Outputs.ScorePath,
	} {
		if err := validateRelativeOutputPath(name, path); err != nil {
			return err
		}
		if _, duplicate := seenOutputs[path]; duplicate {
			return fmt.Errorf("%s collides with another run output", name)
		}
		seenOutputs[path] = struct{}{}
	}
	return nil
}

func campaignBoundArtifact(artifact CampaignArtifactBinding) CampaignBoundArtifact {
	return CampaignBoundArtifact{ArtifactID: artifact.ArtifactID, SHA256: artifact.SHA256}
}

func campaignOptionalBoundArtifact(artifact *CampaignArtifactBinding) *CampaignBoundArtifact {
	if artifact == nil {
		return nil
	}
	bound := campaignBoundArtifact(*artifact)
	return &bound
}

func validateCampaignBoundArtifact(name string, artifact CampaignBoundArtifact) error {
	if err := validateIdentifier(name+".artifact_id", artifact.ArtifactID); err != nil {
		return err
	}
	return validateSHA256(name+".sha256", artifact.SHA256)
}

func (p CampaignPlan) validateTaskArtifactsAndGates() (map[Task]struct{}, error) {
	if len(p.TaskArtifacts) == 0 {
		return nil, fmt.Errorf("task_artifacts must not be empty")
	}
	tasks := make(map[Task]struct{}, len(p.TaskArtifacts))
	artifactIDs := make(map[string]struct{})
	for i, entry := range p.TaskArtifacts {
		if !entry.Task.valid() {
			return nil, fmt.Errorf("task_artifacts[%d]: unsupported task %q", i, entry.Task)
		}
		if _, exists := tasks[entry.Task]; exists {
			return nil, fmt.Errorf("task_artifacts[%d]: duplicate task %q", i, entry.Task)
		}
		tasks[entry.Task] = struct{}{}
		if err := validateCampaignArtifact(fmt.Sprintf("task_artifacts[%d].corpus", i), entry.Corpus, false); err != nil {
			return nil, err
		}
		if err := addCampaignArtifactID(artifactIDs, entry.Corpus.ArtifactID); err != nil {
			return nil, fmt.Errorf("task_artifacts[%d].corpus: %w", i, err)
		}
		if p.Stage != CampaignStageProtocolScreen {
			if entry.GoldClosure == nil || entry.PrivacySidecar == nil {
				return nil, fmt.Errorf("task_artifacts[%d]: gold_closure and privacy_sidecar are required outside protocol_screen", i)
			}
		}
		for name, artifact := range map[string]*CampaignArtifactBinding{
			"gold_closure":    entry.GoldClosure,
			"privacy_sidecar": entry.PrivacySidecar,
		} {
			if artifact == nil {
				continue
			}
			if err := validateCampaignArtifact(fmt.Sprintf("task_artifacts[%d].%s", i, name), *artifact, false); err != nil {
				return nil, err
			}
			if err := addCampaignArtifactID(artifactIDs, artifact.ArtifactID); err != nil {
				return nil, fmt.Errorf("task_artifacts[%d].%s: %w", i, name, err)
			}
		}
	}
	if len(p.EffectivenessGates) != len(tasks) {
		return nil, fmt.Errorf("effectiveness_gates must contain exactly one entry per task")
	}
	gates := make(map[Task]struct{}, len(p.EffectivenessGates))
	for i, gate := range p.EffectivenessGates {
		if _, exists := tasks[gate.Task]; !exists {
			return nil, fmt.Errorf("effectiveness_gates[%d]: task %q has no task artifacts", i, gate.Task)
		}
		if _, exists := gates[gate.Task]; exists {
			return nil, fmt.Errorf("effectiveness_gates[%d]: duplicate task %q", i, gate.Task)
		}
		gates[gate.Task] = struct{}{}
		if err := gate.Validate(); err != nil {
			return nil, fmt.Errorf("effectiveness_gates[%d]: %w", i, err)
		}
	}
	return tasks, nil
}

type campaignMatrixCell struct {
	systemID string
	task     Task
}

func (p CampaignPlan) validateSystemMatrix(taskSet map[Task]struct{}) (map[string]CampaignSystemRole, map[campaignMatrixCell]struct{}, error) {
	roles := make(map[string]CampaignSystemRole, len(p.Systems))
	matrix := make(map[campaignMatrixCell]struct{})
	roleByTask := make(map[Task]map[CampaignSystemRole]int)
	controlKindsByTask := make(map[Task]map[CampaignControlKind]int)
	for i, system := range p.Systems {
		if err := validateIdentifier(fmt.Sprintf("systems[%d].system_id", i), system.SystemID); err != nil {
			return nil, nil, err
		}
		if _, exists := roles[system.SystemID]; exists {
			return nil, nil, fmt.Errorf("systems[%d]: duplicate system_id %q", i, system.SystemID)
		}
		if system.Role != CampaignSystemRoleControl && system.Role != CampaignSystemRoleCandidate {
			return nil, nil, fmt.Errorf("systems[%d]: unsupported role %q", i, system.Role)
		}
		if system.Role == CampaignSystemRoleControl {
			if system.ControlKind != CampaignControlKindDeployed &&
				system.ControlKind != CampaignControlKindNormalized {
				return nil, nil, fmt.Errorf("systems[%d]: control requires deployed or normalized control_kind", i)
			}
		} else if system.ControlKind != "" {
			return nil, nil, fmt.Errorf("systems[%d]: candidate must not declare control_kind", i)
		}
		if len(system.Tasks) == 0 {
			return nil, nil, fmt.Errorf("systems[%d].tasks must not be empty", i)
		}
		roles[system.SystemID] = system.Role
		seenTasks := make(map[Task]struct{}, len(system.Tasks))
		for j, task := range system.Tasks {
			if _, exists := taskSet[task]; !exists {
				return nil, nil, fmt.Errorf("systems[%d].tasks[%d]: task %q has no campaign artifacts", i, j, task)
			}
			if _, exists := seenTasks[task]; exists {
				return nil, nil, fmt.Errorf("systems[%d].tasks[%d]: duplicate task %q", i, j, task)
			}
			seenTasks[task] = struct{}{}
			matrix[campaignMatrixCell{systemID: system.SystemID, task: task}] = struct{}{}
			if roleByTask[task] == nil {
				roleByTask[task] = make(map[CampaignSystemRole]int)
			}
			roleByTask[task][system.Role]++
			if system.Role == CampaignSystemRoleControl {
				if controlKindsByTask[task] == nil {
					controlKindsByTask[task] = make(map[CampaignControlKind]int)
				}
				controlKindsByTask[task][system.ControlKind]++
			}
		}
	}
	for task := range taskSet {
		if roleByTask[task][CampaignSystemRoleControl] == 0 {
			return nil, nil, fmt.Errorf("task %q has no control system", task)
		}
		if roleByTask[task][CampaignSystemRoleCandidate] == 0 {
			return nil, nil, fmt.Errorf("task %q has no candidate system", task)
		}
		if controlKindsByTask[task][CampaignControlKindDeployed] == 0 ||
			controlKindsByTask[task][CampaignControlKindNormalized] == 0 {
			return nil, nil, fmt.Errorf("task %q requires both deployed and normalized controls", task)
		}
	}
	return roles, matrix, nil
}

func (p CampaignPlan) validateRouteSnapshots(systemRoles map[string]CampaignSystemRole) error {
	seen := make(map[string]struct{}, len(p.RouteSnapshots))
	artifactIDs := make(map[string]struct{}, len(p.RouteSnapshots))
	for i, route := range p.RouteSnapshots {
		if _, exists := systemRoles[route.SystemID]; !exists {
			return fmt.Errorf("route_snapshots[%d]: system_id %q is not in the campaign", i, route.SystemID)
		}
		if _, exists := seen[route.SystemID]; exists {
			return fmt.Errorf("route_snapshots[%d]: duplicate system_id %q", i, route.SystemID)
		}
		seen[route.SystemID] = struct{}{}
		if err := validateCampaignArtifact(fmt.Sprintf("route_snapshots[%d].snapshot", i), route.Snapshot, false); err != nil {
			return err
		}
		if err := addCampaignArtifactID(artifactIDs, route.Snapshot.ArtifactID); err != nil {
			return fmt.Errorf("route_snapshots[%d].snapshot: %w", i, err)
		}
	}
	return nil
}

func (p CampaignPlan) validateRuns(matrix map[campaignMatrixCell]struct{}) error {
	seenIDs := make(map[string]struct{}, len(p.Runs))
	seenRepeats := make(map[campaignMatrixCell]map[int]struct{})
	coverage := make(map[campaignMatrixCell]int)
	var total int64
	for i, run := range p.Runs {
		if err := validateIdentifier(fmt.Sprintf("runs[%d].run_id", i), run.RunID); err != nil {
			return err
		}
		if _, exists := seenIDs[run.RunID]; exists {
			return fmt.Errorf("runs[%d]: duplicate run_id %q", i, run.RunID)
		}
		seenIDs[run.RunID] = struct{}{}
		cell := campaignMatrixCell{systemID: run.SystemID, task: run.Task}
		if _, exists := matrix[cell]; !exists {
			return fmt.Errorf("runs[%d]: system/task %q/%q is absent from the declared matrix", i, run.SystemID, run.Task)
		}
		if run.CostCapMicroUSD <= 0 || run.CostCapMicroUSD > p.TotalCostCapMicroUSD {
			return fmt.Errorf("runs[%d].cost_cap_micro_usd must be positive and no greater than the campaign cap", i)
		}
		if total > p.TotalCostCapMicroUSD-run.CostCapMicroUSD {
			return fmt.Errorf("sum of per-run cost caps exceeds total_cost_cap_micro_usd")
		}
		total += run.CostCapMicroUSD
		coverage[cell]++
		if p.Stage == CampaignStageRepeat {
			if run.RepeatIndex <= 0 {
				return fmt.Errorf("runs[%d].repeat_index must be positive in repeat stage", i)
			}
			if seenRepeats[cell] == nil {
				seenRepeats[cell] = make(map[int]struct{})
			}
			if _, exists := seenRepeats[cell][run.RepeatIndex]; exists {
				return fmt.Errorf("runs[%d]: duplicate repeat_index %d for system/task", i, run.RepeatIndex)
			}
			seenRepeats[cell][run.RepeatIndex] = struct{}{}
		} else if run.RepeatIndex != 0 {
			return fmt.Errorf("runs[%d].repeat_index is only valid in repeat stage", i)
		}
	}
	for cell := range matrix {
		count := coverage[cell]
		if p.Stage == CampaignStageRepeat {
			if count < 2 {
				return fmt.Errorf("repeat stage requires at least two runs for %q/%q", cell.systemID, cell.task)
			}
			for index := 1; index <= count; index++ {
				if _, exists := seenRepeats[cell][index]; !exists {
					return fmt.Errorf("repeat indices for %q/%q must be contiguous from 1", cell.systemID, cell.task)
				}
			}
		} else if count != 1 {
			return fmt.Errorf("campaign matrix cell %q/%q requires exactly one run", cell.systemID, cell.task)
		}
	}
	return nil
}

type campaignComparisonPair struct {
	controlRunID   string
	candidateRunID string
}

type campaignComparisonCell struct {
	task        Task
	repeatIndex int
}

func (p CampaignPlan) validateComparisons(
	systemRoles map[string]CampaignSystemRole,
) error {
	if len(p.Comparisons) == 0 || len(p.Comparisons) > maxCampaignComparisons {
		return fmt.Errorf(
			"comparisons must contain the complete 1..%d entry matrix",
			maxCampaignComparisons,
		)
	}
	runByID := make(map[string]CampaignRun, len(p.Runs))
	candidates := make([]CampaignRun, 0, len(p.Runs))
	controlKindBySystem := make(map[string]CampaignControlKind)
	controlsByCell := make(
		map[campaignComparisonCell][]CampaignRun,
	)
	controlKindsByCell := make(
		map[campaignComparisonCell]map[CampaignControlKind]struct{},
	)
	for _, system := range p.Systems {
		if system.Role == CampaignSystemRoleControl {
			controlKindBySystem[system.SystemID] = system.ControlKind
		}
	}
	for _, run := range p.Runs {
		runByID[run.RunID] = run
		switch systemRoles[run.SystemID] {
		case CampaignSystemRoleControl:
			cell := campaignComparisonCell{
				task:        run.Task,
				repeatIndex: run.RepeatIndex,
			}
			controlsByCell[cell] = append(controlsByCell[cell], run)
			if controlKindsByCell[cell] == nil {
				controlKindsByCell[cell] =
					make(map[CampaignControlKind]struct{}, 2)
			}
			controlKindsByCell[cell][controlKindBySystem[run.SystemID]] =
				struct{}{}
		case CampaignSystemRoleCandidate:
			candidates = append(candidates, run)
		}
	}
	expectedPairCount := 0
	for _, candidate := range candidates {
		cell := campaignComparisonCell{
			task:        candidate.Task,
			repeatIndex: candidate.RepeatIndex,
		}
		controlKinds := controlKindsByCell[cell]
		if _, deployed := controlKinds[CampaignControlKindDeployed]; !deployed {
			return fmt.Errorf(
				"candidate run %q has no same-task/same-repeat deployed control",
				candidate.RunID,
			)
		}
		if _, normalized := controlKinds[CampaignControlKindNormalized]; !normalized {
			return fmt.Errorf(
				"candidate run %q has no same-task/same-repeat normalized control",
				candidate.RunID,
			)
		}
		controlCount := len(controlsByCell[cell])
		if controlCount > maxCampaignComparisons-expectedPairCount {
			return fmt.Errorf(
				"comparisons matrix requires more than the %d-entry limit",
				maxCampaignComparisons,
			)
		}
		expectedPairCount += controlCount
	}
	if expectedPairCount == 0 {
		return fmt.Errorf("comparisons matrix has no eligible control/candidate pairs")
	}
	if len(p.Comparisons) != expectedPairCount {
		return fmt.Errorf(
			"comparisons matrix is incomplete: got %d entries, require %d",
			len(p.Comparisons),
			expectedPairCount,
		)
	}
	expected := make(
		map[campaignComparisonPair]struct{},
		expectedPairCount,
	)
	for _, candidate := range candidates {
		cell := campaignComparisonCell{
			task:        candidate.Task,
			repeatIndex: candidate.RepeatIndex,
		}
		for _, control := range controlsByCell[cell] {
			expected[campaignComparisonPair{
				controlRunID:   control.RunID,
				candidateRunID: candidate.RunID,
			}] = struct{}{}
		}
	}
	seenIDs := make(map[string]struct{}, len(p.Comparisons))
	seenPairs := make(map[campaignComparisonPair]struct{}, len(p.Comparisons))
	for index, comparison := range p.Comparisons {
		prefix := fmt.Sprintf("comparisons[%d]", index)
		if err := validateIdentifier(
			prefix+".comparison_id",
			comparison.ComparisonID,
		); err != nil {
			return err
		}
		if _, duplicate := seenIDs[comparison.ComparisonID]; duplicate {
			return fmt.Errorf(
				"%s: duplicate comparison_id %q",
				prefix,
				comparison.ComparisonID,
			)
		}
		seenIDs[comparison.ComparisonID] = struct{}{}
		if err := validateRelativeOutputPath(
			prefix+".output_path",
			comparison.OutputPath,
		); err != nil {
			return err
		}
		control, controlExists := runByID[comparison.ControlRunID]
		candidate, candidateExists := runByID[comparison.CandidateRunID]
		if !controlExists || !candidateExists {
			return fmt.Errorf(
				"%s references a run absent from the campaign",
				prefix,
			)
		}
		if systemRoles[control.SystemID] != CampaignSystemRoleControl ||
			systemRoles[candidate.SystemID] != CampaignSystemRoleCandidate {
			return fmt.Errorf(
				"%s must identify one control run and one candidate run",
				prefix,
			)
		}
		pair := campaignComparisonPair{
			controlRunID:   comparison.ControlRunID,
			candidateRunID: comparison.CandidateRunID,
		}
		if _, required := expected[pair]; !required {
			return fmt.Errorf(
				"%s control/candidate pair has a different task or repeat index",
				prefix,
			)
		}
		if _, duplicate := seenPairs[pair]; duplicate {
			return fmt.Errorf(
				"%s duplicates control/candidate pair %q/%q",
				prefix,
				comparison.ControlRunID,
				comparison.CandidateRunID,
			)
		}
		seenPairs[pair] = struct{}{}
	}
	for pair := range expected {
		if _, exists := seenPairs[pair]; !exists {
			return fmt.Errorf(
				"comparisons matrix is missing control/candidate pair %q/%q",
				pair.controlRunID,
				pair.candidateRunID,
			)
		}
	}
	return nil
}

// ComparisonForRuns returns the exact preregistered report declaration for
// one control/candidate pair.
func (p CampaignPlan) ComparisonForRuns(
	controlRunID string,
	candidateRunID string,
) (CampaignComparison, error) {
	if err := p.Validate(); err != nil {
		return CampaignComparison{}, err
	}
	for _, comparison := range p.Comparisons {
		if comparison.ControlRunID == controlRunID &&
			comparison.CandidateRunID == candidateRunID {
			return comparison, nil
		}
	}
	return CampaignComparison{}, fmt.Errorf(
		"control/candidate pair %q/%q is absent from the campaign comparison matrix",
		controlRunID,
		candidateRunID,
	)
}

func (p CampaignPlan) validateHistoricalBaselines(taskSet map[Task]struct{}) error {
	if len(p.HistoricalBaselines) == 0 {
		return fmt.Errorf("historical_baselines must not be empty")
	}
	seen := make(map[string]struct{}, len(p.HistoricalBaselines))
	artifactIDs := make(map[string]struct{}, len(p.HistoricalBaselines))
	byTask := make(map[Task]int, len(taskSet))
	for i, baseline := range p.HistoricalBaselines {
		if err := validateIdentifier(fmt.Sprintf("historical_baselines[%d].baseline_id", i), baseline.BaselineID); err != nil {
			return err
		}
		if _, exists := seen[baseline.BaselineID]; exists {
			return fmt.Errorf("historical_baselines[%d]: duplicate baseline_id %q", i, baseline.BaselineID)
		}
		seen[baseline.BaselineID] = struct{}{}
		if _, exists := taskSet[baseline.Task]; !exists {
			return fmt.Errorf("historical_baselines[%d]: task %q is not in the campaign", i, baseline.Task)
		}
		byTask[baseline.Task]++
		if baseline.PromotionEligible {
			return fmt.Errorf("historical_baselines[%d]: historical artifacts must be nonpromotable", i)
		}
		if err := validateIdentifier(fmt.Sprintf("historical_baselines[%d].system_id", i), baseline.SystemID); err != nil {
			return err
		}
		if _, err := time.Parse("2006-01-02", baseline.BenchmarkEpoch); err != nil {
			return fmt.Errorf("historical_baselines[%d].benchmark_epoch must be YYYY-MM-DD", i)
		}
		if err := validateIdentifier(fmt.Sprintf("historical_baselines[%d].protocol_version", i), baseline.ProtocolVersion); err != nil {
			return err
		}
		if err := validateCampaignArtifact(fmt.Sprintf("historical_baselines[%d].artifact", i), baseline.Artifact, false); err != nil {
			return err
		}
		if err := addCampaignArtifactID(artifactIDs, baseline.Artifact.ArtifactID); err != nil {
			return fmt.Errorf("historical_baselines[%d].artifact: %w", i, err)
		}
	}
	for task := range taskSet {
		if byTask[task] == 0 {
			return fmt.Errorf("historical_baselines: task %q has no baseline", task)
		}
	}
	return nil
}

func (p CampaignPlan) validateOutputPaths() error {
	seen := make(
		map[string]string,
		len(p.Runs)*3+len(p.Comparisons)+1,
	)
	add := func(name, relative string) error {
		if err := validateRelativeOutputPath(name, relative); err != nil {
			return err
		}
		clean := filepath.Clean(relative)
		if previous, exists := seen[clean]; exists {
			return fmt.Errorf("%s collides with %s", name, previous)
		}
		seen[clean] = name
		return nil
	}
	if err := add("summary_path", p.SummaryPath); err != nil {
		return err
	}
	for i, run := range p.Runs {
		for name, path := range map[string]string{
			"results_path":          run.Outputs.ResultsPath,
			"attempt_evidence_path": run.Outputs.AttemptEvidencePath,
			"score_path":            run.Outputs.ScorePath,
		} {
			if err := add(fmt.Sprintf("runs[%d].outputs.%s", i, name), path); err != nil {
				return err
			}
		}
	}
	for i, comparison := range p.Comparisons {
		if err := add(
			fmt.Sprintf("comparisons[%d].output_path", i),
			comparison.OutputPath,
		); err != nil {
			return err
		}
	}
	for i, run := range p.Runs {
		checkpoint, err := campaignCheckpointRelativePath(
			run.Outputs.ResultsPath,
		)
		if err != nil {
			return fmt.Errorf("runs[%d] checkpoint path: %w", i, err)
		}
		if previous, exists := seen[checkpoint]; exists {
			return fmt.Errorf(
				"runs[%d] derived checkpoint path collides with %s",
				i,
				previous,
			)
		}
		checkpointAbsolute := filepath.Clean(filepath.Join(
			p.OutputRoot,
			checkpoint,
		))
		for _, reference := range p.ArtifactReferences() {
			if reference.Path == "" {
				continue
			}
			if checkpointAbsolute == filepath.Clean(reference.Path) {
				return fmt.Errorf(
					"runs[%d] derived checkpoint path collides with %s",
					i,
					reference.Name,
				)
			}
		}
	}
	return nil
}

func campaignCheckpointRelativePath(resultsPath string) (string, error) {
	if err := validateRelativeOutputPath("results_path", resultsPath); err != nil {
		return "", err
	}
	path := filepath.Join(
		filepath.Dir(resultsPath),
		"."+filepath.Base(resultsPath)+".campaign-checkpoint.json",
	)
	if err := validateRelativeOutputPath("checkpoint_path", path); err != nil {
		return "", err
	}
	return filepath.Clean(path), nil
}

// CampaignCheckpointRelativePath exposes the one reserved transaction path
// derivation used by the campaign CLI. Plan validation reserves this path, so
// executors must not invent a different checkpoint naming convention.
func CampaignCheckpointRelativePath(resultsPath string) (string, error) {
	return campaignCheckpointRelativePath(resultsPath)
}

func (p CampaignPlan) validateGlobalArtifactIDs() error {
	seen := make(map[string]string)
	add := func(name string, artifact *CampaignArtifactBinding) error {
		if artifact == nil {
			return nil
		}
		if previous, exists := seen[artifact.ArtifactID]; exists {
			return fmt.Errorf("%s: duplicate artifact_id %q also used by %s", name, artifact.ArtifactID, previous)
		}
		seen[artifact.ArtifactID] = name
		return nil
	}
	for i := range p.TaskArtifacts {
		entry := &p.TaskArtifacts[i]
		if err := add(fmt.Sprintf("task_artifacts[%d].corpus", i), &entry.Corpus); err != nil {
			return err
		}
		if err := add(fmt.Sprintf("task_artifacts[%d].gold_closure", i), entry.GoldClosure); err != nil {
			return err
		}
		if err := add(fmt.Sprintf("task_artifacts[%d].privacy_sidecar", i), entry.PrivacySidecar); err != nil {
			return err
		}
	}
	for i := range p.RouteSnapshots {
		if err := add(fmt.Sprintf("route_snapshots[%d].snapshot", i), &p.RouteSnapshots[i].Snapshot); err != nil {
			return err
		}
	}
	for i := range p.HistoricalBaselines {
		if err := add(fmt.Sprintf("historical_baselines[%d].artifact", i), &p.HistoricalBaselines[i].Artifact); err != nil {
			return err
		}
	}
	return nil
}

func (g CampaignEffectivenessGate) Validate() error {
	wantHarm, wantUtility := campaignTaskMetrics(g.Task)
	if wantHarm == "" {
		return fmt.Errorf("unsupported task %q", g.Task)
	}
	if g.HarmUtility.HarmMetric != wantHarm || g.HarmUtility.UtilityMetric != wantUtility {
		return fmt.Errorf("task %q requires harm metric %q and utility metric %q", g.Task, wantHarm, wantUtility)
	}
	if g.Inference.ConfidenceLevel != CampaignConfidenceLevel ||
		g.Inference.ProportionBoundMethod != CampaignProportionBoundMethod ||
		g.Inference.ControlDeltaBoundMethod != CampaignControlBoundMethod ||
		g.Inference.BoundSemantics != CampaignBoundSemantics ||
		g.Inference.BootstrapReplicates < 1_000 ||
		g.Inference.BootstrapReplicates > maxBootstrapReplicates ||
		g.Inference.BootstrapSeed == 0 {
		return fmt.Errorf(
			"inference must use confidence %.2f, %q proportions, %q control deltas, %q semantics, 1000..%d bootstrap replicates, and a nonzero preregistered bootstrap seed",
			CampaignConfidenceLevel,
			CampaignProportionBoundMethod,
			CampaignControlBoundMethod,
			CampaignBoundSemantics,
			maxBootstrapReplicates,
		)
	}
	rateGates := map[string]float64{
		"harm_utility.max_harm_rate":                          g.HarmUtility.MaxHarmRate,
		"harm_utility.max_harm_ucb_delta_vs_control":          g.HarmUtility.MaxHarmUCBDeltaVsControl,
		"harm_utility.min_utility_rate":                       g.HarmUtility.MinUtilityRate,
		"schema_reliability.min_first_pass_schema_valid_rate": g.SchemaReliability.MinFirstPassSchemaValidRate,
		"schema_reliability.min_final_schema_valid_rate":      g.SchemaReliability.MinFinalSchemaValidRate,
		"schema_reliability.min_answered_rate":                g.SchemaReliability.MinAnsweredRate,
		"schema_reliability.max_call_error_ucb":               g.SchemaReliability.MaxCallErrorUCB,
		"latency.max_p95_relative_slowdown_vs_control":        g.Latency.MaxP95RelativeSlowdownVsControl,
		"latency.max_timeout_ucb":                             g.Latency.MaxTimeoutUCB,
		"stability.min_repeat_agreement":                      g.Stability.MinRepeatAgreement,
		"stability.max_utility_drift":                         g.Stability.MaxUtilityDrift,
		"stability.max_harm_drift":                            g.Stability.MaxHarmDrift,
		"stability.max_final_action_flip_ucb":                 g.Stability.MaxFinalActionFlipUCB,
	}
	confidenceGatesApplicable := g.Task != TaskMatcherExtract
	if !confidenceGatesApplicable {
		if g.Calibration != nil || g.SelectiveRisk != nil {
			return fmt.Errorf(
				"task %q must omit calibration and selective_risk because its result contract has no confidence",
				g.Task,
			)
		}
	} else {
		if g.Calibration == nil {
			return fmt.Errorf("task %q requires a calibration gate", g.Task)
		}
		if g.SelectiveRisk == nil {
			return fmt.Errorf("task %q requires a selective_risk gate", g.Task)
		}
		rateGates["calibration.max_error"] = g.Calibration.MaxError
		rateGates["calibration.max_brier_score"] =
			g.Calibration.MaxBrierScore
		rateGates["selective_risk.max_selective_risk"] =
			g.SelectiveRisk.MaxSelectiveRisk
		rateGates["selective_risk.max_aurc"] = g.SelectiveRisk.MaxAURC
	}
	for name, value := range rateGates {
		if err := validateCampaignRate(name, value); err != nil {
			return err
		}
	}
	if g.Latency.MaxP50MS <= 0 || g.Latency.MaxP95MS < g.Latency.MaxP50MS || g.Latency.MaxP99MS < g.Latency.MaxP95MS {
		return fmt.Errorf("latency percentiles must be positive and ordered p50 <= p95 <= p99")
	}
	wantDeadline := CampaignTaskDeadlineMS(g.Task)
	if g.Latency.DeadlineMS != wantDeadline {
		return fmt.Errorf("latency.deadline_ms for task %q must equal %d", g.Task, wantDeadline)
	}
	if g.Latency.MaxP99MS > g.Latency.DeadlineMS {
		return fmt.Errorf("latency.max_p99_ms must not exceed deadline_ms")
	}
	if g.HarmUtility.MaxSeverityOneErrors != 0 {
		return fmt.Errorf("harm_utility.max_severity_one_errors must equal 0")
	}
	if g.HarmUtility.MaxHarmRate > CampaignMaxAbsoluteHarmRate {
		return fmt.Errorf("harm_utility.max_harm_rate must be at most %.3f", CampaignMaxAbsoluteHarmRate)
	}
	if g.HarmUtility.MinUtilityRate <= 0 {
		return fmt.Errorf("harm_utility.min_utility_rate must be positive")
	}
	if g.HarmUtility.MaxHarmUCBDeltaVsControl < 0 ||
		g.HarmUtility.MaxHarmUCBDeltaVsControl > CampaignMaxHarmUCBDelta {
		return fmt.Errorf("harm_utility.max_harm_ucb_delta_vs_control must be within 0..%.4f", CampaignMaxHarmUCBDelta)
	}
	if math.IsNaN(g.HarmUtility.MinUtilityLCBDeltaVsControl) ||
		math.IsInf(g.HarmUtility.MinUtilityLCBDeltaVsControl, 0) {
		return fmt.Errorf("harm_utility.min_utility_lcb_delta_vs_control must be finite")
	}
	if g.HarmUtility.MinUtilityLCBDeltaVsControl < CampaignMinUtilityLCBDelta ||
		g.HarmUtility.MinUtilityLCBDeltaVsControl > 0 {
		return fmt.Errorf("harm_utility.min_utility_lcb_delta_vs_control must be within %.2f..0", CampaignMinUtilityLCBDelta)
	}
	if g.SchemaReliability.MinFirstPassSchemaValidRate < CampaignMinFirstPassValidRate {
		return fmt.Errorf("schema_reliability.min_first_pass_schema_valid_rate must be at least %.3f", CampaignMinFirstPassValidRate)
	}
	if g.SchemaReliability.MinFinalSchemaValidRate != 1 {
		return fmt.Errorf("schema_reliability.min_final_schema_valid_rate must equal 1")
	}
	if g.SchemaReliability.MaxSchemaRepairsPerCase < 0 ||
		g.SchemaReliability.MaxSchemaRepairsPerCase > 1 {
		return fmt.Errorf("schema_reliability.max_schema_repairs_per_case must be 0 or 1")
	}
	if g.SchemaReliability.MaxCallErrorUCB > CampaignMaxTimeoutErrorUCB {
		return fmt.Errorf("schema_reliability.max_call_error_ucb must be at most %.3f", CampaignMaxTimeoutErrorUCB)
	}
	if g.Latency.MaxP95RelativeSlowdownVsControl > CampaignMaxLatencySlowdown {
		return fmt.Errorf("latency.max_p95_relative_slowdown_vs_control must be at most %.2f", CampaignMaxLatencySlowdown)
	}
	if g.Latency.MaxTimeoutUCB > CampaignMaxTimeoutErrorUCB {
		return fmt.Errorf("latency.max_timeout_ucb must be at most %.3f", CampaignMaxTimeoutErrorUCB)
	}
	if g.Stability.MaxHarmfulRepeatFlips != 0 {
		return fmt.Errorf("stability.max_harmful_repeat_flips must equal 0")
	}
	if g.Stability.MaxFinalActionFlipUCB > CampaignMaxFinalActionFlipUCB {
		return fmt.Errorf("stability.max_final_action_flip_ucb must be at most %.2f", CampaignMaxFinalActionFlipUCB)
	}
	if confidenceGatesApplicable {
		if g.Calibration.Metric != CampaignCalibrationMetricECE {
			return fmt.Errorf("calibration.metric must be %q", CampaignCalibrationMetricECE)
		}
		if g.Calibration.MaxBrierScore > CampaignMaxBrierScore {
			return fmt.Errorf("calibration.max_brier_score must be at most %.2f", CampaignMaxBrierScore)
		}
		if math.IsNaN(g.SelectiveRisk.MinCoverageRatioVsIncumbent) ||
			math.IsInf(g.SelectiveRisk.MinCoverageRatioVsIncumbent, 0) ||
			g.SelectiveRisk.MinCoverageRatioVsIncumbent < 1 {
			return fmt.Errorf("selective_risk.min_coverage_ratio_vs_incumbent must be finite and at least 1")
		}
	}
	if len(g.CriticalStrata) == 0 {
		return fmt.Errorf("critical_strata must not be empty")
	}
	seenStrata := make(map[string]struct{}, len(g.CriticalStrata))
	for i, stratum := range g.CriticalStrata {
		if err := validateIdentifier(fmt.Sprintf("critical_strata[%d].slice_id", i), stratum.SliceID); err != nil {
			return err
		}
		if _, duplicate := seenStrata[stratum.SliceID]; duplicate {
			return fmt.Errorf("critical_strata[%d]: duplicate slice_id %q", i, stratum.SliceID)
		}
		seenStrata[stratum.SliceID] = struct{}{}
		if stratum.MinimumCases <= 0 {
			return fmt.Errorf("critical_strata[%d].minimum_cases must be positive", i)
		}
		if stratum.MaxSeverityOneErrors != 0 {
			return fmt.Errorf("critical_strata[%d].max_severity_one_errors must equal 0", i)
		}
		if err := validateCampaignRate(fmt.Sprintf("critical_strata[%d].max_harm_rate", i), stratum.MaxHarmRate); err != nil {
			return err
		}
		if stratum.MaxHarmRate > CampaignMaxAbsoluteHarmRate {
			return fmt.Errorf("critical_strata[%d].max_harm_rate must be at most %.3f", i, CampaignMaxAbsoluteHarmRate)
		}
		if err := validateCampaignRate(fmt.Sprintf("critical_strata[%d].min_utility_rate", i), stratum.MinUtilityRate); err != nil {
			return err
		}
		if stratum.MinUtilityRate <= 0 {
			return fmt.Errorf("critical_strata[%d].min_utility_rate must be positive", i)
		}
	}
	if g.Cost.MaxCostPer1000CasesMicroUSD <= 0 ||
		g.Cost.MaxProjectedMonthlyMicroUSD <= 0 ||
		g.Cost.MaxCostPerCorrectSafeActionMicroUSD <= 0 {
		return fmt.Errorf("cost limits must be positive")
	}
	if math.IsNaN(g.Cost.MaxCostRatioVsControl) || math.IsInf(g.Cost.MaxCostRatioVsControl, 0) || g.Cost.MaxCostRatioVsControl <= 0 {
		return fmt.Errorf("cost.max_cost_ratio_vs_control must be finite and positive")
	}
	return nil
}

func (s CampaignStage) valid() bool {
	switch s {
	case CampaignStageProtocolScreen, CampaignStageDevelopment, CampaignStageHoldout, CampaignStageRepeat, CampaignStageProspectiveShadow:
		return true
	default:
		return false
	}
}

func (s CampaignStage) formal() bool {
	return s == CampaignStageDevelopment || s == CampaignStageHoldout ||
		s == CampaignStageRepeat || s == CampaignStageProspectiveShadow
}

func campaignTaskMetrics(task Task) (string, string) {
	switch task {
	case TaskMatcherExtract:
		return CampaignMatcherExtractHarm, CampaignMatcherExtractUtility
	case TaskMatcherRerank:
		return CampaignMatcherRerankHarm, CampaignMatcherRerankUtility
	case TaskContentFilter:
		return CampaignContentFilterHarm, CampaignContentFilterUtility
	case TaskJunkPurge:
		return CampaignJunkPurgeHarm, CampaignJunkPurgeUtility
	default:
		return "", ""
	}
}

func CampaignTaskDeadlineMS(task Task) int64 {
	switch task {
	case TaskContentFilter:
		return 8_000
	case TaskMatcherExtract, TaskMatcherRerank, TaskJunkPurge:
		return 60_000
	default:
		return 0
	}
}

func validateCampaignActions(actions []CampaignAction) error {
	want := map[CampaignAction]struct{}{
		CampaignActionValidate:  {},
		CampaignActionRunShadow: {},
		CampaignActionScore:     {},
		CampaignActionCompare:   {},
	}
	if len(actions) != len(want) {
		return fmt.Errorf("allowed_actions must contain exactly validate_plan, run_shadow, score, and compare")
	}
	seen := make(map[CampaignAction]struct{}, len(actions))
	for i, action := range actions {
		if _, allowed := want[action]; !allowed {
			return fmt.Errorf("allowed_actions[%d] %q is unsafe or unsupported; promotion and deploy actions are forbidden", i, action)
		}
		if _, duplicate := seen[action]; duplicate {
			return fmt.Errorf("allowed_actions[%d]: duplicate %q", i, action)
		}
		seen[action] = struct{}{}
	}
	return nil
}

func validateCampaignArtifact(name string, artifact CampaignArtifactBinding, requirePath bool) error {
	if err := validateIdentifier(name+".artifact_id", artifact.ArtifactID); err != nil {
		return err
	}
	if err := validateSHA256(name+".sha256", artifact.SHA256); err != nil {
		return err
	}
	if requirePath && artifact.Path == "" {
		return fmt.Errorf("%s.path is required", name)
	}
	if artifact.Path != "" {
		if err := validateAbsoluteCampaignPath(name+".path", artifact.Path); err != nil {
			return err
		}
	}
	return nil
}

func addCampaignArtifactID(seen map[string]struct{}, id string) error {
	if _, exists := seen[id]; exists {
		return fmt.Errorf("duplicate artifact_id %q", id)
	}
	seen[id] = struct{}{}
	return nil
}

func validateCampaignRate(name string, value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
		return fmt.Errorf("%s must be finite and within 0..1", name)
	}
	return nil
}

func validateAbsoluteCampaignPath(name, value string) error {
	if strings.TrimSpace(value) == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return fmt.Errorf("%s must be a clean absolute path", name)
	}
	if strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%s contains NUL", name)
	}
	return rejectSecret(name, value)
}

func validateRelativeOutputPath(name, value string) error {
	if strings.TrimSpace(value) == "" || filepath.IsAbs(value) || filepath.Clean(value) != value || value == "." {
		return fmt.Errorf("%s must be a clean relative file path", name)
	}
	if value == ".." || strings.HasPrefix(value, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s must remain below output_root", name)
	}
	if strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%s contains NUL", name)
	}
	return rejectSecret(name, value)
}

func campaignPathWithin(root, candidate string) (bool, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return false, err
	}
	relative, err := filepath.Rel(filepath.Clean(rootAbs), filepath.Clean(candidateAbs))
	if err != nil {
		return false, err
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))), nil
}

func taskOrder(task Task) int {
	for i, candidate := range orderedTasks {
		if task == candidate {
			return i
		}
	}
	return len(orderedTasks)
}

func campaignRoleOrder(role CampaignSystemRole) int {
	if role == CampaignSystemRoleControl {
		return 0
	}
	return 1
}

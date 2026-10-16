package llmeval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"
)

const (
	PromotionBundleProtocolVersion = "bitagent-llm-promotion-v2"

	PromotionFirstPassSchemaFloor     = 0.995
	PromotionCalibrationECECeiling    = 0.10
	PromotionCalibrationBrierCeiling  = 0.25
	PromotionCalibrationMinimumGroups = 30
	PromotionInstabilityCeiling       = 0.01
	PromotionRequiredRepeatRuns       = 2
	PromotionRepeatNaturalGroups      = 600
	PromotionRepeatSafetyGroups       = 600
	PromotionRepeatConfidenceLevel    = 0.95
	PromotionFamilyAlpha              = 0.05

	PromotionRepeatSamplingAlgorithmID = "sha256-task-suite-group-v1"
	PromotionRepeatConfidenceMethod    = "wilson_score_one_sided"

	maxPromotionAttemptEvidenceBytes = 64 << 20
	maxPromotionRosterBytes          = 1 << 20
)

// PromotionSystemRole distinguishes comparator controls from systems that may
// be selected. A control can be reused by several candidates, but every
// candidate names its exact comparator.
type PromotionSystemRole string

const (
	PromotionRoleControl   PromotionSystemRole = "control"
	PromotionRoleCandidate PromotionSystemRole = "candidate"
)

// PromotionServiceTier identifies the production price normalization used for
// cheapest-passing selection. OpenRouter routes are standard-only.
type PromotionServiceTier string

const (
	PromotionServiceStandard    PromotionServiceTier = "standard"
	PromotionServiceOpenAIBatch PromotionServiceTier = "openai_batch"
	PromotionServiceOpenAIFlex  PromotionServiceTier = "openai_flex"
)

// PromotionDecisionThresholds is the frozen, human-readable representation of
// the three production action thresholds. It is carried in the roster so raw
// provider completions cannot be reparsed at result-aware thresholds after the
// holdout is opened.
type PromotionDecisionThresholds struct {
	MatcherAttachConfidence float64 `json:"matcher_attach_confidence"`
	ContentDropConfidence   float64 `json:"content_drop_confidence"`
	JunkConfidence          float64 `json:"junk_confidence"`
}

// PromotionRepeatSamplingPlan is frozen in the pre-holdout roster. It replaces
// prohibitively expensive full-holdout repetitions with a deterministic,
// prevalence-independent group sample and a one-sided confidence bound.
type PromotionRepeatSamplingPlan struct {
	AlgorithmID        string  `json:"algorithm_id"`
	NaturalGroups      int     `json:"natural_groups"`
	SafetyGroups       int     `json:"safety_groups"`
	ConfidenceLevel    float64 `json:"confidence_level"`
	ConfidenceMethod   string  `json:"confidence_method"`
	InstabilityCeiling float64 `json:"instability_ceiling"`
}

func DefaultPromotionRepeatSamplingPlan() PromotionRepeatSamplingPlan {
	return PromotionRepeatSamplingPlan{
		AlgorithmID:        PromotionRepeatSamplingAlgorithmID,
		NaturalGroups:      PromotionRepeatNaturalGroups,
		SafetyGroups:       PromotionRepeatSafetyGroups,
		ConfidenceLevel:    PromotionRepeatConfidenceLevel,
		ConfidenceMethod:   PromotionRepeatConfidenceMethod,
		InstabilityCeiling: PromotionInstabilityCeiling,
	}
}

func (plan PromotionRepeatSamplingPlan) validate() error {
	expected := DefaultPromotionRepeatSamplingPlan()
	if plan != expected {
		return fmt.Errorf(
			"repeat_sampling must equal the promotion v2 preregistration: %+v",
			expected,
		)
	}
	return nil
}

func promotionDecisionThresholds(
	thresholds DecisionThresholds,
) PromotionDecisionThresholds {
	return PromotionDecisionThresholds{
		MatcherAttachConfidence: thresholds.MatcherAttachConfidence,
		ContentDropConfidence:   thresholds.ContentDropConfidence,
		JunkConfidence:          thresholds.JunkConfidence,
	}
}

func (thresholds PromotionDecisionThresholds) runtime() DecisionThresholds {
	return DecisionThresholds{
		MatcherAttachConfidence: thresholds.MatcherAttachConfidence,
		ContentDropConfidence:   thresholds.ContentDropConfidence,
		JunkConfidence:          thresholds.JunkConfidence,
	}
}

func (thresholds PromotionDecisionThresholds) validate() error {
	return thresholds.runtime().Validate()
}

// PromotionAttemptKind records the only two request attempts permitted by the
// v2 promotion protocol. An initial schema failure may receive one accounted
// schema repair. Runtime retry chains are intentionally not promotion-grade.
type PromotionAttemptKind string

const (
	PromotionAttemptInitial      PromotionAttemptKind = "initial"
	PromotionAttemptSchemaRepair PromotionAttemptKind = "schema_repair"
)

// PromotionAttempt is source-text-free charged-attempt evidence for one case.
// Usage is provider-reported or route-priced in the same way as ResultRecord.
type PromotionAttempt struct {
	CaseID   string               `json:"case_id"`
	Sequence int                  `json:"sequence"`
	Kind     PromotionAttemptKind `json:"kind"`
	Status   ResultStatus         `json:"status"`
	Usage    Usage                `json:"usage"`
}

// PromotionAttemptEvidence binds every charged attempt to one exact canonical
// result set. Result Usage must equal the sum of these attempts case by case.
type PromotionAttemptEvidence struct {
	SchemaVersion   int                `json:"schema_version"`
	ProtocolVersion string             `json:"protocol_version"`
	SystemID        string             `json:"system_id"`
	Task            Task               `json:"task"`
	CorpusSHA256    string             `json:"corpus_sha256"`
	ResultsSHA256   string             `json:"results_sha256"`
	Attempts        []PromotionAttempt `json:"attempts"`
}

// ReadPromotionAttemptEvidence strictly decodes one charged-attempt artifact
// and returns the SHA-256 of the exact bytes supplied by the operator.
func ReadPromotionAttemptEvidence(
	r io.Reader,
) (PromotionAttemptEvidence, string, error) {
	if r == nil {
		return PromotionAttemptEvidence{}, "", fmt.Errorf(
			"promotion attempt evidence reader is nil",
		)
	}
	raw, err := io.ReadAll(io.LimitReader(
		r,
		maxPromotionAttemptEvidenceBytes+1,
	))
	if err != nil {
		return PromotionAttemptEvidence{}, "", fmt.Errorf(
			"read promotion attempt evidence: %w",
			err,
		)
	}
	if len(raw) == 0 {
		return PromotionAttemptEvidence{}, "", fmt.Errorf(
			"promotion attempt evidence is empty",
		)
	}
	if len(raw) > maxPromotionAttemptEvidenceBytes {
		return PromotionAttemptEvidence{}, "", fmt.Errorf(
			"promotion attempt evidence exceeds %d bytes",
			maxPromotionAttemptEvidenceBytes,
		)
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return PromotionAttemptEvidence{}, "", fmt.Errorf(
			"decode promotion attempt evidence: %w",
			err,
		)
	}
	var evidence PromotionAttemptEvidence
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&evidence); err != nil {
		return PromotionAttemptEvidence{}, "", fmt.Errorf(
			"decode promotion attempt evidence: %w",
			err,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return PromotionAttemptEvidence{}, "", fmt.Errorf(
				"decode promotion attempt evidence: multiple JSON values",
			)
		}
		return PromotionAttemptEvidence{}, "", fmt.Errorf(
			"decode promotion attempt evidence: trailing data: %w",
			err,
		)
	}
	return evidence, sha256Hex(raw), nil
}

// ResultsIdentity returns the SHA-256 of the exact canonical JSONL emitted by
// WriteResults. Promotion CLI callers additionally bind the raw file hash.
func ResultsIdentity(results []ResultRecord) (string, error) {
	var payload bytes.Buffer
	if err := WriteResults(&payload, results); err != nil {
		return "", err
	}
	return sha256Hex(payload.Bytes()), nil
}

// PromotionCostBasis binds the production volume and service-tier
// normalizations used by every candidate in one task. Source hashes identify
// the exact external artifacts from which the operator obtained these inputs.
type PromotionCostBasis struct {
	ObservedWindowStartUTC   string `json:"observed_window_start_utc"`
	ObservedWindowEndUTC     string `json:"observed_window_end_utc"`
	ObservedEligibleRequests int64  `json:"observed_eligible_requests"`
	VolumeEvidenceSHA256     string `json:"volume_evidence_sha256"`
	PricingEvidenceSHA256    string `json:"pricing_evidence_sha256"`
	OpenAIBatchMultiplierPPM int64  `json:"openai_batch_multiplier_ppm"`
	OpenAIFlexMultiplierPPM  int64  `json:"openai_flex_multiplier_ppm"`
}

// PromotionRoster is the separately frozen, source-path-free declaration of
// every comparison that may enter one task's final holdout. Freezing and
// publishing its exact hash before execution prevents result-aware candidate
// omission from changing Holm multiplicity or cheapest-passing selection.
type PromotionRoster struct {
	SchemaVersion        int                         `json:"schema_version"`
	ProtocolVersion      string                      `json:"protocol_version"`
	Task                 Task                        `json:"task"`
	SystemManifestSHA256 string                      `json:"system_manifest_sha256"`
	RepeatSampling       PromotionRepeatSamplingPlan `json:"repeat_sampling"`
	Systems              []PromotionRosterSystem     `json:"systems"`
}

type PromotionRosterSystem struct {
	Role                      PromotionSystemRole         `json:"role"`
	SystemID                  string                      `json:"system_id"`
	ControlSystemID           string                      `json:"control_system_id,omitempty"`
	ProductionControlSystemID string                      `json:"production_control_system_id,omitempty"`
	DecisionThresholds        PromotionDecisionThresholds `json:"decision_thresholds"`
	DeploymentServiceTier     PromotionServiceTier        `json:"deployment_service_tier"`
}

// ReadPromotionRoster strictly decodes a frozen task roster and returns the
// SHA-256 of the exact bytes supplied by the operator.
func ReadPromotionRoster(r io.Reader) (PromotionRoster, string, error) {
	if r == nil {
		return PromotionRoster{}, "", fmt.Errorf(
			"promotion roster reader is nil",
		)
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxPromotionRosterBytes+1))
	if err != nil {
		return PromotionRoster{}, "", fmt.Errorf(
			"read promotion roster: %w",
			err,
		)
	}
	if len(raw) == 0 {
		return PromotionRoster{}, "", fmt.Errorf(
			"promotion roster is empty",
		)
	}
	if len(raw) > maxPromotionRosterBytes {
		return PromotionRoster{}, "", fmt.Errorf(
			"promotion roster exceeds %d bytes",
			maxPromotionRosterBytes,
		)
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return PromotionRoster{}, "", fmt.Errorf(
			"decode promotion roster: %w",
			err,
		)
	}
	var roster PromotionRoster
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&roster); err != nil {
		return PromotionRoster{}, "", fmt.Errorf(
			"decode promotion roster: %w",
			err,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return PromotionRoster{}, "", fmt.Errorf(
				"decode promotion roster: multiple JSON values",
			)
		}
		return PromotionRoster{}, "", fmt.Errorf(
			"decode promotion roster: trailing data: %w",
			err,
		)
	}
	return roster, sha256Hex(raw), nil
}

// PromotionSystemEvidence is the already-read, no-network input for one exact
// control or candidate system.
type PromotionSystemEvidence struct {
	Role                        PromotionSystemRole
	SystemID                    string
	ControlSystemID             string
	ProductionControlSystemID   string
	DecisionThresholds          PromotionDecisionThresholds
	DeploymentServiceTier       PromotionServiceTier
	Results                     []ResultRecord
	ResultsArtifactSHA256       string
	AttemptEvidence             PromotionAttemptEvidence
	AttemptEvidenceSHA256       string
	RouteSnapshotSHA256         string
	RouteSnapshotVerified       bool
	RepeatResults               [][]ResultRecord
	RepeatArtifactSHA256        []string
	RepeatAttemptEvidence       []PromotionAttemptEvidence
	RepeatAttemptEvidenceSHA256 []string
}

// PromotionCampaignPlanEvidence carries one exact preregistered campaign
// plan and the SHA-256 of the bytes accepted by ReadCampaignPlan. Primary
// holdout and nondeterministic repeat executions use separate plans because a
// CampaignPlan deliberately represents exactly one immutable stage.
type PromotionCampaignPlanEvidence struct {
	Plan            CampaignPlan
	SHA256          string
	exactSHA256     string
	canonicalSHA256 string
}

// ReadPromotionCampaignPlanEvidence is the only constructor for promotion
// campaign input. It retains an internal canonical integrity seal alongside
// the exact raw-byte digest, so an in-memory gate or matrix mutation cannot be
// paired with the old artifact SHA and presented as the original plan.
func ReadPromotionCampaignPlanEvidence(
	r io.Reader,
) (PromotionCampaignPlanEvidence, error) {
	if r == nil {
		return PromotionCampaignPlanEvidence{}, fmt.Errorf(
			"promotion campaign plan reader is nil",
		)
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxCampaignPlanBytes+1))
	if err != nil {
		return PromotionCampaignPlanEvidence{}, fmt.Errorf(
			"read promotion campaign plan: %w",
			err,
		)
	}
	if len(raw) > maxCampaignPlanBytes {
		return PromotionCampaignPlanEvidence{}, fmt.Errorf(
			"promotion campaign plan exceeds the %d-byte limit",
			maxCampaignPlanBytes,
		)
	}
	plan, digest, err := ReadCampaignPlan(bytes.NewReader(raw))
	if err != nil {
		return PromotionCampaignPlanEvidence{}, err
	}
	canonical, err := canonicalJSONIdentity(plan)
	if err != nil {
		return PromotionCampaignPlanEvidence{}, fmt.Errorf(
			"canonical campaign plan: %w",
			err,
		)
	}
	return PromotionCampaignPlanEvidence{
		Plan:            plan,
		SHA256:          digest,
		exactSHA256:     digest,
		canonicalSHA256: canonical,
	}, nil
}

func (e PromotionCampaignPlanEvidence) validateIntegrity(label string) error {
	if err := validateSHA256(label+"_sha256", e.SHA256); err != nil {
		return err
	}
	if e.SHA256 != e.exactSHA256 {
		return fmt.Errorf(
			"%s SHA-256 was mutated after its exact bytes were read",
			label,
		)
	}
	if err := validateSHA256(
		label+"_canonical_sha256",
		e.canonicalSHA256,
	); err != nil {
		return fmt.Errorf(
			"%s must be loaded from exact bytes: %w",
			label,
			err,
		)
	}
	canonical, err := canonicalJSONIdentity(e.Plan)
	if err != nil {
		return fmt.Errorf("%s canonical identity: %w", label, err)
	}
	if canonical != e.canonicalSHA256 {
		return fmt.Errorf(
			"%s was mutated after its exact bytes were read",
			label,
		)
	}
	return nil
}

// PromotionBundleInput contains every artifact required to construct one
// machine-verifiable, task-specific promotion decision.
type PromotionBundleInput struct {
	Task                      Task
	FullHoldout               Corpus
	CorpusPlanBytes           []byte
	CorpusPlanSHA256          string
	GoldClosureManifest       GoldClosureManifest
	GoldClosureManifestSHA256 string
	FinalistRoster            FinalistRosterManifest
	FinalistRosterSHA256      string
	SystemManifest            SystemManifest
	SystemManifestSHA256      string
	PromotionRoster           PromotionRoster
	PromotionRosterSHA256     string
	CampaignPlan              PromotionCampaignPlanEvidence
	RepeatCampaignPlan        *PromotionCampaignPlanEvidence
	EvaluatorBuildSHA256      string
	CostBasis                 PromotionCostBasis
	Systems                   []PromotionSystemEvidence
	ComparisonOptions         ComparisonOptions
}

// PromotionLatencyComparison binds the candidate's latency decision to one
// exact campaign-declared control. Both the deployed and normalized controls
// must pass their own absolute gates and the candidate must stay within the
// preregistered relative p95 slowdown ceiling against each.
type PromotionLatencyComparison struct {
	ControlSystemID    string  `json:"control_system_id"`
	ControlP95MS       int64   `json:"control_p95_ms"`
	RelativeSlowdown   float64 `json:"relative_slowdown"`
	MaximumSlowdown    float64 `json:"maximum_slowdown"`
	ControlGatePassed  bool    `json:"control_gate_passed"`
	RelativeGatePassed bool    `json:"relative_gate_passed"`
	Passed             bool    `json:"passed"`
}

// PromotionLatencyReport evaluates complete primary request timing against
// the exact campaign gate. Missing timing is an evidence-completeness failure;
// measured latency outside a preregistered bound is a promotion-gate failure.
type PromotionLatencyReport struct {
	Gate                        CampaignLatencyGate          `json:"gate"`
	Observed                    int                          `json:"observed"`
	Missing                     int                          `json:"missing"`
	TimeoutCount                int                          `json:"timeout_count"`
	DeadlineExceededCount       int                          `json:"deadline_exceeded_count"`
	TimeoutUpperConfidenceBound float64                      `json:"timeout_upper_confidence_bound"`
	P50MS                       int64                        `json:"p50_ms"`
	P95MS                       int64                        `json:"p95_ms"`
	P99MS                       int64                        `json:"p99_ms"`
	MaxMS                       int64                        `json:"max_ms"`
	DeadlineMS                  int64                        `json:"deadline_ms"`
	Complete                    bool                         `json:"complete"`
	AbsoluteGatePassed          bool                         `json:"absolute_gate_passed"`
	Comparisons                 []PromotionLatencyComparison `json:"comparisons,omitempty"`
	Passed                      bool                         `json:"passed"`
}

// CampaignBoundedRateReport records the exact numerator, denominator, point
// estimate, and preregistered one-sided confidence bound used by an absolute
// campaign gate. Utility uses a lower bound; harm/failure uses an upper bound.
type CampaignBoundedRateReport struct {
	Metric           string  `json:"metric"`
	Count            int     `json:"count"`
	Denominator      int     `json:"denominator"`
	Rate             float64 `json:"rate"`
	OneSidedBound    float64 `json:"one_sided_bound"`
	BoundDirection   string  `json:"bound_direction"`
	Threshold        float64 `json:"threshold"`
	EvidenceComplete bool    `json:"evidence_complete"`
	Passed           bool    `json:"passed"`
}

type CampaignHarmUtilityControlReport struct {
	ControlSystemID  string  `json:"control_system_id"`
	HarmUCBDelta     float64 `json:"harm_ucb_delta"`
	UtilityLCBDelta  float64 `json:"utility_lcb_delta"`
	EvidenceComplete bool    `json:"evidence_complete"`
	HarmPassed       bool    `json:"harm_passed"`
	UtilityPassed    bool    `json:"utility_passed"`
	Passed           bool    `json:"passed"`
}

type CampaignHarmUtilityReport struct {
	Gate               CampaignHarmUtilityGate            `json:"gate"`
	SeverityOneErrors  int                                `json:"severity_one_errors"`
	Harm               CampaignBoundedRateReport          `json:"harm"`
	Utility            CampaignBoundedRateReport          `json:"utility"`
	ControlComparisons []CampaignHarmUtilityControlReport `json:"control_comparisons,omitempty"`
	EvidenceComplete   bool                               `json:"evidence_complete"`
	AbsolutePassed     bool                               `json:"absolute_passed"`
	Passed             bool                               `json:"passed"`
}

// FirstPassSchemaReport proves the relationship between initial attempts,
// optional one-shot repairs, final results, and all charged usage.
type FirstPassSchemaReport struct {
	Gate                          CampaignSchemaReliabilityGate `json:"gate"`
	Cases                         int                           `json:"cases"`
	FirstPassValid                int                           `json:"first_pass_valid"`
	FirstPassValidity             float64                       `json:"first_pass_validity"`
	FirstPassFloor                float64                       `json:"first_pass_floor"`
	SchemaRepairCases             int                           `json:"schema_repair_cases"`
	RuntimeFailureCases           int                           `json:"runtime_failure_cases"`
	Answered                      int                           `json:"answered"`
	AnsweredRate                  float64                       `json:"answered_rate"`
	CallErrorUpperConfidenceBound float64                       `json:"call_error_upper_confidence_bound"`
	MaximumRepairsObserved        int                           `json:"maximum_repairs_observed"`
	FinalValid                    int                           `json:"final_valid"`
	FinalValidity                 float64                       `json:"final_validity"`
	Usage                         UsageTotals                   `json:"usage"`
	UsageFullyAccounted           bool                          `json:"usage_fully_accounted"`
	AtMostOneRepair               bool                          `json:"at_most_one_repair"`
	NoRepairWhenFirstPass100      bool                          `json:"no_repair_when_first_pass_100"`
	EvidenceComplete              bool                          `json:"evidence_complete"`
	Passed                        bool                          `json:"passed"`
}

// CalibrationBin is one fixed-width confidence bin. Correct is the count of
// gold-correct decisive actions, not an aggregate model score.
type CalibrationBin struct {
	LowerInclusive   float64 `json:"lower_inclusive"`
	UpperInclusive   float64 `json:"upper_inclusive"`
	Samples          int     `json:"samples"`
	Correct          int     `json:"correct"`
	MeanConfidence   float64 `json:"mean_confidence"`
	ObservedAccuracy float64 `json:"observed_accuracy"`
}

// ConfidenceCalibrationSlice reports calibration on one primary suite.
type ConfidenceCalibrationSlice struct {
	Suite         string           `json:"suite"`
	EligibleCases int              `json:"eligible_cases"`
	UniqueGroups  int              `json:"unique_groups"`
	ExcludedCases int              `json:"excluded_cases"`
	ECE           float64          `json:"ece"`
	Brier         float64          `json:"brier"`
	Bins          []CalibrationBin `json:"bins"`
	MeetsMinimum  bool             `json:"meets_minimum"`
	Passed        bool             `json:"passed"`
}

// ConfidenceCalibrationReport is applicable only to tasks whose normalized
// result contract returns confidence. It keeps natural and safety calibration
// separate and never prevalence-weights the safety top-up.
type ConfidenceCalibrationReport struct {
	Gate                CampaignCalibrationGate      `json:"gate"`
	Applicable          bool                         `json:"applicable"`
	NotApplicableReason string                       `json:"not_applicable_reason,omitempty"`
	EvidenceComplete    bool                         `json:"evidence_complete"`
	Definition          string                       `json:"definition"`
	ECECeiling          float64                      `json:"ece_ceiling"`
	BrierCeiling        float64                      `json:"brier_ceiling"`
	MinimumGroups       int                          `json:"minimum_groups"`
	Suites              []ConfidenceCalibrationSlice `json:"suites"`
	Passed              bool                         `json:"passed"`
}

// InstabilitySlice reports group-cluster instability within one preregistered
// suite. Promotion never prevalence-weights the natural and safety strata.
type InstabilitySlice struct {
	Suite                     string  `json:"suite"`
	SampledGroups             int     `json:"sampled_groups"`
	SampledCases              int     `json:"sampled_cases"`
	DivergentGroups           int     `json:"divergent_groups"`
	InstabilityRate           float64 `json:"instability_rate"`
	UpperConfidenceBound      float64 `json:"upper_confidence_bound"`
	FinalActionFlipGroups     int     `json:"final_action_flip_groups"`
	FinalActionFlipRate       float64 `json:"final_action_flip_rate"`
	FinalActionFlipUpperBound float64 `json:"final_action_flip_upper_bound"`
	Passed                    bool    `json:"passed"`
}

// InstabilityReport is required only when the manifest cannot prove both
// temperature zero and a fixed seed for the route. Nondeterministic systems
// rerun only the roster-bound stratified sample, not the complete holdout.
type InstabilityReport struct {
	Gate                        CampaignStabilityGate `json:"gate"`
	DeterministicControlsProven bool                  `json:"deterministic_controls_proven"`
	RepeatsRequired             bool                  `json:"repeats_required"`
	RequiredRepeatRuns          int                   `json:"required_repeat_runs"`
	ObservedRepeatRuns          int                   `json:"observed_repeat_runs"`
	SamplingAlgorithmID         string                `json:"sampling_algorithm_id"`
	SampleCorpusSHA256          string                `json:"sample_corpus_sha256"`
	ConfidenceMethod            string                `json:"confidence_method"`
	ConfidenceLevel             float64               `json:"confidence_level"`
	Cases                       int                   `json:"cases"`
	DivergentCases              int                   `json:"divergent_cases"`
	Groups                      int                   `json:"groups"`
	DivergentGroups             int                   `json:"divergent_groups"`
	InstabilityRate             float64               `json:"instability_rate"`
	UpperConfidenceBound        float64               `json:"upper_confidence_bound"`
	Ceiling                     float64               `json:"ceiling"`
	RepeatAgreement             float64               `json:"repeat_agreement"`
	MaximumUtilityDrift         float64               `json:"maximum_utility_drift"`
	MaximumHarmDrift            float64               `json:"maximum_harm_drift"`
	HarmfulRepeatFlips          int                   `json:"harmful_repeat_flips"`
	FinalActionFlipGroups       int                   `json:"final_action_flip_groups"`
	FinalActionFlipRate         float64               `json:"final_action_flip_rate"`
	FinalActionFlipUpperBound   float64               `json:"final_action_flip_upper_bound"`
	EvidenceComplete            bool                  `json:"evidence_complete"`
	Suites                      []InstabilitySlice    `json:"suites"`
	Passed                      bool                  `json:"passed"`
}

type CampaignSelectiveRiskPoint struct {
	ConfidenceThreshold float64 `json:"confidence_threshold"`
	Covered             int     `json:"covered"`
	Coverage            float64 `json:"coverage"`
	Errors              int     `json:"errors"`
	Risk                float64 `json:"risk"`
}

type CampaignSelectiveRiskControlReport struct {
	ControlSystemID  string  `json:"control_system_id"`
	ControlCoverage  float64 `json:"control_coverage"`
	RequiredCoverage float64 `json:"required_coverage"`
	CandidateRisk    float64 `json:"candidate_risk"`
	EvidenceComplete bool    `json:"evidence_complete"`
	CoveragePassed   bool    `json:"coverage_passed"`
	RiskPassed       bool    `json:"risk_passed"`
	Passed           bool    `json:"passed"`
}

type CampaignSelectiveRiskReport struct {
	Gate                CampaignSelectiveRiskGate            `json:"gate"`
	Applicable          bool                                 `json:"applicable"`
	NotApplicableReason string                               `json:"not_applicable_reason,omitempty"`
	EligibleCases       int                                  `json:"eligible_cases"`
	CoveredCases        int                                  `json:"covered_cases"`
	Coverage            float64                              `json:"coverage"`
	AURC                float64                              `json:"aurc"`
	Curve               []CampaignSelectiveRiskPoint         `json:"curve,omitempty"`
	ControlComparisons  []CampaignSelectiveRiskControlReport `json:"control_comparisons,omitempty"`
	EvidenceComplete    bool                                 `json:"evidence_complete"`
	Passed              bool                                 `json:"passed"`
}

type CampaignCriticalStratumReport struct {
	Gate                    CampaignCriticalStratumGate `json:"gate"`
	Cases                   int                         `json:"cases"`
	SeverityOneErrors       int                         `json:"severity_one_errors"`
	HarmApplicable          bool                        `json:"harm_applicable"`
	HarmNotApplicableReason string                      `json:"harm_not_applicable_reason,omitempty"`
	Harm                    CampaignBoundedRateReport   `json:"harm"`
	Utility                 CampaignBoundedRateReport   `json:"utility"`
	EvidenceComplete        bool                        `json:"evidence_complete"`
	Passed                  bool                        `json:"passed"`
}

type CampaignCostControlReport struct {
	ControlSystemID  string  `json:"control_system_id"`
	CostRatio        float64 `json:"cost_ratio"`
	EvidenceComplete bool    `json:"evidence_complete"`
	Passed           bool    `json:"passed"`
}

type CampaignCostGateReport struct {
	Gate                             CampaignCostEffectivenessGate `json:"gate"`
	CostPer1000CasesMicroUSD         float64                       `json:"cost_per_1000_cases_micro_usd"`
	CostPer1000CasesPassed           bool                          `json:"cost_per_1000_cases_passed"`
	ProjectedMonthlyMicroUSD         float64                       `json:"projected_monthly_micro_usd"`
	ProjectedMonthlyPassed           bool                          `json:"projected_monthly_passed"`
	CorrectSafeActions               int                           `json:"correct_safe_actions"`
	CostPerCorrectSafeActionMicroUSD float64                       `json:"cost_per_correct_safe_action_micro_usd"`
	CostPerCorrectSafeActionPassed   bool                          `json:"cost_per_correct_safe_action_passed"`
	ControlComparisons               []CampaignCostControlReport   `json:"control_comparisons,omitempty"`
	EvidenceComplete                 bool                          `json:"evidence_complete"`
	AbsolutePassed                   bool                          `json:"absolute_passed"`
	Passed                           bool                          `json:"passed"`
}

// RequestShapeReport makes the one-case limitation explicit. Reusing one
// provider request ID across cases is packed execution and is not promotable
// under v2.
type RequestShapeReport struct {
	Variant              string `json:"variant"`
	UniqueRequests       int    `json:"unique_requests"`
	PackedRequests       int    `json:"packed_requests"`
	DuplicateRequestIDs  int    `json:"duplicate_request_ids"`
	MissingRequestIDs    int    `json:"missing_request_ids"`
	OneCaseVerified      bool   `json:"one_case_verified"`
	PackedVariantSupport string `json:"packed_variant_support"`
}

// CostProjection preserves measured usage before scaling it to the observed
// eligible request volume. OpenAI service-tier normalizations are populated
// only for direct OpenAI systems.
type CostProjection struct {
	MeasuredCases                        int                  `json:"measured_cases"`
	MeasuredProviderRequests             int                  `json:"measured_provider_requests"`
	MeasuredUsage                        UsageTotals          `json:"measured_usage"`
	ObservedWindowEligibleRequests       int64                `json:"observed_window_eligible_requests"`
	ProjectedThirtyDayEligibleRequests   float64              `json:"projected_30_day_eligible_requests"`
	MeasuredProviderCreditsUSD           float64              `json:"measured_provider_credits_usd"`
	MeasuredCashCostUSD                  float64              `json:"measured_cash_cost_usd"`
	RecurringCashMultiplier              float64              `json:"recurring_cash_multiplier"`
	OpenRouterOneTimeMinimumFeeUSD       float64              `json:"openrouter_one_time_minimum_fee_usd,omitempty"`
	ProjectedThirtyDayProviderCreditsUSD float64              `json:"projected_30_day_provider_credits_usd"`
	ProjectedThirtyDayCashCostUSD        float64              `json:"projected_30_day_cash_cost_usd"`
	ProjectedCostPerThousandUSD          float64              `json:"projected_cost_per_1000_usd"`
	OpenAIStandardNormalizedUSD          *float64             `json:"openai_standard_normalized_30_day_usd,omitempty"`
	OpenAIBatchNormalizedUSD             *float64             `json:"openai_batch_normalized_30_day_usd,omitempty"`
	OpenAIFlexNormalizedUSD              *float64             `json:"openai_flex_normalized_30_day_usd,omitempty"`
	SelectionServiceTier                 PromotionServiceTier `json:"selection_service_tier"`
	SelectionProjectedThirtyDayUSD       float64              `json:"selection_projected_30_day_usd"`
}

// PromotionIUTComponent preserves each elementary one-sided non-inferiority
// p-value used by the candidate-level intersection-union test. A candidate
// clears the conjunction only when every component clears; equivalently, its
// valid conjunction p-value is the maximum component p-value.
type PromotionIUTComponent struct {
	Comparator         string  `json:"comparator"`
	ComparatorSystemID string  `json:"comparator_system_id"`
	Metric             string  `json:"metric"`
	UnadjustedP        float64 `json:"unadjusted_p"`
}

// HolmHypothesis reports one candidate-level intersection-union hypothesis
// after adjustment across every candidate in the frozen task roster.
type HolmHypothesis struct {
	CandidateSystemID string                  `json:"candidate_system_id"`
	Metric            string                  `json:"metric"`
	Components        []PromotionIUTComponent `json:"components"`
	UnadjustedP       float64                 `json:"unadjusted_p"`
	AdjustedP         float64                 `json:"adjusted_p"`
	Alpha             float64                 `json:"alpha"`
	Passed            bool                    `json:"passed"`
}

type HolmFamilyReport struct {
	Task       Task             `json:"task"`
	Metric     string           `json:"metric"`
	Method     string           `json:"method"`
	Hypotheses []HolmHypothesis `json:"hypotheses"`
}

// PromotionSystemReport contains no source text or provider response body.
// Each candidate exposes every independent gate; Eligible never uses an
// aggregate score to hide a failed gate.
type PromotionSystemReport struct {
	Role                             PromotionSystemRole             `json:"role"`
	ControlSystemID                  string                          `json:"control_system_id,omitempty"`
	ProductionControlSystemID        string                          `json:"production_control_system_id,omitempty"`
	DecisionThresholds               PromotionDecisionThresholds     `json:"decision_thresholds"`
	Lane                             EvaluationLane                  `json:"lane"`
	System                           SystemDescriptor                `json:"system"`
	ResultsArtifactSHA256            string                          `json:"results_artifact_sha256"`
	CanonicalResultsSHA256           string                          `json:"canonical_results_sha256"`
	AttemptEvidenceSHA256            string                          `json:"attempt_evidence_sha256"`
	RouteSnapshotSHA256              string                          `json:"route_snapshot_sha256,omitempty"`
	RouteSnapshotVerified            bool                            `json:"route_snapshot_verified"`
	RepeatArtifactSHA256             []string                        `json:"repeat_artifact_sha256,omitempty"`
	RepeatAttemptEvidenceSHA256      []string                        `json:"repeat_attempt_evidence_sha256,omitempty"`
	CampaignRun                      CampaignRunBinding              `json:"campaign_run"`
	RepeatCampaignRuns               []CampaignRunBinding            `json:"repeat_campaign_runs,omitempty"`
	ScoreReportSHA256                string                          `json:"score_report_sha256"`
	Score                            ScoreReport                     `json:"score"`
	HarmUtility                      CampaignHarmUtilityReport       `json:"campaign_harm_utility"`
	Latency                          PromotionLatencyReport          `json:"latency"`
	ComparisonReportSHA256           string                          `json:"comparison_report_sha256,omitempty"`
	Comparison                       *ComparisonReport               `json:"comparison,omitempty"`
	ProductionComparisonReportSHA256 string                          `json:"production_comparison_report_sha256,omitempty"`
	ProductionComparison             *ComparisonReport               `json:"production_comparison,omitempty"`
	FirstPassSchema                  FirstPassSchemaReport           `json:"first_pass_schema"`
	Calibration                      ConfidenceCalibrationReport     `json:"confidence_calibration"`
	Instability                      InstabilityReport               `json:"instability"`
	SelectiveRisk                    CampaignSelectiveRiskReport     `json:"selective_risk"`
	CriticalStrata                   []CampaignCriticalStratumReport `json:"critical_strata"`
	RequestShape                     RequestShapeReport              `json:"request_shape"`
	Cost                             CostProjection                  `json:"cost"`
	CostGate                         CampaignCostGateReport          `json:"campaign_cost"`
	CoreGatesPassed                  bool                            `json:"core_gates_passed"`
	HolmPassed                       bool                            `json:"holm_passed"`
	EvidenceComplete                 bool                            `json:"evidence_complete"`
	CampaignGatesPassed              bool                            `json:"campaign_gates_passed"`
	Eligible                         bool                            `json:"eligible"`
	ReasonCodes                      []string                        `json:"reason_codes"`
}

type PromotionBundleBindings struct {
	HoldoutCorpusSHA256              string `json:"holdout_corpus_sha256"`
	FullTaskCorpusSHA256             string `json:"full_task_corpus_sha256"`
	CorpusPlanSHA256                 string `json:"corpus_plan_sha256"`
	GoldClosureManifestSHA256        string `json:"gold_closure_manifest_sha256"`
	DevelopmentClosureManifestSHA256 string `json:"development_closure_manifest_sha256"`
	FinalistRosterManifestSHA256     string `json:"finalist_roster_manifest_sha256"`
	SystemManifestSHA256             string `json:"system_manifest_sha256"`
	PromotionRosterSHA256            string `json:"promotion_roster_sha256"`
	CampaignPlanSHA256               string `json:"campaign_plan_sha256"`
	RepeatCampaignPlanSHA256         string `json:"repeat_campaign_plan_sha256,omitempty"`
	EvaluatorBuildSHA256             string `json:"evaluator_build_sha256"`
	VolumeEvidenceSHA256             string `json:"volume_evidence_sha256"`
	PricingEvidenceSHA256            string `json:"pricing_evidence_sha256"`
}

// PromotionBundle is the only artifact that can set ProtocolComplete=true.
// Ordinary pairwise ComparisonReport promotion fields remain fail-closed.
type PromotionBundle struct {
	SchemaVersion    int                     `json:"schema_version"`
	ProtocolVersion  string                  `json:"protocol_version"`
	Task             Task                    `json:"task"`
	Bindings         PromotionBundleBindings `json:"bindings"`
	CostBasis        PromotionCostBasis      `json:"cost_basis"`
	Systems          []PromotionSystemReport `json:"systems"`
	HolmFamilies     []HolmFamilyReport      `json:"holm_families"`
	ProtocolComplete bool                    `json:"protocol_complete"`
	Eligible         bool                    `json:"eligible"`
	Decision         ComparisonDecision      `json:"decision"`
	SelectedSystemID string                  `json:"selected_system_id,omitempty"`
	ReasonCodes      []string                `json:"reason_codes"`
}

// BuildPromotionBundle validates and recomputes one task's complete promotion
// evidence without making provider, production, or network calls.
func BuildPromotionBundle(input PromotionBundleInput) (PromotionBundle, error) {
	if !validTask(input.Task) {
		return PromotionBundle{}, fmt.Errorf("promotion task %q is unsupported", input.Task)
	}
	if err := validateSHA256(
		"corpus_plan_sha256",
		input.CorpusPlanSHA256,
	); err != nil {
		return PromotionBundle{}, err
	}
	corpusPlanBytes := append([]byte(nil), input.CorpusPlanBytes...)
	corpusPlan, parsedCorpusPlanSHA256, err := ReadCorpusPlan(
		bytes.NewReader(corpusPlanBytes),
	)
	if err != nil {
		return PromotionBundle{}, fmt.Errorf("corpus plan: %w", err)
	}
	if parsedCorpusPlanSHA256 != input.CorpusPlanSHA256 {
		return PromotionBundle{}, fmt.Errorf(
			"corpus_plan_sha256 does not match the exact supplied corpus plan bytes",
		)
	}
	if err := validateSHA256(
		"gold_closure_manifest_sha256",
		input.GoldClosureManifestSHA256,
	); err != nil {
		return PromotionBundle{}, err
	}
	if err := validateSHA256(
		"finalist_roster_sha256",
		input.FinalistRosterSHA256,
	); err != nil {
		return PromotionBundle{}, err
	}
	if err := validateSHA256(
		"system_manifest_sha256",
		input.SystemManifestSHA256,
	); err != nil {
		return PromotionBundle{}, err
	}
	if err := validateSHA256(
		"promotion_roster_sha256",
		input.PromotionRosterSHA256,
	); err != nil {
		return PromotionBundle{}, err
	}
	if err := input.CampaignPlan.validateIntegrity("campaign_plan"); err != nil {
		return PromotionBundle{}, err
	}
	if input.RepeatCampaignPlan != nil {
		if err := input.RepeatCampaignPlan.validateIntegrity(
			"repeat_campaign_plan",
		); err != nil {
			return PromotionBundle{}, err
		}
	}
	if err := validateSHA256(
		"evaluator_build_sha256",
		input.EvaluatorBuildSHA256,
	); err != nil {
		return PromotionBundle{}, err
	}
	if err := input.SystemManifest.Validate(); err != nil {
		return PromotionBundle{}, fmt.Errorf("system manifest: %w", err)
	}
	if !corpusPlan.Executable {
		return PromotionBundle{}, fmt.Errorf(
			"promotion requires an executable corpus plan",
		)
	}
	if corpusPlan.PlanID != input.GoldClosureManifest.PlanID ||
		input.CorpusPlanSHA256 != input.GoldClosureManifest.PlanSHA256 {
		return PromotionBundle{}, fmt.Errorf(
			"exact corpus plan does not match the gold closure manifest",
		)
	}
	if err := input.FinalistRoster.Validate(
		input.SystemManifest,
		input.SystemManifestSHA256,
	); err != nil {
		return PromotionBundle{}, fmt.Errorf("finalist roster: %w", err)
	}
	if input.FinalistRosterSHA256 !=
		input.GoldClosureManifest.FinalistRosterManifestSHA256 {
		return PromotionBundle{}, fmt.Errorf(
			"finalist roster does not match the exact manifest frozen before holdout review",
		)
	}
	if input.FinalistRoster.PlanSHA256 !=
		input.GoldClosureManifest.PlanSHA256 ||
		input.FinalistRoster.DevelopmentClosureManifestSHA256 !=
			input.GoldClosureManifest.DevelopmentClosureManifestSHA256 ||
		input.FinalistRoster.DevelopmentCorpusSHA256 !=
			input.GoldClosureManifest.DevelopmentCorpusSHA256 ||
		input.FinalistRoster.SystemManifestSHA256 !=
			input.SystemManifestSHA256 {
		return PromotionBundle{}, fmt.Errorf(
			"finalist roster does not bind the exact plan, development closure, development corpus, and system manifest",
		)
	}
	if input.PromotionRoster.SchemaVersion != SchemaVersion ||
		input.PromotionRoster.ProtocolVersion !=
			PromotionBundleProtocolVersion {
		return PromotionBundle{}, fmt.Errorf(
			"promotion roster has unsupported schema_version or protocol_version",
		)
	}
	if input.PromotionRoster.Task != input.Task ||
		input.PromotionRoster.SystemManifestSHA256 !=
			input.SystemManifestSHA256 {
		return PromotionBundle{}, fmt.Errorf(
			"promotion roster task or system-manifest binding mismatch",
		)
	}
	if err := input.PromotionRoster.RepeatSampling.validate(); err != nil {
		return PromotionBundle{}, fmt.Errorf("promotion roster: %w", err)
	}
	if err := ValidateGoldClosureCorpus(
		input.FullHoldout,
		input.GoldClosureManifest,
	); err != nil {
		return PromotionBundle{}, fmt.Errorf("final holdout: %w", err)
	}
	if input.FullHoldout.SHA256 != input.GoldClosureManifest.HoldoutCorpusSHA256 {
		return PromotionBundle{}, fmt.Errorf(
			"promotion requires the exact final full holdout corpus",
		)
	}
	if !input.GoldClosureManifest.ReviewPhaseSeparated {
		return PromotionBundle{}, fmt.Errorf(
			"promotion requires development closure before finalist freeze and holdout review",
		)
	}
	if err := validateSHA256(
		"development_closure_manifest_sha256",
		input.GoldClosureManifest.DevelopmentClosureManifestSHA256,
	); err != nil {
		return PromotionBundle{}, err
	}
	if err := validateSHA256(
		"finalist_roster_manifest_sha256",
		input.GoldClosureManifest.FinalistRosterManifestSHA256,
	); err != nil {
		return PromotionBundle{}, err
	}
	taskCorpus, err := FilterCorpus(input.FullHoldout, input.Task, 0)
	if err != nil {
		return PromotionBundle{}, fmt.Errorf("full task holdout: %w", err)
	}
	expectedTaskCases := 0
	for _, summary := range input.GoldClosureManifest.Tasks {
		if summary.Task == input.Task {
			expectedTaskCases = summary.HoldoutCases
			break
		}
	}
	if expectedTaskCases <= 0 || len(taskCorpus.Records) != expectedTaskCases {
		return PromotionBundle{}, fmt.Errorf(
			"full task holdout has %d cases, closure manifest says %d",
			len(taskCorpus.Records),
			expectedTaskCases,
		)
	}
	windowDays, err := input.CostBasis.validate()
	if err != nil {
		return PromotionBundle{}, fmt.Errorf("cost basis: %w", err)
	}
	if len(input.Systems) < 2 {
		return PromotionBundle{}, fmt.Errorf(
			"promotion bundle requires at least one control and one candidate",
		)
	}
	if len(input.PromotionRoster.Systems) != len(input.Systems) {
		return PromotionBundle{}, fmt.Errorf(
			"promotion evidence does not exactly match the frozen roster",
		)
	}

	configs := make(map[string]SystemConfig, len(input.SystemManifest.Systems))
	deployedProductionControlID := ""
	deployedProductionControlCount := 0
	for _, config := range input.SystemManifest.Systems {
		configs[config.SystemID] = config
		if config.SupportsTask(input.Task) &&
			config.EvaluationLane == EvaluationLaneProductionFidelity {
			deployedProductionControlID = config.SystemID
			deployedProductionControlCount++
		}
	}
	if deployedProductionControlCount != 1 {
		return PromotionBundle{}, fmt.Errorf(
			"system manifest must contain exactly one production_fidelity system for task %q, got %d",
			input.Task,
			deployedProductionControlCount,
		)
	}
	evidenceByID := make(map[string]PromotionSystemEvidence, len(input.Systems))
	roles := make(map[string]PromotionSystemRole, len(input.Systems))
	for i, evidence := range input.Systems {
		if _, exists := evidenceByID[evidence.SystemID]; exists {
			return PromotionBundle{}, fmt.Errorf(
				"systems[%d]: duplicate system_id %q",
				i,
				evidence.SystemID,
			)
		}
		config, exists := configs[evidence.SystemID]
		if !exists {
			return PromotionBundle{}, fmt.Errorf(
				"systems[%d]: system_id %q is absent from the exact manifest",
				i,
				evidence.SystemID,
			)
		}
		if !config.SupportsTask(input.Task) {
			return PromotionBundle{}, fmt.Errorf(
				"systems[%d]: system %q does not support task %q",
				i,
				evidence.SystemID,
				input.Task,
			)
		}
		if err := evidence.DecisionThresholds.validate(); err != nil {
			return PromotionBundle{}, fmt.Errorf(
				"systems[%d]: decision_thresholds: %w",
				i,
				err,
			)
		}
		switch evidence.Role {
		case PromotionRoleControl:
			if evidence.ControlSystemID != "" ||
				evidence.ProductionControlSystemID != "" {
				return PromotionBundle{}, fmt.Errorf(
					"systems[%d]: a control cannot name either comparator",
					i,
				)
			}
		case PromotionRoleCandidate:
			if evidence.ControlSystemID == "" {
				return PromotionBundle{}, fmt.Errorf(
					"systems[%d]: candidate control_system_id is required",
					i,
				)
			}
			if evidence.ProductionControlSystemID == "" {
				return PromotionBundle{}, fmt.Errorf(
					"systems[%d]: candidate production_control_system_id is required",
					i,
				)
			}
		default:
			return PromotionBundle{}, fmt.Errorf(
				"systems[%d]: unsupported role %q",
				i,
				evidence.Role,
			)
		}
		evidenceByID[evidence.SystemID] = evidence
		roles[evidence.SystemID] = evidence.Role
	}
	rosterByID := make(
		map[string]PromotionRosterSystem,
		len(input.PromotionRoster.Systems),
	)
	for i, rosterSystem := range input.PromotionRoster.Systems {
		if rosterSystem.SystemID == "" {
			return PromotionBundle{}, fmt.Errorf(
				"promotion roster systems[%d].system_id is required",
				i,
			)
		}
		if _, duplicate := rosterByID[rosterSystem.SystemID]; duplicate {
			return PromotionBundle{}, fmt.Errorf(
				"promotion roster has duplicate system_id %q",
				rosterSystem.SystemID,
			)
		}
		evidence, exists := evidenceByID[rosterSystem.SystemID]
		if !exists ||
			evidence.Role != rosterSystem.Role ||
			evidence.ControlSystemID != rosterSystem.ControlSystemID ||
			evidence.ProductionControlSystemID !=
				rosterSystem.ProductionControlSystemID ||
			evidence.DecisionThresholds !=
				rosterSystem.DecisionThresholds ||
			evidence.DeploymentServiceTier !=
				rosterSystem.DeploymentServiceTier {
			return PromotionBundle{}, fmt.Errorf(
				"promotion evidence for %q does not exactly match the frozen roster",
				rosterSystem.SystemID,
			)
		}
		rosterByID[rosterSystem.SystemID] = rosterSystem
	}
	if len(rosterByID) != len(evidenceByID) {
		return PromotionBundle{}, fmt.Errorf(
			"promotion evidence does not exactly match the frozen roster",
		)
	}
	finalistIDs, err := validateFinalistPromotionBinding(
		input.Task,
		input.FinalistRoster,
		input.PromotionRoster,
		input.PromotionRosterSHA256,
		configs,
	)
	if err != nil {
		return PromotionBundle{}, err
	}
	candidates := 0
	controlByLane := make(map[EvaluationLane]string)
	referencedControls := make(map[string]struct{})
	for systemID, evidence := range evidenceByID {
		if evidence.Role != PromotionRoleControl {
			continue
		}
		lane := configs[systemID].EvaluationLane
		if prior := controlByLane[lane]; prior != "" {
			return PromotionBundle{}, fmt.Errorf(
				"promotion lane %q has more than one control: %q and %q",
				lane,
				prior,
				systemID,
			)
		}
		controlByLane[lane] = systemID
	}
	for systemID, evidence := range evidenceByID {
		if evidence.Role != PromotionRoleCandidate {
			continue
		}
		candidates++
		if configs[systemID].EvaluationLane != EvaluationLaneNormalizedStrict {
			return PromotionBundle{}, fmt.Errorf(
				"candidate %q must use the normalized_strict lane",
				systemID,
			)
		}
		if roles[evidence.ControlSystemID] != PromotionRoleControl {
			return PromotionBundle{}, fmt.Errorf(
				"candidate %q names missing or non-control comparator %q",
				systemID,
				evidence.ControlSystemID,
			)
		}
		if configs[evidence.ControlSystemID].EvaluationLane !=
			EvaluationLaneNormalizedStrict {
			return PromotionBundle{}, fmt.Errorf(
				"candidate %q normalized control %q has lane %q, want %q",
				systemID,
				evidence.ControlSystemID,
				configs[evidence.ControlSystemID].EvaluationLane,
				EvaluationLaneNormalizedStrict,
			)
		}
		if configs[evidence.ControlSystemID].Provider != "openai" {
			return PromotionBundle{}, fmt.Errorf(
				"candidate %q normalized control %q is not direct OpenAI",
				systemID,
				evidence.ControlSystemID,
			)
		}
		lane := EvaluationLaneNormalizedStrict
		if controlByLane[lane] != evidence.ControlSystemID {
			return PromotionBundle{}, fmt.Errorf(
				"candidate %q does not use the one frozen control %q for lane %q",
				systemID,
				controlByLane[lane],
				lane,
			)
		}
		if roles[evidence.ProductionControlSystemID] != PromotionRoleControl {
			return PromotionBundle{}, fmt.Errorf(
				"candidate %q names missing or non-control production comparator %q",
				systemID,
				evidence.ProductionControlSystemID,
			)
		}
		productionConfig := configs[evidence.ProductionControlSystemID]
		if evidence.ProductionControlSystemID != deployedProductionControlID ||
			productionConfig.EvaluationLane !=
				EvaluationLaneProductionFidelity ||
			productionConfig.Provider != "openai" {
			return PromotionBundle{}, fmt.Errorf(
				"candidate %q production comparator %q is not the exact deployed production_fidelity system %q",
				systemID,
				evidence.ProductionControlSystemID,
				deployedProductionControlID,
			)
		}
		if controlByLane[EvaluationLaneProductionFidelity] !=
			evidence.ProductionControlSystemID {
			return PromotionBundle{}, fmt.Errorf(
				"candidate %q does not use the one frozen production control %q",
				systemID,
				controlByLane[EvaluationLaneProductionFidelity],
			)
		}
		referencedControls[evidence.ControlSystemID] = struct{}{}
		referencedControls[evidence.ProductionControlSystemID] = struct{}{}
	}
	if candidates == 0 {
		return PromotionBundle{}, fmt.Errorf("promotion bundle has no candidates")
	}
	if candidates != len(finalistIDs) {
		return PromotionBundle{}, fmt.Errorf(
			"promotion candidate count %d differs from frozen finalist count %d",
			candidates,
			len(finalistIDs),
		)
	}
	for _, controlID := range controlByLane {
		if _, referenced := referencedControls[controlID]; !referenced {
			return PromotionBundle{}, fmt.Errorf(
				"promotion control %q is not referenced by any candidate",
				controlID,
			)
		}
	}
	campaign, err := validatePromotionCampaignEvidence(
		input,
		corpusPlan,
		taskCorpus,
		configs,
		evidenceByID,
	)
	if err != nil {
		return PromotionBundle{}, fmt.Errorf("campaign evidence: %w", err)
	}

	options := input.ComparisonOptions
	registeredInference := campaign.gate.Inference
	if options.BootstrapReplicates != 0 &&
		options.BootstrapReplicates != registeredInference.BootstrapReplicates {
		return PromotionBundle{}, fmt.Errorf(
			"comparison bootstrap_replicates differs from the preregistered campaign inference gate",
		)
	}
	if options.BootstrapSeed != 0 &&
		options.BootstrapSeed != registeredInference.BootstrapSeed {
		return PromotionBundle{}, fmt.Errorf(
			"comparison bootstrap_seed differs from the preregistered campaign inference gate",
		)
	}
	options.BootstrapReplicates = registeredInference.BootstrapReplicates
	options.BootstrapSeed = registeredInference.BootstrapSeed
	options.ProductionSuiteBinding = true
	options.ClosureBoundHoldout = true
	options.PromotionMode = false
	options.EvaluatorBuildSHA256 = input.EvaluatorBuildSHA256
	options, err = normalizeComparisonOptions(options)
	if err != nil {
		return PromotionBundle{}, err
	}

	reportByID := make(map[string]*PromotionSystemReport, len(input.Systems))
	resultsByID := make(map[string][]ResultRecord, len(input.Systems))
	orderedIDs := make([]string, 0, len(input.Systems))
	for systemID := range evidenceByID {
		orderedIDs = append(orderedIDs, systemID)
	}
	sort.Strings(orderedIDs)
	for _, systemID := range orderedIDs {
		evidence := evidenceByID[systemID]
		config := configs[systemID]
		report, err := buildPromotionSystemReport(
			taskCorpus,
			config,
			evidence,
			input.PromotionRoster.RepeatSampling,
			input.SystemManifestSHA256,
			input.EvaluatorBuildSHA256,
			input.CostBasis,
			windowDays,
			promotionSystemCampaignEvidence{
				primaryRun: campaign.primaryRuns[systemID],
				repeatRuns: campaign.repeatRuns[systemID],
				gate:       campaign.gate,
			},
		)
		if err != nil {
			return PromotionBundle{}, fmt.Errorf("system %q: %w", systemID, err)
		}
		reportByID[systemID] = &report
		resultsByID[systemID] = evidence.Results
	}

	candidateIDs := make([]string, 0, candidates)
	for _, systemID := range orderedIDs {
		evidence := evidenceByID[systemID]
		if evidence.Role != PromotionRoleCandidate {
			continue
		}
		candidateIDs = append(candidateIDs, systemID)
		normalizedControlEvidence := evidenceByID[evidence.ControlSystemID]
		normalizedComparison, err := CompareWithThresholds(
			taskCorpus,
			resultsByID[evidence.ControlSystemID],
			resultsByID[systemID],
			options,
			normalizedControlEvidence.DecisionThresholds.runtime(),
			evidence.DecisionThresholds.runtime(),
		)
		if err != nil {
			return PromotionBundle{}, fmt.Errorf(
				"candidate %q normalized-control comparison: %w",
				systemID,
				err,
			)
		}
		if len(normalizedComparison.Tasks) != 1 ||
			normalizedComparison.Tasks[0].Task != input.Task {
			return PromotionBundle{}, fmt.Errorf(
				"candidate %q normalized-control comparison did not produce exactly one task report",
				systemID,
			)
		}
		normalizedHash, err := canonicalJSONIdentity(normalizedComparison)
		if err != nil {
			return PromotionBundle{}, err
		}
		productionControlEvidence :=
			evidenceByID[evidence.ProductionControlSystemID]
		productionComparison, err := CompareWithThresholds(
			taskCorpus,
			resultsByID[evidence.ProductionControlSystemID],
			resultsByID[systemID],
			options,
			productionControlEvidence.DecisionThresholds.runtime(),
			evidence.DecisionThresholds.runtime(),
		)
		if err != nil {
			return PromotionBundle{}, fmt.Errorf(
				"candidate %q production-control comparison: %w",
				systemID,
				err,
			)
		}
		if len(productionComparison.Tasks) != 1 ||
			productionComparison.Tasks[0].Task != input.Task {
			return PromotionBundle{}, fmt.Errorf(
				"candidate %q production-control comparison did not produce exactly one task report",
				systemID,
			)
		}
		productionHash, err := canonicalJSONIdentity(productionComparison)
		if err != nil {
			return PromotionBundle{}, err
		}
		systemReport := reportByID[systemID]
		systemReport.Comparison = &normalizedComparison
		systemReport.ComparisonReportSHA256 = normalizedHash
		systemReport.ProductionComparison = &productionComparison
		systemReport.ProductionComparisonReportSHA256 = productionHash
		systemReport.CoreGatesPassed = everyTaskGatePassed(
			normalizedComparison.Tasks[0].Gates,
		) && everyTaskGatePassed(productionComparison.Tasks[0].Gates)
		if !systemReport.CoreGatesPassed {
			systemReport.ReasonCodes = append(
				systemReport.ReasonCodes,
				"normalized_or_production_control_core_gate_not_passed",
			)
		}
		applyPromotionRelativeLatencyGate(
			systemReport,
			[]*PromotionSystemReport{
				reportByID[evidence.ControlSystemID],
				reportByID[evidence.ProductionControlSystemID],
			},
		)
		applyPromotionCampaignControlGates(
			taskCorpus,
			systemReport,
			resultsByID[systemID],
			[]*PromotionSystemReport{
				reportByID[evidence.ControlSystemID],
				reportByID[evidence.ProductionControlSystemID],
			},
			[][]ResultRecord{
				resultsByID[evidence.ControlSystemID],
				resultsByID[evidence.ProductionControlSystemID],
			},
		)
	}

	holmFamilies, holmPass, err := buildPromotionHolmFamilies(
		taskCorpus,
		candidateIDs,
		evidenceByID,
		resultsByID,
		options,
	)
	if err != nil {
		return PromotionBundle{}, err
	}
	for systemID, passed := range holmPass {
		report := reportByID[systemID]
		report.HolmPassed = passed
		if !passed {
			report.ReasonCodes = append(
				report.ReasonCodes,
				"holm_adjusted_noninferiority_not_passed",
			)
		}
	}

	bundle := PromotionBundle{
		SchemaVersion:   SchemaVersion,
		ProtocolVersion: PromotionBundleProtocolVersion,
		Task:            input.Task,
		Bindings: PromotionBundleBindings{
			HoldoutCorpusSHA256:              input.FullHoldout.SHA256,
			FullTaskCorpusSHA256:             taskCorpus.SHA256,
			CorpusPlanSHA256:                 input.CorpusPlanSHA256,
			GoldClosureManifestSHA256:        input.GoldClosureManifestSHA256,
			DevelopmentClosureManifestSHA256: input.GoldClosureManifest.DevelopmentClosureManifestSHA256,
			FinalistRosterManifestSHA256:     input.FinalistRosterSHA256,
			SystemManifestSHA256:             input.SystemManifestSHA256,
			PromotionRosterSHA256:            input.PromotionRosterSHA256,
			CampaignPlanSHA256:               input.CampaignPlan.SHA256,
			EvaluatorBuildSHA256:             input.EvaluatorBuildSHA256,
			VolumeEvidenceSHA256:             input.CostBasis.VolumeEvidenceSHA256,
			PricingEvidenceSHA256:            input.CostBasis.PricingEvidenceSHA256,
		},
		CostBasis:    input.CostBasis,
		HolmFamilies: holmFamilies,
		Decision:     ComparisonInconclusive,
	}
	if input.RepeatCampaignPlan != nil {
		bundle.Bindings.RepeatCampaignPlanSHA256 =
			input.RepeatCampaignPlan.SHA256
	}
	for _, systemID := range orderedIDs {
		report := reportByID[systemID]
		if report.Role == PromotionRoleCandidate {
			report.Eligible = report.EvidenceComplete &&
				report.CoreGatesPassed &&
				report.HolmPassed &&
				report.CampaignGatesPassed &&
				report.FirstPassSchema.Passed &&
				report.Calibration.Passed &&
				report.Instability.Passed &&
				report.RequestShape.OneCaseVerified &&
				report.Latency.Passed &&
				promotionLaneEligible(report.Lane)
		}
		sort.Strings(report.ReasonCodes)
		report.ReasonCodes = compactStrings(report.ReasonCodes)
		bundle.Systems = append(bundle.Systems, *report)
	}

	bundle.ProtocolComplete = true
	for _, report := range bundle.Systems {
		if !report.EvidenceComplete {
			bundle.ProtocolComplete = false
			break
		}
	}
	if !bundle.ProtocolComplete {
		bundle.ReasonCodes = append(
			bundle.ReasonCodes,
			"system_evidence_incomplete",
		)
	}
	var selectable []*PromotionSystemReport
	for systemID, report := range reportByID {
		if roles[systemID] == PromotionRoleCandidate && report.Eligible {
			selectable = append(selectable, report)
		}
	}
	sort.Slice(selectable, func(i, j int) bool {
		left := selectable[i].Cost.SelectionProjectedThirtyDayUSD
		right := selectable[j].Cost.SelectionProjectedThirtyDayUSD
		if math.Abs(left-right) > 1e-12 {
			return left < right
		}
		return selectable[i].System.SystemID < selectable[j].System.SystemID
	})
	if bundle.ProtocolComplete && len(selectable) > 0 {
		bundle.Eligible = true
		bundle.Decision = ComparisonPass
		bundle.SelectedSystemID = selectable[0].System.SystemID
	} else if bundle.ProtocolComplete {
		bundle.Decision = ComparisonFail
		bundle.ReasonCodes = append(
			bundle.ReasonCodes,
			"no_candidate_cleared_every_gate",
		)
	}
	sort.Strings(bundle.ReasonCodes)
	bundle.ReasonCodes = compactStrings(bundle.ReasonCodes)
	return bundle, nil
}

type promotionCampaignContext struct {
	gate        CampaignEffectivenessGate
	primaryRuns map[string]CampaignRunBinding
	repeatRuns  map[string][]CampaignRunBinding
}

func validatePromotionCampaignEvidence(
	input PromotionBundleInput,
	corpusPlan CorpusPlan,
	taskCorpus Corpus,
	configs map[string]SystemConfig,
	evidenceByID map[string]PromotionSystemEvidence,
) (promotionCampaignContext, error) {
	primary := input.CampaignPlan
	if err := primary.Plan.ValidateBindings(
		input.SystemManifest,
		input.SystemManifestSHA256,
		corpusPlan,
		input.CorpusPlanSHA256,
		"",
	); err != nil {
		return promotionCampaignContext{}, fmt.Errorf(
			"holdout plan bindings: %w",
			err,
		)
	}
	if primary.Plan.Stage != CampaignStageHoldout {
		return promotionCampaignContext{}, fmt.Errorf(
			"primary campaign stage is %q, want %q",
			primary.Plan.Stage,
			CampaignStageHoldout,
		)
	}
	if primary.Plan.SystemManifestSHA256 != input.SystemManifestSHA256 {
		return promotionCampaignContext{}, fmt.Errorf(
			"holdout plan system-manifest binding mismatch",
		)
	}
	if primary.Plan.CorpusPlanID != input.GoldClosureManifest.PlanID {
		return promotionCampaignContext{}, fmt.Errorf(
			"holdout campaign corpus_plan_id %q differs from gold closure manifest.plan_id %q",
			primary.Plan.CorpusPlanID,
			input.GoldClosureManifest.PlanID,
		)
	}
	if primary.Plan.CorpusPlanSHA256 != input.GoldClosureManifest.PlanSHA256 {
		return promotionCampaignContext{}, fmt.Errorf(
			"holdout campaign corpus_plan_sha256 differs from gold closure manifest.plan_sha256",
		)
	}
	primaryArtifacts, primaryGate, err := promotionCampaignTaskBindings(
		primary.Plan,
		input.Task,
	)
	if err != nil {
		return promotionCampaignContext{}, fmt.Errorf(
			"holdout plan: %w",
			err,
		)
	}
	if err := validatePromotionCampaignArtifacts(
		primaryArtifacts,
		taskCorpus.SHA256,
		input.GoldClosureManifestSHA256,
		input.GoldClosureManifest.PrivacySidecarSHA256,
	); err != nil {
		return promotionCampaignContext{}, fmt.Errorf(
			"holdout plan: %w",
			err,
		)
	}
	primaryRunSets, err := promotionCampaignRunsForTask(
		primary,
		input.Task,
		configs,
		evidenceByID,
		false,
	)
	if err != nil {
		return promotionCampaignContext{}, fmt.Errorf(
			"holdout plan: %w",
			err,
		)
	}
	primaryRuns := make(map[string]CampaignRunBinding, len(primaryRunSets))
	for systemID, bindings := range primaryRunSets {
		primaryRuns[systemID] = bindings[0]
	}

	repeatSystems := make(map[string]PromotionSystemEvidence)
	for systemID, evidence := range evidenceByID {
		if len(evidence.RepeatResults) > 0 {
			repeatSystems[systemID] = evidence
		}
	}
	context := promotionCampaignContext{
		gate:        primaryGate,
		primaryRuns: primaryRuns,
		repeatRuns:  make(map[string][]CampaignRunBinding),
	}
	if len(repeatSystems) == 0 {
		if input.RepeatCampaignPlan != nil {
			return promotionCampaignContext{}, fmt.Errorf(
				"repeat campaign plan was supplied but no repeat results exist",
			)
		}
		return context, nil
	}
	if input.RepeatCampaignPlan == nil {
		return promotionCampaignContext{}, fmt.Errorf(
			"repeat campaign plan is required when repeat results exist",
		)
	}
	repeat := *input.RepeatCampaignPlan
	if err := repeat.Plan.ValidateBindings(
		input.SystemManifest,
		input.SystemManifestSHA256,
		corpusPlan,
		input.CorpusPlanSHA256,
		"",
	); err != nil {
		return promotionCampaignContext{}, fmt.Errorf(
			"repeat plan bindings: %w",
			err,
		)
	}
	if repeat.Plan.Stage != CampaignStageRepeat {
		return promotionCampaignContext{}, fmt.Errorf(
			"repeat campaign stage is %q, want %q",
			repeat.Plan.Stage,
			CampaignStageRepeat,
		)
	}
	if repeat.Plan.SystemManifestSHA256 != primary.Plan.SystemManifestSHA256 ||
		repeat.Plan.CorpusPlanID != primary.Plan.CorpusPlanID ||
		repeat.Plan.CorpusPlanSHA256 != primary.Plan.CorpusPlanSHA256 ||
		repeat.Plan.BenchmarkEpoch != primary.Plan.BenchmarkEpoch {
		return promotionCampaignContext{}, fmt.Errorf(
			"repeat plan does not match the holdout benchmark, corpus-plan, and system-manifest identities",
		)
	}
	repeatArtifacts, repeatGate, err := promotionCampaignTaskBindings(
		repeat.Plan,
		input.Task,
	)
	if err != nil {
		return promotionCampaignContext{}, fmt.Errorf(
			"repeat plan: %w",
			err,
		)
	}
	if !reflect.DeepEqual(primaryGate, repeatGate) {
		return promotionCampaignContext{}, fmt.Errorf(
			"repeat plan effectiveness gate differs from the holdout plan",
		)
	}
	sample, _, err := promotionRepeatSample(
		taskCorpus,
		input.PromotionRoster.RepeatSampling,
	)
	if err != nil {
		return promotionCampaignContext{}, fmt.Errorf(
			"repeat sample: %w",
			err,
		)
	}
	if err := validatePromotionCampaignArtifacts(
		repeatArtifacts,
		sample.SHA256,
		input.GoldClosureManifestSHA256,
		input.GoldClosureManifest.PrivacySidecarSHA256,
	); err != nil {
		return promotionCampaignContext{}, fmt.Errorf(
			"repeat plan: %w",
			err,
		)
	}
	if !reflect.DeepEqual(
		campaignOptionalBoundArtifact(primaryArtifacts.GoldClosure),
		campaignOptionalBoundArtifact(repeatArtifacts.GoldClosure),
	) || !reflect.DeepEqual(
		campaignOptionalBoundArtifact(primaryArtifacts.PrivacySidecar),
		campaignOptionalBoundArtifact(repeatArtifacts.PrivacySidecar),
	) {
		return promotionCampaignContext{}, fmt.Errorf(
			"repeat plan gold-closure or privacy artifact identity differs from the holdout plan",
		)
	}
	repeatRuns, err := promotionCampaignRunsForTask(
		repeat,
		input.Task,
		configs,
		evidenceByID,
		true,
	)
	if err != nil {
		return promotionCampaignContext{}, fmt.Errorf(
			"repeat plan: %w",
			err,
		)
	}
	for systemID, bindings := range repeatRuns {
		primaryBinding := primaryRuns[systemID]
		for _, binding := range bindings {
			if !reflect.DeepEqual(
				primaryBinding.RouteSnapshot,
				binding.RouteSnapshot,
			) || primaryBinding.ExactEndpointEvidence !=
				binding.ExactEndpointEvidence {
				return promotionCampaignContext{}, fmt.Errorf(
					"repeat plan route artifact identity for system %q differs from the holdout plan",
					systemID,
				)
			}
		}
	}
	for systemID, evidence := range repeatSystems {
		bindings := repeatRuns[systemID]
		if len(bindings) != len(evidence.RepeatResults) {
			return promotionCampaignContext{}, fmt.Errorf(
				"system %q has %d repeat artifacts but %d preregistered repeat runs",
				systemID,
				len(evidence.RepeatResults),
				len(bindings),
			)
		}
		context.repeatRuns[systemID] = bindings
	}
	return context, nil
}

func promotionCampaignTaskBindings(
	plan CampaignPlan,
	task Task,
) (CampaignTaskArtifacts, CampaignEffectivenessGate, error) {
	var artifacts *CampaignTaskArtifacts
	for index := range plan.TaskArtifacts {
		if plan.TaskArtifacts[index].Task == task {
			artifacts = &plan.TaskArtifacts[index]
			break
		}
	}
	if artifacts == nil {
		return CampaignTaskArtifacts{}, CampaignEffectivenessGate{},
			fmt.Errorf("task %q has no artifact binding", task)
	}
	var gate *CampaignEffectivenessGate
	for index := range plan.EffectivenessGates {
		if plan.EffectivenessGates[index].Task == task {
			gate = &plan.EffectivenessGates[index]
			break
		}
	}
	if gate == nil {
		return CampaignTaskArtifacts{}, CampaignEffectivenessGate{},
			fmt.Errorf("task %q has no effectiveness gate", task)
	}
	return *artifacts, *gate, nil
}

func validatePromotionCampaignArtifacts(
	artifacts CampaignTaskArtifacts,
	corpusSHA256 string,
	goldClosureSHA256 string,
	privacySidecarSHA256 string,
) error {
	if artifacts.Corpus.SHA256 != corpusSHA256 {
		return fmt.Errorf("task corpus binding mismatch")
	}
	if artifacts.GoldClosure == nil ||
		artifacts.GoldClosure.SHA256 != goldClosureSHA256 {
		return fmt.Errorf("exact gold-closure binding is required")
	}
	if artifacts.PrivacySidecar == nil ||
		artifacts.PrivacySidecar.SHA256 != privacySidecarSHA256 {
		return fmt.Errorf("exact privacy-sidecar binding is required")
	}
	return nil
}

func promotionCampaignRunsForTask(
	plan PromotionCampaignPlanEvidence,
	task Task,
	configs map[string]SystemConfig,
	evidenceByID map[string]PromotionSystemEvidence,
	repeat bool,
) (map[string][]CampaignRunBinding, error) {
	plannedSystems := make(map[string]CampaignSystemMatrix)
	for _, system := range plan.Plan.Systems {
		if campaignSystemSupportsTask(system, task) {
			plannedSystems[system.SystemID] = system
		}
	}
	if len(plannedSystems) != len(evidenceByID) {
		return nil, fmt.Errorf(
			"task system matrix has %d systems, promotion evidence has %d",
			len(plannedSystems),
			len(evidenceByID),
		)
	}
	for systemID, evidence := range evidenceByID {
		planned, exists := plannedSystems[systemID]
		if !exists {
			return nil, fmt.Errorf(
				"system %q is absent from the task matrix",
				systemID,
			)
		}
		if err := validatePromotionCampaignSystem(
			planned,
			evidence,
			configs[systemID],
		); err != nil {
			return nil, fmt.Errorf("system %q: %w", systemID, err)
		}
	}

	runs := make(map[string][]CampaignRunBinding, len(evidenceByID))
	for _, run := range plan.Plan.Runs {
		if run.Task != task {
			continue
		}
		if _, exists := evidenceByID[run.SystemID]; !exists {
			return nil, fmt.Errorf(
				"run %q names task system %q outside promotion evidence",
				run.RunID,
				run.SystemID,
			)
		}
		binding, err := plan.Plan.RunBinding(plan.SHA256, run.RunID)
		if err != nil {
			return nil, fmt.Errorf("run %q: %w", run.RunID, err)
		}
		if err := validatePromotionCampaignRoute(
			binding,
			configs[run.SystemID],
			evidenceByID[run.SystemID],
		); err != nil {
			return nil, fmt.Errorf("run %q: %w", run.RunID, err)
		}
		runs[run.SystemID] = append(runs[run.SystemID], binding)
	}
	for systemID := range evidenceByID {
		bindings := runs[systemID]
		if repeat {
			sort.Slice(bindings, func(i, j int) bool {
				return bindings[i].RepeatIndex < bindings[j].RepeatIndex
			})
			for index := range bindings {
				if bindings[index].RepeatIndex != index+1 {
					return nil, fmt.Errorf(
						"system %q repeat runs are not contiguous from 1",
						systemID,
					)
				}
			}
		} else if len(bindings) != 1 {
			return nil, fmt.Errorf(
				"system %q requires exactly one holdout run, got %d",
				systemID,
				len(bindings),
			)
		}
		runs[systemID] = bindings
	}
	return runs, nil
}

func campaignSystemSupportsTask(system CampaignSystemMatrix, task Task) bool {
	for _, candidate := range system.Tasks {
		if candidate == task {
			return true
		}
	}
	return false
}

func validatePromotionCampaignSystem(
	planned CampaignSystemMatrix,
	evidence PromotionSystemEvidence,
	config SystemConfig,
) error {
	switch evidence.Role {
	case PromotionRoleCandidate:
		if planned.Role != CampaignSystemRoleCandidate ||
			planned.ControlKind != "" {
			return fmt.Errorf("candidate role binding mismatch")
		}
	case PromotionRoleControl:
		if planned.Role != CampaignSystemRoleControl {
			return fmt.Errorf("control role binding mismatch")
		}
		wantKind := CampaignControlKindNormalized
		if config.EvaluationLane == EvaluationLaneProductionFidelity {
			wantKind = CampaignControlKindDeployed
		}
		if planned.ControlKind != wantKind {
			return fmt.Errorf(
				"control_kind is %q, want %q",
				planned.ControlKind,
				wantKind,
			)
		}
	default:
		return fmt.Errorf("unsupported promotion role %q", evidence.Role)
	}
	return nil
}

func validatePromotionCampaignRoute(
	binding CampaignRunBinding,
	config SystemConfig,
	evidence PromotionSystemEvidence,
) error {
	if config.Provider == "openrouter" {
		if binding.RouteSnapshot == nil ||
			binding.RouteSnapshot.SHA256 != evidence.RouteSnapshotSHA256 {
			return fmt.Errorf("route-snapshot binding mismatch")
		}
		if !binding.ExactEndpointEvidence {
			return fmt.Errorf(
				"formal OpenRouter run lacks exact endpoint evidence",
			)
		}
		return nil
	}
	if binding.RouteSnapshot != nil || binding.ExactEndpointEvidence {
		return fmt.Errorf(
			"direct provider run must not carry OpenRouter route evidence",
		)
	}
	return nil
}

func promotionLatencyReport(
	latency LatencySummary,
	resultCount int,
	gate CampaignLatencyGate,
) PromotionLatencyReport {
	report := PromotionLatencyReport{
		Gate:                  gate,
		Observed:              latency.Observed,
		Missing:               latency.Missing,
		TimeoutCount:          latency.TimeoutCount,
		DeadlineExceededCount: latency.DeadlineExceededCount,
		P50MS:                 latency.P50MS,
		P95MS:                 latency.P95MS,
		P99MS:                 latency.P99MS,
		MaxMS:                 latency.MaxMS,
		DeadlineMS:            latency.DeadlineMS,
	}
	report.TimeoutUpperConfidenceBound = promotionWilsonUpperBound(
		latency.TimeoutCount,
		resultCount,
	)
	report.Complete = resultCount > 0 &&
		latency.Observed == resultCount &&
		latency.Missing == 0 &&
		latency.DeadlineMS == gate.DeadlineMS
	report.AbsoluteGatePassed = report.Complete &&
		latency.P50MS <= gate.MaxP50MS &&
		latency.P95MS <= gate.MaxP95MS &&
		latency.P99MS <= gate.MaxP99MS &&
		report.TimeoutUpperConfidenceBound <= gate.MaxTimeoutUCB+1e-12
	report.Passed = report.AbsoluteGatePassed
	return report
}

func applyPromotionRelativeLatencyGate(
	candidate *PromotionSystemReport,
	controls []*PromotionSystemReport,
) {
	if candidate == nil {
		return
	}
	for _, control := range controls {
		comparison := PromotionLatencyComparison{
			MaximumSlowdown: candidate.Latency.Gate.
				MaxP95RelativeSlowdownVsControl,
		}
		if control != nil {
			comparison.ControlSystemID = control.System.SystemID
			comparison.ControlP95MS = control.Latency.P95MS
			comparison.ControlGatePassed = control.Latency.Passed
			if control.Latency.P95MS > 0 {
				comparison.RelativeSlowdown =
					float64(candidate.Latency.P95MS-control.Latency.P95MS) /
						float64(control.Latency.P95MS)
				comparison.RelativeGatePassed =
					comparison.RelativeSlowdown <=
						comparison.MaximumSlowdown+1e-12
			}
		}
		comparison.Passed = comparison.ControlGatePassed &&
			comparison.RelativeGatePassed
		candidate.Latency.Comparisons = append(
			candidate.Latency.Comparisons,
			comparison,
		)
		candidate.Latency.Passed = candidate.Latency.Passed &&
			comparison.Passed
	}
	if !candidate.Latency.Passed {
		candidate.ReasonCodes = append(
			candidate.ReasonCodes,
			"campaign_latency_gate_not_passed",
		)
	}
}

func promotionCampaignHarmUtilityReport(
	corpus Corpus,
	results []ResultRecord,
	gate CampaignHarmUtilityGate,
) CampaignHarmUtilityReport {
	resultByCase := make(map[string]ResultRecord, len(results))
	for _, result := range results {
		resultByCase[result.CaseID] = result
	}
	report := CampaignHarmUtilityReport{Gate: gate}
	var harmCount, harmTotal int
	var utilityCount, utilityTotal int
	var contentEnglishCorrect, contentEnglishTotal int
	var contentOtherCorrect, contentOtherTotal int
	for _, record := range corpus.Records {
		result := resultByCase[record.CaseID]
		indicators := indicatorsForCase(record, result)
		if indicators.harmEligible {
			harmTotal++
			if indicators.harm {
				harmCount++
			}
		}
		switch record.Task {
		case TaskMatcherExtract:
			required := len(record.MatcherExtract.Expected.Acceptable) > 0 &&
				!record.MatcherExtract.Expected.AllowAbstain
			if !required {
				continue
			}
			utilityTotal++
			if result.Status != ResultStatusOK ||
				result.MatcherExtract.Action != MatcherExtractActionExtract {
				continue
			}
			for _, acceptable := range record.MatcherExtract.Expected.Acceptable {
				if *result.MatcherExtract.Extraction == acceptable {
					utilityCount++
					break
				}
			}
		case TaskMatcherRerank:
			if indicators.successEligible {
				utilityTotal++
				if indicators.success {
					utilityCount++
				}
			}
		case TaskContentFilter:
			if record.ContentFilter.Expected.AllowAbstain {
				continue
			}
			if record.ContentFilter.Expected.Language == LanguageEnglish {
				contentEnglishTotal++
				if goldOutcomeSuccess(record, result) {
					contentEnglishCorrect++
				}
			} else {
				contentOtherTotal++
				if goldOutcomeSuccess(record, result) {
					contentOtherCorrect++
				}
			}
		case TaskJunkPurge:
			if record.JunkPurge.Expected.Disposition == JunkDispositionDelete {
				utilityTotal++
				if goldOutcomeSuccess(record, result) {
					utilityCount++
				}
			}
		}
	}
	report.SeverityOneErrors = harmCount
	report.Harm = CampaignBoundedRateReport{
		Metric: gate.HarmMetric, Count: harmCount, Denominator: harmTotal,
		BoundDirection: "upper", Threshold: gate.MaxHarmRate,
		EvidenceComplete: harmTotal > 0,
	}
	if harmTotal > 0 {
		report.Harm.Rate = float64(harmCount) / float64(harmTotal)
		report.Harm.OneSidedBound = promotionWilsonUpperBound(harmCount, harmTotal)
		report.Harm.Passed = report.Harm.OneSidedBound <= gate.MaxHarmRate+1e-12
	}
	report.Utility = CampaignBoundedRateReport{
		Metric: gate.UtilityMetric, Count: utilityCount, Denominator: utilityTotal,
		BoundDirection: "lower", Threshold: gate.MinUtilityRate,
	}
	if corpus.Records[0].Task == TaskContentFilter {
		report.Utility.Count = contentEnglishCorrect + contentOtherCorrect
		report.Utility.Denominator = contentEnglishTotal + contentOtherTotal
		report.Utility.EvidenceComplete = contentEnglishTotal > 0 && contentOtherTotal > 0
		if report.Utility.EvidenceComplete {
			englishRate := float64(contentEnglishCorrect) / float64(contentEnglishTotal)
			otherRate := float64(contentOtherCorrect) / float64(contentOtherTotal)
			report.Utility.Rate = (englishRate + otherRate) / 2
			// The minimum class-specific one-sided Wilson bound is a
			// conservative lower bound on their balanced mean.
			report.Utility.OneSidedBound = math.Min(
				promotionWilsonLowerBound(contentEnglishCorrect, contentEnglishTotal),
				promotionWilsonLowerBound(contentOtherCorrect, contentOtherTotal),
			)
		}
	} else {
		report.Utility.EvidenceComplete = utilityTotal > 0
		if utilityTotal > 0 {
			report.Utility.Rate = float64(utilityCount) / float64(utilityTotal)
			report.Utility.OneSidedBound = promotionWilsonLowerBound(utilityCount, utilityTotal)
		}
	}
	if report.Utility.EvidenceComplete {
		report.Utility.Passed = report.Utility.OneSidedBound+1e-12 >= gate.MinUtilityRate
	}
	report.EvidenceComplete = report.Harm.EvidenceComplete && report.Utility.EvidenceComplete
	report.AbsolutePassed = report.EvidenceComplete &&
		report.SeverityOneErrors <= gate.MaxSeverityOneErrors &&
		report.Harm.Passed && report.Utility.Passed
	report.Passed = report.AbsolutePassed
	return report
}

func promotionCampaignCriticalStrata(
	corpus Corpus,
	results []ResultRecord,
	gates []CampaignCriticalStratumGate,
) []CampaignCriticalStratumReport {
	resultByCase := make(map[string]ResultRecord, len(results))
	for _, result := range results {
		resultByCase[result.CaseID] = result
	}
	reports := make([]CampaignCriticalStratumReport, 0, len(gates))
	for _, gate := range gates {
		report := CampaignCriticalStratumReport{Gate: gate}
		var harmCount, harmTotal, utilityCount int
		for _, record := range corpus.Records {
			if !hasSlice(record.SliceIDs, gate.SliceID) {
				continue
			}
			report.Cases++
			result := resultByCase[record.CaseID]
			indicators := indicatorsForCase(record, result)
			if indicators.harmEligible {
				harmTotal++
				if indicators.harm {
					harmCount++
				}
			}
			if goldOutcomeSuccess(record, result) {
				utilityCount++
			}
		}
		report.SeverityOneErrors = harmCount
		report.HarmApplicable = harmTotal > 0
		report.Harm = CampaignBoundedRateReport{
			Metric: "critical_stratum_harm", Count: harmCount,
			Denominator: harmTotal, BoundDirection: "upper",
			Threshold: gate.MaxHarmRate, EvidenceComplete: harmTotal > 0,
		}
		if harmTotal > 0 {
			report.Harm.Rate = float64(harmCount) / float64(harmTotal)
			report.Harm.OneSidedBound = promotionWilsonUpperBound(harmCount, harmTotal)
			report.Harm.Passed = report.Harm.OneSidedBound <= gate.MaxHarmRate+1e-12
		} else if report.Cases > 0 {
			// Some plan-required strata deliberately cover the complement of a
			// task's severity-one population. For example,
			// latin_script_non_english measures contentfilter utility while the
			// task harm denominator is wanted English content. That is a
			// structurally inapplicable harm gate, not missing evidence.
			report.HarmNotApplicableReason =
				"no_gold_cases_in_task_harm_denominator"
			report.Harm.EvidenceComplete = true
			report.Harm.Passed = true
		}
		report.Utility = CampaignBoundedRateReport{
			Metric: "critical_stratum_correct_action_rate", Count: utilityCount,
			Denominator: report.Cases, BoundDirection: "lower",
			Threshold: gate.MinUtilityRate, EvidenceComplete: report.Cases > 0,
		}
		if report.Cases > 0 {
			report.Utility.Rate = float64(utilityCount) / float64(report.Cases)
			report.Utility.OneSidedBound = promotionWilsonLowerBound(utilityCount, report.Cases)
			report.Utility.Passed = report.Utility.OneSidedBound+1e-12 >= gate.MinUtilityRate
		}
		report.EvidenceComplete = report.Cases >= gate.MinimumCases &&
			report.Harm.EvidenceComplete && report.Utility.EvidenceComplete
		report.Passed = report.EvidenceComplete &&
			report.SeverityOneErrors <= gate.MaxSeverityOneErrors &&
			report.Harm.Passed && report.Utility.Passed
		reports = append(reports, report)
	}
	return reports
}

type campaignSelectiveObservation struct {
	confidence float64
	correct    bool
}

func promotionCampaignSelectiveRisk(
	corpus Corpus,
	results []ResultRecord,
	gate CampaignSelectiveRiskGate,
) CampaignSelectiveRiskReport {
	report := CampaignSelectiveRiskReport{
		Gate:       gate,
		Applicable: corpus.Records[0].Task != TaskMatcherExtract,
	}
	if !report.Applicable {
		report.NotApplicableReason =
			"matcher_extract result contract has no confidence field"
		report.EvidenceComplete = true
		report.Passed = true
		return report
	}
	resultByCase := make(map[string]ResultRecord, len(results))
	for _, result := range results {
		resultByCase[result.CaseID] = result
	}
	observations := make([]campaignSelectiveObservation, 0, len(corpus.Records))
	for _, record := range corpus.Records {
		eligible := false
		switch record.Task {
		case TaskMatcherRerank:
			eligible = !record.MatcherRerank.Expected.AllowAbstain
		case TaskContentFilter:
			eligible = !record.ContentFilter.Expected.AllowAbstain
		case TaskJunkPurge:
			eligible = record.JunkPurge.Expected.Disposition != JunkDispositionAbstain
		}
		if !eligible {
			continue
		}
		report.EligibleCases++
		result := resultByCase[record.CaseID]
		confidence, covered := promotionConfidence(record, result)
		if covered {
			observations = append(observations, campaignSelectiveObservation{
				confidence: confidence,
				correct:    goldOutcomeSuccess(record, result),
			})
		}
	}
	sort.Slice(observations, func(i, j int) bool {
		return observations[i].confidence > observations[j].confidence
	})
	errorsSeen := 0
	for start := 0; start < len(observations); {
		end := start + 1
		for end < len(observations) &&
			math.Abs(observations[end].confidence-observations[start].confidence) <= 1e-12 {
			end++
		}
		for _, observation := range observations[start:end] {
			if !observation.correct {
				errorsSeen++
			}
		}
		covered := end
		coverage := float64(covered) / float64(report.EligibleCases)
		risk := float64(errorsSeen) / float64(covered)
		report.Curve = append(report.Curve, CampaignSelectiveRiskPoint{
			ConfidenceThreshold: observations[start].confidence,
			Covered:             covered, Coverage: coverage, Errors: errorsSeen, Risk: risk,
		})
		previousCoverage := 0.0
		if len(report.Curve) > 1 {
			previousCoverage = report.Curve[len(report.Curve)-2].Coverage
		}
		report.AURC += (coverage - previousCoverage) * risk
		start = end
	}
	report.CoveredCases = len(observations)
	if report.EligibleCases > 0 {
		report.Coverage = float64(report.CoveredCases) / float64(report.EligibleCases)
	}
	report.EvidenceComplete = report.EligibleCases > 0 && report.CoveredCases > 0
	report.Passed = report.EvidenceComplete && report.AURC <= gate.MaxAURC+1e-12
	return report
}

func selectiveRiskAtCoverage(
	report CampaignSelectiveRiskReport,
	coverage float64,
) (float64, bool) {
	if coverage <= 0 {
		return 0, report.EvidenceComplete
	}
	for _, point := range report.Curve {
		if point.Coverage+1e-12 >= coverage {
			return point.Risk, true
		}
	}
	return 0, false
}

func promotionCampaignCostGateReport(
	corpus Corpus,
	results []ResultRecord,
	projection CostProjection,
	gate CampaignCostEffectivenessGate,
) CampaignCostGateReport {
	resultByCase := make(map[string]ResultRecord, len(results))
	for _, result := range results {
		resultByCase[result.CaseID] = result
	}
	correct := 0
	for _, record := range corpus.Records {
		if goldOutcomeSuccess(record, resultByCase[record.CaseID]) {
			correct++
		}
	}
	report := CampaignCostGateReport{
		Gate:                     gate,
		CostPer1000CasesMicroUSD: projection.ProjectedCostPerThousandUSD * 1_000_000,
		ProjectedMonthlyMicroUSD: projection.SelectionProjectedThirtyDayUSD * 1_000_000,
		CorrectSafeActions:       correct,
		EvidenceComplete:         projection.MeasuredCases > 0 && correct > 0,
	}
	if correct > 0 {
		report.CostPerCorrectSafeActionMicroUSD =
			float64(projection.MeasuredUsage.CostMicroUSD) / float64(correct)
	}
	report.CostPer1000CasesPassed = report.EvidenceComplete &&
		report.CostPer1000CasesMicroUSD <=
			float64(gate.MaxCostPer1000CasesMicroUSD)+1e-9
	report.ProjectedMonthlyPassed = report.EvidenceComplete &&
		report.ProjectedMonthlyMicroUSD <=
			float64(gate.MaxProjectedMonthlyMicroUSD)+1e-9
	report.CostPerCorrectSafeActionPassed = report.EvidenceComplete &&
		report.CostPerCorrectSafeActionMicroUSD <=
			float64(gate.MaxCostPerCorrectSafeActionMicroUSD)+1e-9
	report.AbsolutePassed = report.CostPer1000CasesPassed &&
		report.ProjectedMonthlyPassed &&
		report.CostPerCorrectSafeActionPassed
	report.Passed = report.AbsolutePassed
	return report
}

func applyPromotionCampaignControlGates(
	corpus Corpus,
	candidate *PromotionSystemReport,
	candidateResults []ResultRecord,
	controls []*PromotionSystemReport,
	controlResults [][]ResultRecord,
) {
	if candidate == nil {
		return
	}
	comparisons := []*ComparisonReport{
		candidate.Comparison,
		candidate.ProductionComparison,
	}
	for index, control := range controls {
		harmUtility := CampaignHarmUtilityControlReport{}
		selective := CampaignSelectiveRiskControlReport{}
		cost := CampaignCostControlReport{}
		if control != nil {
			harmUtility.ControlSystemID = control.System.SystemID
			selective.ControlSystemID = control.System.SystemID
			cost.ControlSystemID = control.System.SystemID
		}
		if index < len(comparisons) && comparisons[index] != nil &&
			index < len(controlResults) && control != nil {
			harmUpper, harmComplete, utilityLower, utilityComplete :=
				promotionCampaignPairedBounds(
					corpus,
					controlResults[index],
					candidateResults,
					comparisons[index].Options,
					control.System.SystemID,
					candidate.System.SystemID,
				)
			harmUtility.EvidenceComplete = harmComplete && utilityComplete
			harmUtility.HarmUCBDelta = harmUpper
			harmUtility.UtilityLCBDelta = utilityLower
			harmUtility.HarmPassed = harmComplete && harmUpper <=
				candidate.HarmUtility.Gate.MaxHarmUCBDeltaVsControl+1e-12
			harmUtility.UtilityPassed = utilityComplete && utilityLower+1e-12 >=
				candidate.HarmUtility.Gate.MinUtilityLCBDeltaVsControl
			harmUtility.Passed = harmUtility.EvidenceComplete &&
				harmUtility.HarmPassed && harmUtility.UtilityPassed
		}
		candidate.HarmUtility.ControlComparisons = append(
			candidate.HarmUtility.ControlComparisons,
			harmUtility,
		)
		candidate.HarmUtility.EvidenceComplete =
			candidate.HarmUtility.EvidenceComplete && harmUtility.EvidenceComplete
		candidate.HarmUtility.Passed = candidate.HarmUtility.Passed && harmUtility.Passed

		if candidate.SelectiveRisk.Applicable {
			if control != nil && candidate.SelectiveRisk.EvidenceComplete &&
				control.SelectiveRisk.Applicable &&
				control.SelectiveRisk.EvidenceComplete {
				selective.ControlCoverage = control.SelectiveRisk.Coverage
				selective.RequiredCoverage = selective.ControlCoverage *
					candidate.SelectiveRisk.Gate.MinCoverageRatioVsIncumbent
				selective.CoveragePassed = candidate.SelectiveRisk.Coverage+1e-12 >=
					selective.RequiredCoverage
				if risk, ok := selectiveRiskAtCoverage(
					candidate.SelectiveRisk,
					selective.ControlCoverage,
				); ok {
					selective.EvidenceComplete = true
					selective.CandidateRisk = risk
					selective.RiskPassed = risk <=
						candidate.SelectiveRisk.Gate.MaxSelectiveRisk+1e-12
				}
			}
			selective.Passed = selective.EvidenceComplete &&
				selective.CoveragePassed && selective.RiskPassed
			candidate.SelectiveRisk.ControlComparisons = append(
				candidate.SelectiveRisk.ControlComparisons,
				selective,
			)
			candidate.SelectiveRisk.EvidenceComplete =
				candidate.SelectiveRisk.EvidenceComplete && selective.EvidenceComplete
			candidate.SelectiveRisk.Passed =
				candidate.SelectiveRisk.Passed && selective.Passed
		}
		if control != nil && candidate.CostGate.EvidenceComplete &&
			control.CostGate.EvidenceComplete {
			controlCost := control.CostGate.CostPer1000CasesMicroUSD
			candidateCost := candidate.CostGate.CostPer1000CasesMicroUSD
			cost.EvidenceComplete = true
			switch {
			case controlCost > 0:
				cost.CostRatio = candidateCost / controlCost
				cost.Passed = cost.CostRatio <=
					candidate.CostGate.Gate.MaxCostRatioVsControl+1e-12
			case candidateCost == 0:
				cost.CostRatio = 1
				cost.Passed = 1 <= candidate.CostGate.Gate.MaxCostRatioVsControl+1e-12
			default:
				// A positive-cost candidate cannot clear a finite ratio
				// against a genuinely zero-cost control.
				cost.Passed = false
			}
		}
		candidate.CostGate.ControlComparisons = append(
			candidate.CostGate.ControlComparisons,
			cost,
		)
		candidate.CostGate.EvidenceComplete =
			candidate.CostGate.EvidenceComplete && cost.EvidenceComplete
		candidate.CostGate.Passed = candidate.CostGate.Passed && cost.Passed
	}
	refreshPromotionCampaignGateState(candidate)
}

func promotionCampaignPairedBounds(
	corpus Corpus,
	controlResults []ResultRecord,
	candidateResults []ResultRecord,
	options ComparisonOptions,
	controlSystemID string,
	candidateSystemID string,
) (float64, bool, float64, bool) {
	controlByCase := make(map[string]ResultRecord, len(controlResults))
	for _, result := range controlResults {
		controlByCase[result.CaseID] = result
	}
	candidateByCase := make(map[string]ResultRecord, len(candidateResults))
	for _, result := range candidateResults {
		candidateByCase[result.CaseID] = result
	}
	harm := make([]pairedObservation, 0, len(corpus.Records))
	utility := make([]pairedObservation, 0, len(corpus.Records))
	contentUtility := [2][]pairedObservation{}
	for _, record := range corpus.Records {
		control, controlOK := controlByCase[record.CaseID]
		candidate, candidateOK := candidateByCase[record.CaseID]
		if !controlOK || !candidateOK {
			return 0, false, 0, false
		}
		controlIndicators := indicatorsForCase(record, control)
		candidateIndicators := indicatorsForCase(record, candidate)
		if controlIndicators.harmEligible != candidateIndicators.harmEligible {
			return 0, false, 0, false
		}
		if controlIndicators.harmEligible {
			harm = append(harm, pairedObservation{
				groupID:   record.GroupID,
				control:   controlIndicators.harm,
				candidate: candidateIndicators.harm,
			})
		}
		controlEligible, controlSuccess, stratum :=
			promotionCampaignUtilityOutcome(record, control)
		candidateEligible, candidateSuccess, candidateStratum :=
			promotionCampaignUtilityOutcome(record, candidate)
		if controlEligible != candidateEligible || stratum != candidateStratum {
			return 0, false, 0, false
		}
		if !controlEligible {
			continue
		}
		observation := pairedObservation{
			groupID:   record.GroupID,
			control:   controlSuccess,
			candidate: candidateSuccess,
		}
		if record.Task == TaskContentFilter {
			contentUtility[stratum] = append(contentUtility[stratum], observation)
		} else {
			utility = append(utility, observation)
		}
	}
	if len(harm) == 0 {
		return 0, false, 0, false
	}
	domain := string(corpus.Records[0].Task) + ":" + controlSystemID +
		":" + candidateSystemID + ":campaign"
	_, harmUpper := clusterBootstrapBounds(
		harm,
		options.BootstrapReplicates,
		deriveBootstrapSeed(options.BootstrapSeed, domain+":harm"),
	)
	if corpus.Records[0].Task == TaskContentFilter {
		if len(contentUtility[0]) == 0 || len(contentUtility[1]) == 0 {
			return harmUpper, true, 0, false
		}
		utilityLower, _ := promotionStratifiedClusterBootstrapBounds(
			contentUtility[:],
			options.BootstrapReplicates,
			deriveBootstrapSeed(options.BootstrapSeed, domain+":balanced-utility"),
		)
		return harmUpper, true, utilityLower, true
	}
	if len(utility) == 0 {
		return harmUpper, true, 0, false
	}
	utilityLower, _ := clusterBootstrapBounds(
		utility,
		options.BootstrapReplicates,
		deriveBootstrapSeed(options.BootstrapSeed, domain+":utility"),
	)
	return harmUpper, true, utilityLower, true
}

// promotionCampaignUtilityOutcome implements the task-specific utility named
// by CampaignEffectivenessGate. The returned stratum is used only for the two
// equally weighted content-filter language classes.
func promotionCampaignUtilityOutcome(
	record CorpusRecord,
	result ResultRecord,
) (bool, bool, int) {
	switch record.Task {
	case TaskMatcherExtract:
		required := len(record.MatcherExtract.Expected.Acceptable) > 0 &&
			!record.MatcherExtract.Expected.AllowAbstain
		if !required {
			return false, false, 0
		}
		if result.Status != ResultStatusOK || result.MatcherExtract == nil ||
			result.MatcherExtract.Action != MatcherExtractActionExtract ||
			result.MatcherExtract.Extraction == nil {
			return true, false, 0
		}
		for _, acceptable := range record.MatcherExtract.Expected.Acceptable {
			if *result.MatcherExtract.Extraction == acceptable {
				return true, true, 0
			}
		}
		return true, false, 0
	case TaskMatcherRerank:
		required := len(record.MatcherRerank.Expected.AcceptableTMDBIDs) > 0 &&
			!record.MatcherRerank.Expected.AllowAbstain
		if !required {
			return false, false, 0
		}
		if result.Status != ResultStatusOK || result.MatcherRerank == nil ||
			result.MatcherRerank.Action != MatcherRerankActionAttach {
			return true, false, 0
		}
		for _, acceptable := range record.MatcherRerank.Expected.AcceptableTMDBIDs {
			if result.MatcherRerank.TMDBID == acceptable {
				return true, true, 0
			}
		}
		return true, false, 0
	case TaskContentFilter:
		if record.ContentFilter.Expected.AllowAbstain {
			return false, false, 0
		}
		stratum := 1
		if record.ContentFilter.Expected.Language == LanguageEnglish {
			stratum = 0
		}
		return true, goldOutcomeSuccess(record, result), stratum
	case TaskJunkPurge:
		if record.JunkPurge.Expected.Disposition != JunkDispositionDelete {
			return false, false, 0
		}
		return true, goldOutcomeSuccess(record, result), 0
	default:
		return false, false, 0
	}
}

func promotionStratifiedClusterBootstrapBounds(
	strata [][]pairedObservation,
	replicates int,
	seed uint64,
) (float64, float64) {
	groupsByStratum := make([][]clusterAggregate, 0, len(strata))
	allDifferencesZero := true
	for _, observations := range strata {
		byGroup := make(map[string]clusterAggregate)
		for _, observation := range observations {
			aggregate := byGroup[observation.groupID]
			aggregate.eligible++
			if observation.control {
				aggregate.control++
			}
			if observation.candidate {
				aggregate.candidate++
			}
			byGroup[observation.groupID] = aggregate
		}
		groupIDs := make([]string, 0, len(byGroup))
		for groupID := range byGroup {
			groupIDs = append(groupIDs, groupID)
		}
		sort.Strings(groupIDs)
		groups := make([]clusterAggregate, 0, len(groupIDs))
		for _, groupID := range groupIDs {
			aggregate := byGroup[groupID]
			groups = append(groups, aggregate)
			if aggregate.candidate != aggregate.control {
				allDifferencesZero = false
			}
		}
		groupsByStratum = append(groupsByStratum, groups)
	}
	if allDifferencesZero {
		return 0, 0
	}
	rng := splitMix64{state: seed}
	differences := make([]float64, replicates)
	for replicate := 0; replicate < replicates; replicate++ {
		for _, groups := range groupsByStratum {
			eligible, control, candidate := 0, 0, 0
			for draw := 0; draw < len(groups); draw++ {
				aggregate := groups[rng.intn(len(groups))]
				eligible += aggregate.eligible
				control += aggregate.control
				candidate += aggregate.candidate
			}
			differences[replicate] +=
				(float64(candidate) - float64(control)) /
					float64(eligible) / float64(len(groupsByStratum))
		}
	}
	sort.Float64s(differences)
	alpha := 1 - CampaignConfidenceLevel
	return percentile(differences, alpha),
		percentile(differences, CampaignConfidenceLevel)
}

func refreshPromotionCampaignGateState(report *PromotionSystemReport) {
	if report == nil {
		return
	}
	criticalComplete := len(report.CriticalStrata) > 0
	criticalPassed := len(report.CriticalStrata) > 0
	for _, stratum := range report.CriticalStrata {
		criticalComplete = criticalComplete && stratum.EvidenceComplete
		criticalPassed = criticalPassed && stratum.Passed
	}
	calibrationComplete := !report.Calibration.Applicable ||
		report.Calibration.EvidenceComplete
	calibrationPassed := !report.Calibration.Applicable ||
		report.Calibration.Passed
	selectiveRiskComplete := !report.SelectiveRisk.Applicable ||
		report.SelectiveRisk.EvidenceComplete
	selectiveRiskPassed := !report.SelectiveRisk.Applicable ||
		report.SelectiveRisk.Passed
	campaignEvidenceComplete := report.HarmUtility.EvidenceComplete &&
		report.FirstPassSchema.EvidenceComplete &&
		report.Latency.Complete &&
		report.Instability.EvidenceComplete &&
		calibrationComplete && selectiveRiskComplete &&
		criticalComplete && report.CostGate.EvidenceComplete
	report.CampaignGatesPassed = campaignEvidenceComplete &&
		report.HarmUtility.Passed && report.FirstPassSchema.Passed &&
		report.Latency.Passed && report.Instability.Passed &&
		calibrationPassed && selectiveRiskPassed &&
		criticalPassed && report.CostGate.Passed
	if !campaignEvidenceComplete {
		report.EvidenceComplete = false
		report.ReasonCodes = append(report.ReasonCodes, "campaign_effectiveness_evidence_incomplete")
	}
	if !report.HarmUtility.Passed {
		report.ReasonCodes = append(report.ReasonCodes, "campaign_harm_utility_gate_not_passed")
	}
	if !report.FirstPassSchema.Passed {
		report.ReasonCodes = append(report.ReasonCodes, "campaign_schema_reliability_gate_not_passed")
	}
	if report.Calibration.Applicable && !report.Calibration.EvidenceComplete {
		report.ReasonCodes = append(report.ReasonCodes, "campaign_calibration_evidence_unavailable")
	} else if report.Calibration.Applicable && !report.Calibration.Passed {
		report.ReasonCodes = append(report.ReasonCodes, "campaign_calibration_gate_not_passed")
	}
	if !report.Instability.Passed {
		report.ReasonCodes = append(report.ReasonCodes, "campaign_stability_gate_not_passed")
	}
	if report.SelectiveRisk.Applicable && !report.SelectiveRisk.EvidenceComplete {
		report.ReasonCodes = append(report.ReasonCodes, "campaign_selective_risk_evidence_unavailable")
	} else if report.SelectiveRisk.Applicable && !report.SelectiveRisk.Passed {
		report.ReasonCodes = append(report.ReasonCodes, "campaign_selective_risk_gate_not_passed")
	}
	if !criticalComplete {
		report.ReasonCodes = append(report.ReasonCodes, "campaign_critical_strata_evidence_incomplete")
	} else if !criticalPassed {
		report.ReasonCodes = append(report.ReasonCodes, "campaign_critical_strata_gate_not_passed")
	}
	if !report.CostGate.EvidenceComplete {
		report.ReasonCodes = append(report.ReasonCodes, "campaign_cost_evidence_incomplete")
	} else if !report.CostGate.Passed {
		report.ReasonCodes = append(report.ReasonCodes, "campaign_cost_gate_not_passed")
	}
}

func validateFinalistPromotionBinding(
	task Task,
	finalists FinalistRosterManifest,
	roster PromotionRoster,
	rosterSHA256 string,
	configs map[string]SystemConfig,
) ([]string, error) {
	var taskFinalists *FinalistTaskRoster
	for index := range finalists.Tasks {
		entry := &finalists.Tasks[index]
		for _, evidence := range entry.SystemEvidence {
			config, exists := configs[evidence.SystemID]
			if !exists {
				return nil, fmt.Errorf(
					"finalist roster task %q evidence names system %q absent from the exact system manifest",
					entry.Task,
					evidence.SystemID,
				)
			}
			if _, err := validateFinalistSystemManifestBinding(
				entry.Task,
				evidence,
				config,
			); err != nil {
				return nil, fmt.Errorf(
					"finalist roster evidence system %q: %w",
					evidence.SystemID,
					err,
				)
			}
		}
		for _, systemID := range entry.SystemIDs {
			config, exists := configs[systemID]
			if !exists {
				return nil, fmt.Errorf(
					"finalist roster task %q names system %q absent from the exact system manifest",
					entry.Task,
					systemID,
				)
			}
			if !config.SupportsTask(entry.Task) {
				return nil, fmt.Errorf(
					"finalist roster system %q does not support task %q",
					systemID,
					entry.Task,
				)
			}
		}
		if entry.Task == task {
			taskFinalists = entry
		}
	}
	if taskFinalists == nil {
		return nil, fmt.Errorf(
			"finalist roster has no entry for task %q",
			task,
		)
	}
	if taskFinalists.PromotionRosterSHA256 != rosterSHA256 {
		return nil, fmt.Errorf(
			"promotion roster does not match the exact task policy frozen before holdout review",
		)
	}
	if len(taskFinalists.SystemEvidence) != len(roster.Systems) {
		return nil, fmt.Errorf(
			"promotion roster systems do not exactly match the frozen Stage 1 evidence for task %q",
			task,
		)
	}
	rosterByID := make(map[string]PromotionRosterSystem, len(roster.Systems))
	for _, system := range roster.Systems {
		rosterByID[system.SystemID] = system
	}
	for _, evidence := range taskFinalists.SystemEvidence {
		system, exists := rosterByID[evidence.SystemID]
		if !exists || evidence.Role != system.Role ||
			evidence.ControlSystemID != system.ControlSystemID ||
			evidence.ProductionControlSystemID !=
				system.ProductionControlSystemID ||
			evidence.DecisionThresholds != system.DecisionThresholds ||
			evidence.DeploymentServiceTier !=
				system.DeploymentServiceTier {
			return nil, fmt.Errorf(
				"promotion roster system %q does not exactly match its frozen Stage 1 evidence",
				evidence.SystemID,
			)
		}
	}
	candidateIDs := make([]string, 0, len(roster.Systems))
	for _, system := range roster.Systems {
		if system.Role == PromotionRoleCandidate {
			candidateIDs = append(candidateIDs, system.SystemID)
		}
	}
	if !sameOrderedStrings(candidateIDs, taskFinalists.SystemIDs) {
		return nil, fmt.Errorf(
			"promotion roster candidates do not exactly match every frozen Stage 1 passer for task %q",
			task,
		)
	}
	return append([]string(nil), taskFinalists.SystemIDs...), nil
}

func validatePromotionServiceTier(
	config SystemConfig,
	serviceTier PromotionServiceTier,
) error {
	switch serviceTier {
	case PromotionServiceStandard:
		return nil
	case PromotionServiceOpenAIBatch, PromotionServiceOpenAIFlex:
		if config.Provider != "openai" {
			return fmt.Errorf(
				"service tier %q is valid only for direct OpenAI systems",
				serviceTier,
			)
		}
		return nil
	default:
		return fmt.Errorf(
			"unsupported deployment service tier %q",
			serviceTier,
		)
	}
}

func promotionPricedSystem(
	config SystemConfig,
	serviceTier PromotionServiceTier,
	basis PromotionCostBasis,
) (SystemConfig, error) {
	if err := validatePromotionServiceTier(config, serviceTier); err != nil {
		return SystemConfig{}, err
	}
	switch serviceTier {
	case PromotionServiceStandard:
		return config, nil
	case PromotionServiceOpenAIBatch:
		return openAIBatchPricedSystem(config), nil
	case PromotionServiceOpenAIFlex:
		multiplier := float64(basis.OpenAIFlexMultiplierPPM) / 1_000_000
		config.InputUSDPerMillion *= multiplier
		config.CachedInputUSDPerMillion *= multiplier
		config.CacheWriteUSDPerMillion *= multiplier
		config.OutputUSDPerMillion *= multiplier
		config.USDPerRequest *= multiplier
		return config, nil
	default:
		return SystemConfig{}, fmt.Errorf(
			"unsupported deployment service tier %q",
			serviceTier,
		)
	}
}

func sameOrderedStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (basis PromotionCostBasis) validate() (float64, error) {
	start, err := time.Parse(time.RFC3339Nano, basis.ObservedWindowStartUTC)
	if err != nil {
		return 0, fmt.Errorf("observed_window_start_utc must be RFC3339")
	}
	end, err := time.Parse(time.RFC3339Nano, basis.ObservedWindowEndUTC)
	if err != nil {
		return 0, fmt.Errorf("observed_window_end_utc must be RFC3339")
	}
	if !start.Equal(start.UTC()) || !end.Equal(end.UTC()) {
		return 0, fmt.Errorf("observed volume window must use UTC")
	}
	duration := end.Sub(start)
	if duration <= 0 || duration > 366*24*time.Hour {
		return 0, fmt.Errorf("observed volume window must be in (0, 366 days]")
	}
	if basis.ObservedEligibleRequests <= 0 {
		return 0, fmt.Errorf("observed_eligible_requests must be positive")
	}
	if err := validateSHA256(
		"volume_evidence_sha256",
		basis.VolumeEvidenceSHA256,
	); err != nil {
		return 0, err
	}
	if err := validateSHA256(
		"pricing_evidence_sha256",
		basis.PricingEvidenceSHA256,
	); err != nil {
		return 0, err
	}
	if basis.OpenAIBatchMultiplierPPM != OpenAIBatchPricingPPM {
		return 0, fmt.Errorf(
			"openai_batch_multiplier_ppm must be %d",
			OpenAIBatchPricingPPM,
		)
	}
	if basis.OpenAIFlexMultiplierPPM <= 0 ||
		basis.OpenAIFlexMultiplierPPM > 1_000_000 {
		return 0, fmt.Errorf(
			"openai_flex_multiplier_ppm must be in (0,1000000]",
		)
	}
	return duration.Hours() / 24, nil
}

type promotionSystemCampaignEvidence struct {
	primaryRun CampaignRunBinding
	repeatRuns []CampaignRunBinding
	gate       CampaignEffectivenessGate
}

func buildPromotionSystemReport(
	corpus Corpus,
	config SystemConfig,
	evidence PromotionSystemEvidence,
	repeatSampling PromotionRepeatSamplingPlan,
	manifestSHA256 string,
	evaluatorBuildSHA256 string,
	costBasis PromotionCostBasis,
	windowDays float64,
	campaignEvidence ...promotionSystemCampaignEvidence,
) (PromotionSystemReport, error) {
	if len(campaignEvidence) > 1 {
		return PromotionSystemReport{}, fmt.Errorf(
			"at most one campaign evidence binding may be supplied",
		)
	}
	var campaign *promotionSystemCampaignEvidence
	if len(campaignEvidence) == 1 {
		campaign = &campaignEvidence[0]
	}
	if err := validateSHA256(
		"results_artifact_sha256",
		evidence.ResultsArtifactSHA256,
	); err != nil {
		return PromotionSystemReport{}, err
	}
	if err := validateSHA256(
		"attempt_evidence_sha256",
		evidence.AttemptEvidenceSHA256,
	); err != nil {
		return PromotionSystemReport{}, err
	}
	canonicalResultsSHA, err := ResultsIdentity(evidence.Results)
	if err != nil {
		return PromotionSystemReport{}, fmt.Errorf("results: %w", err)
	}
	for _, result := range evidence.Results {
		if result.ExecutionAudit.ManifestSHA256 != manifestSHA256 {
			return PromotionSystemReport{}, fmt.Errorf(
				"case %q manifest binding differs from exact system manifest",
				result.CaseID,
			)
		}
		if campaign != nil && result.ExecutionAudit.Campaign == nil {
			return PromotionSystemReport{}, fmt.Errorf(
				"case %q is missing campaign run binding",
				result.CaseID,
			)
		}
		if campaign != nil && !reflect.DeepEqual(
			*result.ExecutionAudit.Campaign,
			campaign.primaryRun,
		) {
			return PromotionSystemReport{}, fmt.Errorf(
				"case %q campaign run binding differs from the exact holdout plan",
				result.CaseID,
			)
		}
		if campaign != nil && result.RequestTiming == nil {
			return PromotionSystemReport{}, fmt.Errorf(
				"case %q is missing primary request timing",
				result.CaseID,
			)
		}
		if campaign != nil {
			if err := result.RequestTiming.Validate(); err != nil {
				return PromotionSystemReport{}, fmt.Errorf(
					"case %q primary request timing: %w",
					result.CaseID,
					err,
				)
			}
		}
		if campaign != nil &&
			(result.RequestTiming.DeadlineMS !=
				campaign.gate.Latency.DeadlineMS ||
				result.RequestTiming.DeadlineMS != config.RequestTimeoutMS) {
			return PromotionSystemReport{}, fmt.Errorf(
				"case %q request timing deadline %d differs from campaign and manifest deadline %d",
				result.CaseID,
				result.RequestTiming.DeadlineMS,
				campaign.gate.Latency.DeadlineMS,
			)
		}
		allowance := result.ExecutionAudit.UncappedOutputTokenAllowance
		if isUncappedGenerativeSystem(config) {
			if allowance <= 0 || result.Usage.OutputTokens > allowance {
				return PromotionSystemReport{}, fmt.Errorf(
					"case %q has invalid uncapped output-token allowance evidence",
					result.CaseID,
				)
			}
		} else if allowance != 0 {
			return PromotionSystemReport{}, fmt.Errorf(
				"case %q carries uncapped output-token allowance evidence for a capped system",
				result.CaseID,
			)
		}
	}
	score, err := ScoreWithThresholds(
		corpus,
		evidence.Results,
		evaluatorBuildSHA256,
		evidence.DecisionThresholds.runtime(),
	)
	if err != nil {
		return PromotionSystemReport{}, fmt.Errorf("score: %w", err)
	}
	if score.System != config.Descriptor() {
		return PromotionSystemReport{}, fmt.Errorf(
			"results system descriptor does not match exact manifest system",
		)
	}
	resultByCase := make(map[string]ResultRecord, len(evidence.Results))
	for _, result := range evidence.Results {
		resultByCase[result.CaseID] = result
	}
	for _, record := range corpus.Records {
		if isUncappedGenerativeSystem(config) {
			prompt, err := BuildPrompt(record)
			if err != nil {
				return PromotionSystemReport{}, err
			}
			if resultByCase[record.CaseID].ExecutionAudit.
				UncappedOutputTokenAllowance < int64(prompt.MaxCompletionTokens) {
				return PromotionSystemReport{}, fmt.Errorf(
					"case %q uncapped output-token allowance is below the nominal prompt ceiling",
					record.CaseID,
				)
			}
		}
		expectedContract, err := RequestContractSHA256(
			record,
			config,
			evidence.DecisionThresholds.runtime(),
		)
		if err != nil {
			return PromotionSystemReport{}, fmt.Errorf(
				"case %q request contract: %w",
				record.CaseID,
				err,
			)
		}
		if resultByCase[record.CaseID].RequestContractSHA256 !=
			expectedContract {
			return PromotionSystemReport{}, fmt.Errorf(
				"case %q request contract does not match the frozen decision thresholds",
				record.CaseID,
			)
		}
	}
	scoreSHA, err := canonicalJSONIdentity(score)
	if err != nil {
		return PromotionSystemReport{}, err
	}
	report := PromotionSystemReport{
		Role:                      evidence.Role,
		ControlSystemID:           evidence.ControlSystemID,
		ProductionControlSystemID: evidence.ProductionControlSystemID,
		DecisionThresholds:        evidence.DecisionThresholds,
		Lane:                      config.EvaluationLane,
		System:                    config.Descriptor(),
		ResultsArtifactSHA256:     evidence.ResultsArtifactSHA256,
		CanonicalResultsSHA256:    canonicalResultsSHA,
		AttemptEvidenceSHA256:     evidence.AttemptEvidenceSHA256,
		RouteSnapshotSHA256:       evidence.RouteSnapshotSHA256,
		RouteSnapshotVerified:     evidence.RouteSnapshotVerified,
		RepeatArtifactSHA256: append(
			[]string(nil),
			evidence.RepeatArtifactSHA256...,
		),
		RepeatAttemptEvidenceSHA256: append(
			[]string(nil),
			evidence.RepeatAttemptEvidenceSHA256...,
		),
		ScoreReportSHA256: scoreSHA,
		Score:             score,
		CoreGatesPassed:   evidence.Role == PromotionRoleControl,
		HolmPassed:        evidence.Role == PromotionRoleControl,
		EvidenceComplete:  true,
	}
	if campaign != nil {
		report.CampaignRun = campaign.primaryRun
		report.RepeatCampaignRuns = append(
			[]CampaignRunBinding(nil),
			campaign.repeatRuns...,
		)
		report.Latency = promotionLatencyReport(
			score.Latency,
			len(evidence.Results),
			campaign.gate.Latency,
		)
		report.HarmUtility = promotionCampaignHarmUtilityReport(
			corpus,
			evidence.Results,
			campaign.gate.HarmUtility,
		)
		selectiveRiskGate := CampaignSelectiveRiskGate{}
		if campaign.gate.SelectiveRisk != nil {
			selectiveRiskGate = *campaign.gate.SelectiveRisk
		}
		report.SelectiveRisk = promotionCampaignSelectiveRisk(
			corpus,
			evidence.Results,
			selectiveRiskGate,
		)
		report.CriticalStrata = promotionCampaignCriticalStrata(
			corpus,
			evidence.Results,
			campaign.gate.CriticalStrata,
		)
	}
	if campaign != nil && !report.Latency.Complete {
		report.EvidenceComplete = false
		report.ReasonCodes = append(
			report.ReasonCodes,
			"primary_request_timing_incomplete",
		)
	}
	if campaign != nil && !report.Latency.Passed {
		report.ReasonCodes = append(
			report.ReasonCodes,
			"campaign_latency_gate_not_passed",
		)
	}

	if !hasNaturalAndSafetyReports(score) {
		report.EvidenceComplete = false
		report.ReasonCodes = append(
			report.ReasonCodes,
			"natural_and_safety_reports_required",
		)
	}
	routeComplete, err := verifyPromotionRouteEvidence(
		config,
		evidence,
		manifestSHA256,
	)
	if err != nil {
		return PromotionSystemReport{}, err
	}
	if !routeComplete {
		report.EvidenceComplete = false
		report.ReasonCodes = append(
			report.ReasonCodes,
			"route_snapshot_or_return_proof_incomplete",
		)
	}
	var schemaGates []CampaignSchemaReliabilityGate
	if campaign != nil {
		schemaGates = append(schemaGates, campaign.gate.SchemaReliability)
	}
	pricedConfig, err := promotionPricedSystem(
		config,
		evidence.DeploymentServiceTier,
		costBasis,
	)
	if err != nil {
		return PromotionSystemReport{}, err
	}
	firstPass, err := validatePromotionAttemptEvidence(
		corpus,
		pricedConfig,
		evidence.AttemptEvidence,
		canonicalResultsSHA,
		evidence.Results,
		schemaGates...,
	)
	if err != nil {
		return PromotionSystemReport{}, fmt.Errorf("attempt evidence: %w", err)
	}
	report.FirstPassSchema = firstPass
	if !firstPass.Passed {
		report.ReasonCodes = append(
			report.ReasonCodes,
			"first_pass_or_repair_schema_gate_not_passed",
		)
	}
	report.RequestShape = promotionRequestShape(evidence.AttemptEvidence)
	if !report.RequestShape.OneCaseVerified {
		report.EvidenceComplete = false
		report.ReasonCodes = append(
			report.ReasonCodes,
			"packed_or_unverifiable_request_shape",
		)
	}
	var calibrationGates []CampaignCalibrationGate
	if campaign != nil && campaign.gate.Calibration != nil {
		calibrationGates = append(calibrationGates, *campaign.gate.Calibration)
	}
	report.Calibration, err = promotionConfidenceCalibration(
		corpus,
		evidence.Results,
		calibrationGates...,
	)
	if err != nil {
		return PromotionSystemReport{}, err
	}
	if !report.Calibration.Passed {
		report.ReasonCodes = append(
			report.ReasonCodes,
			"confidence_calibration_not_passed",
		)
	}
	var instabilityCampaignEvidence []promotionInstabilityCampaignEvidence
	if campaign != nil {
		instabilityCampaignEvidence = append(
			instabilityCampaignEvidence,
			promotionInstabilityCampaignEvidence{
				repeatRuns: campaign.repeatRuns,
				gate:       campaign.gate,
				costConfig: pricedConfig,
			},
		)
	}
	report.Instability, err = promotionInstability(
		corpus,
		config,
		evidence.Results,
		evidence.AttemptEvidence.Attempts,
		evidence.RepeatResults,
		evidence.RepeatArtifactSHA256,
		evidence.RepeatAttemptEvidence,
		evidence.RepeatAttemptEvidenceSHA256,
		repeatSampling,
		evaluatorBuildSHA256,
		manifestSHA256,
		evidence.DecisionThresholds.runtime(),
		instabilityCampaignEvidence...,
	)
	if err != nil {
		return PromotionSystemReport{}, err
	}
	if report.Instability.RepeatsRequired &&
		report.Instability.ObservedRepeatRuns !=
			report.Instability.RequiredRepeatRuns {
		report.EvidenceComplete = false
	}
	if report.Instability.DeterministicControlsProven &&
		report.Instability.ObservedRepeatRuns != 0 {
		report.EvidenceComplete = false
		report.ReasonCodes = append(
			report.ReasonCodes,
			"repeats_supplied_for_deterministic_route",
		)
	}
	if !report.Instability.Passed {
		report.ReasonCodes = append(
			report.ReasonCodes,
			"instability_gate_not_passed",
		)
	}
	report.Cost, err = promotionCostProjection(
		config,
		evidence.AttemptEvidence,
		costBasis,
		windowDays,
		evidence.DeploymentServiceTier,
	)
	if err != nil {
		return PromotionSystemReport{}, err
	}
	if campaign != nil {
		report.CostGate = promotionCampaignCostGateReport(
			corpus,
			evidence.Results,
			report.Cost,
			campaign.gate.Cost,
		)
		refreshPromotionCampaignGateState(&report)
	}
	if evidence.Role == PromotionRoleCandidate &&
		!promotionLaneEligible(config.EvaluationLane) {
		report.ReasonCodes = append(
			report.ReasonCodes,
			"evaluation_lane_not_promotion_eligible",
		)
	}
	return report, nil
}

func verifyPromotionRouteEvidence(
	config SystemConfig,
	evidence PromotionSystemEvidence,
	manifestSHA256 string,
) (bool, error) {
	if len(evidence.Results) == 0 {
		return false, fmt.Errorf("results are empty")
	}
	concreteDirectModel := ""
	for _, result := range evidence.Results {
		route := result.ExecutionAudit.Route
		if !route.Verified {
			return false, nil
		}
		if result.ExecutionAudit.ManifestSHA256 != manifestSHA256 {
			return false, fmt.Errorf("manifest binding mismatch")
		}
		if config.Provider == "openrouter" {
			if !evidence.RouteSnapshotVerified ||
				evidence.RouteSnapshotSHA256 == "" ||
				route.SnapshotSHA256 != evidence.RouteSnapshotSHA256 {
				return false, nil
			}
			continue
		}
		if evidence.RouteSnapshotSHA256 != "" ||
			evidence.RouteSnapshotVerified ||
			route.SnapshotSHA256 != "" {
			return false, fmt.Errorf(
				"direct provider cannot carry an OpenRouter route snapshot",
			)
		}
		if route.Proof != RouteProofDirectResponseModel ||
			!directModelMatches(config.Model, route.ReturnedModel) {
			return false, nil
		}
		if concreteDirectModel == "" {
			concreteDirectModel = route.ReturnedModel
		} else if route.ReturnedModel != concreteDirectModel {
			return false, nil
		}
	}
	return true, nil
}

func validatePromotionAttemptEvidence(
	corpus Corpus,
	config SystemConfig,
	evidence PromotionAttemptEvidence,
	resultsSHA256 string,
	results []ResultRecord,
	campaignGates ...CampaignSchemaReliabilityGate,
) (FirstPassSchemaReport, error) {
	gate := CampaignSchemaReliabilityGate{
		MinFirstPassSchemaValidRate: PromotionFirstPassSchemaFloor,
		MinFinalSchemaValidRate:     1,
		MinAnsweredRate:             0,
		MaxSchemaRepairsPerCase:     1,
		MaxCallErrorUCB:             1,
	}
	if len(campaignGates) > 1 {
		return FirstPassSchemaReport{}, fmt.Errorf("at most one campaign schema gate may be supplied")
	}
	if len(campaignGates) == 1 {
		gate = campaignGates[0]
	}
	if evidence.SchemaVersion != SchemaVersion ||
		evidence.ProtocolVersion != PromotionBundleProtocolVersion {
		return FirstPassSchemaReport{}, fmt.Errorf(
			"unsupported schema_version or protocol_version",
		)
	}
	if evidence.SystemID != config.SystemID ||
		evidence.Task != corpus.Records[0].Task ||
		evidence.CorpusSHA256 != corpus.SHA256 ||
		evidence.ResultsSHA256 != resultsSHA256 {
		return FirstPassSchemaReport{}, fmt.Errorf(
			"attempt evidence binding does not match system/task/corpus/results",
		)
	}
	resultByCase := make(map[string]ResultRecord, len(results))
	for _, result := range results {
		if !result.Usage.AccountingComplete ||
			!usageAccountingCompleteForSystem(
				config,
				result.Usage.InputTokens,
				result.Usage.OutputTokens,
			) {
			return FirstPassSchemaReport{}, fmt.Errorf(
				"result %q has incomplete usage accounting",
				result.CaseID,
			)
		}
		if err := validatePromotionResultCostProvenance(
			config,
			result.Usage,
			fmt.Sprintf("result %q", result.CaseID),
		); err != nil {
			return FirstPassSchemaReport{}, err
		}
		resultByCase[result.CaseID] = result
	}
	attemptsByCase := make(map[string][]PromotionAttempt, len(results))
	for i, attempt := range evidence.Attempts {
		if _, exists := resultByCase[attempt.CaseID]; !exists {
			return FirstPassSchemaReport{}, fmt.Errorf(
				"attempts[%d] identifies unknown case %q",
				i,
				attempt.CaseID,
			)
		}
		if attempt.Sequence <= 0 {
			return FirstPassSchemaReport{}, fmt.Errorf(
				"attempts[%d].sequence must be positive",
				i,
			)
		}
		switch attempt.Kind {
		case PromotionAttemptInitial, PromotionAttemptSchemaRepair:
		default:
			return FirstPassSchemaReport{}, fmt.Errorf(
				"attempts[%d].kind is unsupported",
				i,
			)
		}
		switch attempt.Status {
		case ResultStatusOK, ResultStatusSchemaError, ResultStatusError:
		default:
			return FirstPassSchemaReport{}, fmt.Errorf(
				"attempts[%d].status is unsupported",
				i,
			)
		}
		if err := attempt.Usage.Validate(); err != nil {
			return FirstPassSchemaReport{}, fmt.Errorf(
				"attempts[%d].usage: %w",
				i,
				err,
			)
		}
		if !attempt.Usage.AccountingComplete ||
			!usageAccountingCompleteForSystem(
				config,
				attempt.Usage.InputTokens,
				attempt.Usage.OutputTokens,
			) {
			return FirstPassSchemaReport{}, fmt.Errorf(
				"attempts[%d].usage accounting is incomplete",
				i,
			)
		}
		if err := validatePromotionUsageCost(
			config,
			attempt.Usage,
			fmt.Sprintf("attempts[%d]", i),
		); err != nil {
			return FirstPassSchemaReport{}, err
		}
		attemptsByCase[attempt.CaseID] = append(
			attemptsByCase[attempt.CaseID],
			attempt,
		)
	}
	report := FirstPassSchemaReport{
		Gate:                     gate,
		Cases:                    len(results),
		FirstPassFloor:           gate.MinFirstPassSchemaValidRate,
		UsageFullyAccounted:      true,
		AtMostOneRepair:          true,
		NoRepairWhenFirstPass100: true,
	}
	for _, record := range corpus.Records {
		result := resultByCase[record.CaseID]
		attempts := attemptsByCase[record.CaseID]
		sort.Slice(attempts, func(i, j int) bool {
			return attempts[i].Sequence < attempts[j].Sequence
		})
		if len(attempts) == 0 || len(attempts) > 2 {
			return FirstPassSchemaReport{}, fmt.Errorf(
				"case %q must have one initial attempt and at most one repair",
				record.CaseID,
			)
		}
		for index, attempt := range attempts {
			if attempt.Sequence != index+1 {
				return FirstPassSchemaReport{}, fmt.Errorf(
					"case %q attempt sequence is not contiguous",
					record.CaseID,
				)
			}
		}
		if attempts[0].Kind != PromotionAttemptInitial {
			return FirstPassSchemaReport{}, fmt.Errorf(
				"case %q first attempt must be initial",
				record.CaseID,
			)
		}
		switch attempts[0].Status {
		case ResultStatusOK:
			report.FirstPassValid++
			if len(attempts) != 1 {
				return FirstPassSchemaReport{}, fmt.Errorf(
					"case %q cannot repair a schema-valid first pass",
					record.CaseID,
				)
			}
		case ResultStatusSchemaError:
			if len(attempts) != 2 ||
				attempts[1].Kind != PromotionAttemptSchemaRepair {
				return FirstPassSchemaReport{}, fmt.Errorf(
					"case %q schema error requires exactly one accounted repair",
					record.CaseID,
				)
			}
			report.SchemaRepairCases++
		case ResultStatusError:
			if len(attempts) != 1 {
				return FirstPassSchemaReport{}, fmt.Errorf(
					"case %q runtime failure cannot enter the schema-repair path",
					record.CaseID,
				)
			}
		}
		finalAttempt := attempts[len(attempts)-1]
		if finalAttempt.Status == ResultStatusError {
			report.RuntimeFailureCases++
		}
		if finalAttempt.Status == ResultStatusOK {
			report.FinalValid++
			report.Answered++
		}
		repairs := len(attempts) - 1
		if repairs > report.MaximumRepairsObserved {
			report.MaximumRepairsObserved = repairs
		}
		if result.Status != finalAttempt.Status {
			return FirstPassSchemaReport{}, fmt.Errorf(
				"case %q final result status differs from final attempt",
				record.CaseID,
			)
		}
		summed := Usage{AccountingComplete: true}
		for _, attempt := range attempts {
			var err error
			summed, err = sumPromotionUsage(summed, attempt.Usage)
			if err != nil {
				return FirstPassSchemaReport{}, fmt.Errorf(
					"case %q attempt usage: %w",
					record.CaseID,
					err,
				)
			}
			if err := addPromotionUsageTotals(&report.Usage, attempt.Usage); err != nil {
				return FirstPassSchemaReport{}, err
			}
		}
		if !usageNumbersEqual(summed, result.Usage) {
			return FirstPassSchemaReport{}, fmt.Errorf(
				"case %q result usage does not equal all charged attempts",
				record.CaseID,
			)
		}
		if len(attempts) == 1 &&
			result.Usage.RequestID != attempts[0].Usage.RequestID {
			return FirstPassSchemaReport{}, fmt.Errorf(
				"case %q request identity differs from its only attempt",
				record.CaseID,
			)
		}
		if len(attempts) == 2 && result.Usage.RequestID != "" {
			return FirstPassSchemaReport{}, fmt.Errorf(
				"case %q repaired aggregate result must not claim one request_id",
				record.CaseID,
			)
		}
	}
	if len(attemptsByCase) != len(results) {
		return FirstPassSchemaReport{}, fmt.Errorf(
			"attempt evidence does not cover exactly every result case",
		)
	}
	report.FirstPassValidity = float64(report.FirstPassValid) /
		float64(report.Cases)
	report.FinalValidity = float64(report.FinalValid) / float64(report.Cases)
	report.AnsweredRate = float64(report.Answered) / float64(report.Cases)
	report.CallErrorUpperConfidenceBound = promotionWilsonUpperBound(
		report.RuntimeFailureCases,
		report.Cases,
	)
	if report.FirstPassValid == report.Cases &&
		report.SchemaRepairCases != 0 {
		report.NoRepairWhenFirstPass100 = false
	}
	report.Usage.Results = len(results)
	report.Usage.CostUSD = float64(report.Usage.CostMicroUSD) / 1_000_000
	report.Usage.TotalTokens = report.Usage.InputTokens +
		report.Usage.OutputTokens
	report.EvidenceComplete = report.Cases > 0 &&
		len(attemptsByCase) == len(results) && report.UsageFullyAccounted
	report.Passed = report.EvidenceComplete &&
		report.FirstPassValidity+1e-12 >= gate.MinFirstPassSchemaValidRate &&
		report.FinalValidity+1e-12 >= gate.MinFinalSchemaValidRate &&
		report.AnsweredRate+1e-12 >= gate.MinAnsweredRate &&
		report.MaximumRepairsObserved <= gate.MaxSchemaRepairsPerCase &&
		report.CallErrorUpperConfidenceBound <= gate.MaxCallErrorUCB+1e-12 &&
		report.UsageFullyAccounted &&
		report.AtMostOneRepair &&
		report.NoRepairWhenFirstPass100
	return report, nil
}

func validatePromotionResultCostProvenance(
	config SystemConfig,
	usage Usage,
	label string,
) error {
	expectedSource := CostSourceManifestEstimate
	if config.Provider == "openrouter" {
		// OpenRouter generative promotion lanes expose authenticated generation
		// cost. Requiring that provenance keeps a locally fabricated manifest
		// estimate from being relabeled as provider-observed evidence. Direct
		// OpenAI responses do not expose a charge and are therefore priced from
		// the immutable manifest.
		expectedSource = CostSourceProviderReported
	}
	if usage.CostSource != expectedSource {
		return fmt.Errorf(
			"%s cost_source %q is incompatible with provider %q; want %q",
			label,
			usage.CostSource,
			config.Provider,
			expectedSource,
		)
	}
	if config.isZeroPriced() {
		if usage.CostMicroUSD != 0 {
			return fmt.Errorf(
				"%s has nonzero usage cost for a zero-priced manifest route",
				label,
			)
		}
		return nil
	}
	if usage.CostMicroUSD <= 0 {
		return fmt.Errorf(
			"%s on a paid manifest route has zero usage cost",
			label,
		)
	}
	return nil
}

func validatePromotionUsageCost(
	config SystemConfig,
	usage Usage,
	label string,
) error {
	if err := validatePromotionResultCostProvenance(
		config,
		usage,
		label,
	); err != nil {
		return err
	}
	expected := config.EstimateCostWithCacheWriteMicroUSD(
		usage.InputTokens,
		usage.CachedInputTokens,
		usage.CacheWriteTokens,
		usage.OutputTokens,
	)
	if expected == math.MaxInt64 {
		return fmt.Errorf("%s manifest cost estimate overflowed", label)
	}
	if usage.CostMicroUSD != expected {
		return fmt.Errorf(
			"%s cost_micro_usd %d differs from exact manifest estimate %d",
			label,
			usage.CostMicroUSD,
			expected,
		)
	}
	return nil
}

// validatePromotionAggregateCost constrains pre-holdout Stage 1 cost evidence
// to the price implied by the exact manifest. Stage 1 freezes aggregate score
// evidence rather than individual request rows, so independent per-request
// micro-dollar rounding can move the aggregate by at most one micro-dollar per
// request. Final promotion cost gates do not use this bounded value: they use
// exact per-attempt recomputation in promotionCostProjection.
func validatePromotionAggregateCost(
	config SystemConfig,
	usage UsageTotals,
	serviceTier PromotionServiceTier,
	label string,
) error {
	expected, err := promotionAggregateManifestCost(
		config,
		usage,
		serviceTier,
		label,
	)
	if err != nil {
		return err
	}
	if !config.isZeroPriced() && usage.CostMicroUSD == 0 {
		return fmt.Errorf(
			"%s on a paid manifest route has zero usage cost",
			label,
		)
	}
	if config.isZeroPriced() && usage.CostMicroUSD != 0 {
		return fmt.Errorf(
			"%s has nonzero usage cost for a zero-priced manifest route",
			label,
		)
	}
	tolerance := int64(usage.Requests)
	lower := expected - tolerance
	if lower < 0 {
		lower = 0
	}
	upper, err := checkedAddInt64(expected, tolerance)
	if err != nil {
		upper = math.MaxInt64
	}
	if usage.CostMicroUSD < lower || usage.CostMicroUSD > upper {
		return fmt.Errorf(
			"%s aggregate cost_micro_usd %d is outside manifest-derived rounding bounds %d..%d",
			label,
			usage.CostMicroUSD,
			lower,
			upper,
		)
	}
	return nil
}

func promotionAggregateManifestCost(
	config SystemConfig,
	usage UsageTotals,
	serviceTier PromotionServiceTier,
	label string,
) (int64, error) {
	if usage.Requests <= 0 {
		return 0, fmt.Errorf("%s has no priced requests", label)
	}
	aggregateConfig := config
	switch serviceTier {
	case PromotionServiceStandard:
	case PromotionServiceOpenAIBatch:
		aggregateConfig = openAIBatchPricedSystem(config)
	case PromotionServiceOpenAIFlex:
		// Flex's exact multiplier is frozen later with pricing evidence. Stage 1
		// therefore cannot independently rank a Flex run by aggregate spend.
		return 0, fmt.Errorf(
			"%s cannot prove aggregate manifest cost for openai_flex before pricing evidence is bound",
			label,
		)
	default:
		return 0, fmt.Errorf("%s has unsupported service tier %q", label, serviceTier)
	}
	aggregateConfig.USDPerRequest *= float64(usage.Requests)
	expected := aggregateConfig.EstimateCostWithCacheWriteMicroUSD(
		usage.InputTokens,
		usage.CachedInputTokens,
		usage.CacheWriteTokens,
		usage.OutputTokens,
	)
	if expected == math.MaxInt64 {
		return 0, fmt.Errorf("%s aggregate manifest cost estimate overflowed", label)
	}
	return expected, nil
}

func promotionRequestShape(
	evidence PromotionAttemptEvidence,
) RequestShapeReport {
	report := RequestShapeReport{
		Variant:              "one_case_per_provider_request",
		PackedVariantSupport: "not_validated; packed request IDs are rejected",
	}
	requestCases := make(map[string]string)
	seenRequestIDs := make(map[string]struct{})
	for _, attempt := range evidence.Attempts {
		requestID := strings.TrimSpace(attempt.Usage.RequestID)
		if requestID == "" {
			report.MissingRequestIDs++
			continue
		}
		if _, duplicate := seenRequestIDs[requestID]; duplicate {
			report.DuplicateRequestIDs++
		}
		seenRequestIDs[requestID] = struct{}{}
		if prior, exists := requestCases[requestID]; exists &&
			prior != attempt.CaseID {
			report.PackedRequests++
		} else {
			requestCases[requestID] = attempt.CaseID
		}
	}
	report.UniqueRequests = len(requestCases)
	report.OneCaseVerified = report.PackedRequests == 0 &&
		report.DuplicateRequestIDs == 0 &&
		report.MissingRequestIDs == 0
	return report
}

func promotionConfidenceCalibration(
	corpus Corpus,
	results []ResultRecord,
	campaignGates ...CampaignCalibrationGate,
) (ConfidenceCalibrationReport, error) {
	task := corpus.Records[0].Task
	gate := CampaignCalibrationGate{}
	if task != TaskMatcherExtract {
		gate = CampaignCalibrationGate{
			Metric:        CampaignCalibrationMetricECE,
			MaxError:      PromotionCalibrationECECeiling,
			MaxBrierScore: PromotionCalibrationBrierCeiling,
		}
	}
	if len(campaignGates) > 1 {
		return ConfidenceCalibrationReport{}, fmt.Errorf("at most one campaign calibration gate may be supplied")
	}
	campaignRequired := len(campaignGates) == 1
	if campaignRequired {
		gate = campaignGates[0]
	}
	report := ConfidenceCalibrationReport{
		Gate:          gate,
		Applicable:    task != TaskMatcherExtract,
		Definition:    "fixed 10-bin ECE and Brier score over decisive outputs on unambiguous reviewed-gold cases; natural and safety suites remain separate",
		ECECeiling:    gate.MaxError,
		BrierCeiling:  gate.MaxBrierScore,
		MinimumGroups: PromotionCalibrationMinimumGroups,
		Passed:        true,
	}
	if !report.Applicable {
		report.NotApplicableReason =
			"matcher_extract result contract has no confidence field"
		report.EvidenceComplete = true
		report.Passed = true
		return report, nil
	}
	report.EvidenceComplete = true
	resultByCase := make(map[string]ResultRecord, len(results))
	for _, result := range results {
		resultByCase[result.CaseID] = result
	}
	type observation struct {
		groupID    string
		confidence float64
		correct    bool
	}
	bySuite := make(map[string][]observation)
	excluded := make(map[string]int)
	for _, record := range corpus.Records {
		suite, err := scoringPrimarySuite(record)
		if err != nil {
			return ConfidenceCalibrationReport{}, err
		}
		result := resultByCase[record.CaseID]
		confidence, eligible := promotionConfidence(record, result)
		if !eligible {
			excluded[suite]++
			continue
		}
		bySuite[suite] = append(bySuite[suite], observation{
			groupID:    record.GroupID,
			confidence: confidence,
			correct:    goldOutcomeSuccess(record, result),
		})
	}
	suites := make([]string, 0, len(bySuite)+len(excluded))
	seenSuite := make(map[string]struct{})
	for suite := range bySuite {
		suites = append(suites, suite)
		seenSuite[suite] = struct{}{}
	}
	for suite := range excluded {
		if _, exists := seenSuite[suite]; !exists {
			suites = append(suites, suite)
		}
	}
	sort.Strings(suites)
	for _, suite := range suites {
		observations := bySuite[suite]
		slice := ConfidenceCalibrationSlice{
			Suite:         suite,
			EligibleCases: len(observations),
			ExcludedCases: excluded[suite],
			Bins:          make([]CalibrationBin, 10),
		}
		groups := make(map[string]struct{})
		var brier float64
		for index := range slice.Bins {
			slice.Bins[index].LowerInclusive = float64(index) / 10
			slice.Bins[index].UpperInclusive = float64(index+1) / 10
		}
		for _, observation := range observations {
			groups[observation.groupID] = struct{}{}
			bin := int(math.Floor(observation.confidence * 10))
			if bin == 10 {
				bin = 9
			}
			slice.Bins[bin].Samples++
			slice.Bins[bin].MeanConfidence += observation.confidence
			if observation.correct {
				slice.Bins[bin].Correct++
			}
			target := 0.0
			if observation.correct {
				target = 1
			}
			difference := observation.confidence - target
			brier += difference * difference
		}
		slice.UniqueGroups = len(groups)
		if len(observations) > 0 {
			slice.Brier = brier / float64(len(observations))
			for index := range slice.Bins {
				bin := &slice.Bins[index]
				if bin.Samples == 0 {
					continue
				}
				bin.MeanConfidence /= float64(bin.Samples)
				bin.ObservedAccuracy = float64(bin.Correct) /
					float64(bin.Samples)
				slice.ECE += float64(bin.Samples) /
					float64(len(observations)) *
					math.Abs(bin.MeanConfidence-bin.ObservedAccuracy)
			}
		}
		slice.MeetsMinimum = slice.UniqueGroups >=
			PromotionCalibrationMinimumGroups
		report.EvidenceComplete = report.EvidenceComplete &&
			slice.MeetsMinimum
		slice.Passed = slice.MeetsMinimum &&
			slice.ECE <= gate.MaxError+1e-12 &&
			slice.Brier <= gate.MaxBrierScore+1e-12
		report.Passed = report.Passed && slice.Passed
		report.Suites = append(report.Suites, slice)
	}
	if len(report.Suites) != 2 {
		report.EvidenceComplete = false
		report.Passed = false
	}
	return report, nil
}

func promotionConfidence(
	record CorpusRecord,
	result ResultRecord,
) (float64, bool) {
	if result.Status != ResultStatusOK {
		return 0, false
	}
	switch record.Task {
	case TaskMatcherRerank:
		if record.MatcherRerank.Expected.AllowAbstain ||
			result.MatcherRerank.Action == MatcherRerankActionAbstain {
			return 0, false
		}
		return result.MatcherRerank.Confidence, true
	case TaskContentFilter:
		if record.ContentFilter.Expected.AllowAbstain ||
			result.ContentFilter.Action == ContentFilterActionAbstain {
			return 0, false
		}
		return result.ContentFilter.Confidence, true
	case TaskJunkPurge:
		if record.JunkPurge.Expected.Disposition == JunkDispositionAbstain ||
			result.JunkPurge.Action == JunkPurgeActionAbstain {
			return 0, false
		}
		return result.JunkPurge.Confidence, true
	default:
		return 0, false
	}
}

func reservePromotionAttemptRequestIDs(
	seen map[string]struct{},
	label string,
	attempts []PromotionAttempt,
) error {
	for index, attempt := range attempts {
		requestID := strings.TrimSpace(attempt.Usage.RequestID)
		if requestID == "" {
			return fmt.Errorf(
				"%s attempt %d has no independent request identity",
				label,
				index,
			)
		}
		if _, duplicate := seen[requestID]; duplicate {
			return fmt.Errorf(
				"%s request identity %q was reused",
				label,
				requestID,
			)
		}
		seen[requestID] = struct{}{}
	}
	return nil
}

type promotionInstabilityCampaignEvidence struct {
	repeatRuns []CampaignRunBinding
	gate       CampaignEffectivenessGate
	costConfig SystemConfig
}

func promotionInstability(
	corpus Corpus,
	config SystemConfig,
	primary []ResultRecord,
	primaryAttempts []PromotionAttempt,
	repeats [][]ResultRecord,
	repeatHashes []string,
	repeatAttempts []PromotionAttemptEvidence,
	repeatAttemptHashes []string,
	sampling PromotionRepeatSamplingPlan,
	evaluatorBuildSHA256 string,
	manifestSHA256 string,
	decisionThresholds DecisionThresholds,
	campaignEvidence ...promotionInstabilityCampaignEvidence,
) (InstabilityReport, error) {
	var repeatCampaignRuns []CampaignRunBinding
	costConfig := config
	campaignRequired := len(campaignEvidence) == 1
	gate := CampaignEffectivenessGate{
		Stability: CampaignStabilityGate{
			MinRepeatAgreement:    0,
			MaxUtilityDrift:       1,
			MaxHarmDrift:          1,
			MaxHarmfulRepeatFlips: int(^uint(0) >> 1),
			MaxFinalActionFlipUCB: sampling.InstabilityCeiling,
		},
		SchemaReliability: CampaignSchemaReliabilityGate{
			MinFirstPassSchemaValidRate: PromotionFirstPassSchemaFloor,
			MinFinalSchemaValidRate:     1,
			MaxSchemaRepairsPerCase:     1,
			MaxCallErrorUCB:             1,
		},
	}
	if len(campaignEvidence) > 1 {
		return InstabilityReport{}, fmt.Errorf(
			"at most one repeat campaign evidence set may be supplied",
		)
	}
	if campaignRequired {
		repeatCampaignRuns = campaignEvidence[0].repeatRuns
		gate = campaignEvidence[0].gate
		costConfig = campaignEvidence[0].costConfig
	}
	if err := sampling.validate(); err != nil {
		return InstabilityReport{}, err
	}
	sample, suiteTemplates, err := promotionRepeatSample(corpus, sampling)
	if err != nil {
		return InstabilityReport{}, err
	}
	deterministic := config.Temperature != nil &&
		math.Abs(*config.Temperature) <= 1e-12 &&
		config.Seed != nil
	report := InstabilityReport{
		Gate:                        gate.Stability,
		DeterministicControlsProven: deterministic,
		RepeatsRequired:             !deterministic,
		RequiredRepeatRuns:          PromotionRequiredRepeatRuns,
		ObservedRepeatRuns:          len(repeats),
		SamplingAlgorithmID:         sampling.AlgorithmID,
		SampleCorpusSHA256:          sample.SHA256,
		ConfidenceMethod:            sampling.ConfidenceMethod,
		ConfidenceLevel:             sampling.ConfidenceLevel,
		Cases:                       len(sample.Records),
		Groups: sampling.NaturalGroups +
			sampling.SafetyGroups,
		Ceiling: gate.Stability.MaxFinalActionFlipUCB,
		Suites:  suiteTemplates,
	}
	if len(repeats) != len(repeatHashes) ||
		len(repeats) != len(repeatAttempts) ||
		len(repeats) != len(repeatAttemptHashes) ||
		(campaignRequired &&
			len(repeats) != len(repeatCampaignRuns)) {
		return InstabilityReport{}, fmt.Errorf(
			"repeat results, hashes, attempt evidence, and campaign runs differ in count",
		)
	}
	for index, hash := range repeatHashes {
		if err := validateSHA256(
			fmt.Sprintf("repeat_artifact_sha256[%d]", index),
			hash,
		); err != nil {
			return InstabilityReport{}, err
		}
	}
	for index, hash := range repeatAttemptHashes {
		if err := validateSHA256(
			fmt.Sprintf("repeat_attempt_evidence_sha256[%d]", index),
			hash,
		); err != nil {
			return InstabilityReport{}, err
		}
	}
	if campaignRequired {
		for index, resultSet := range repeats {
			for _, result := range resultSet {
				if result.ExecutionAudit.Campaign == nil {
					return InstabilityReport{}, fmt.Errorf(
						"repeat %d case %q is missing campaign run binding",
						index+1,
						result.CaseID,
					)
				}
				if !reflect.DeepEqual(
					*result.ExecutionAudit.Campaign,
					repeatCampaignRuns[index],
				) {
					return InstabilityReport{}, fmt.Errorf(
						"repeat %d case %q campaign run binding differs from the exact repeat plan",
						index+1,
						result.CaseID,
					)
				}
			}
		}
	}
	seenRequestIDs := make(map[string]struct{}, len(primaryAttempts))
	if err := reservePromotionAttemptRequestIDs(
		seenRequestIDs,
		"primary",
		primaryAttempts,
	); err != nil {
		return InstabilityReport{}, err
	}
	if deterministic {
		report.EvidenceComplete = len(repeats) == 0
		report.RepeatAgreement = 1
		report.Passed = report.EvidenceComplete
		for index := range report.Suites {
			report.Suites[index].Passed = report.Passed
		}
		return report, nil
	}
	if len(repeats) != PromotionRequiredRepeatRuns {
		report.UpperConfidenceBound = 1
		for index := range report.Suites {
			report.Suites[index].UpperConfidenceBound = 1
			report.Suites[index].Passed = false
		}
		report.Passed = false
		report.EvidenceComplete = false
		return report, nil
	}
	primaryByCase := make(map[string]ResultRecord, len(primary))
	for _, result := range primary {
		primaryByCase[result.CaseID] = result
	}
	sampleRecordByCase := make(map[string]CorpusRecord, len(sample.Records))
	for _, record := range sample.Records {
		sampleRecordByCase[record.CaseID] = record
	}
	repeatMaps := make([]map[string]ResultRecord, 0, len(repeats))
	for index, resultSet := range repeats {
		if len(resultSet) != len(sample.Records) {
			return InstabilityReport{}, fmt.Errorf(
				"repeat %d does not cover every sampled case",
				index+1,
			)
		}
		canonicalRepeatSHA, err := ResultsIdentity(resultSet)
		if err != nil {
			return InstabilityReport{}, fmt.Errorf(
				"repeat %d results: %w",
				index+1,
				err,
			)
		}
		repeatFirstPass, err := validatePromotionAttemptEvidence(
			sample,
			costConfig,
			repeatAttempts[index],
			canonicalRepeatSHA,
			resultSet,
			gate.SchemaReliability,
		)
		if err != nil {
			return InstabilityReport{}, fmt.Errorf(
				"repeat %d attempt evidence: %w",
				index+1,
				err,
			)
		}
		if !repeatFirstPass.Passed {
			return InstabilityReport{}, fmt.Errorf(
				"repeat %d attempt evidence did not pass the schema/repair gate",
				index+1,
			)
		}
		if !promotionRequestShape(repeatAttempts[index]).OneCaseVerified {
			return InstabilityReport{}, fmt.Errorf(
				"repeat %d attempt evidence does not prove one case per provider request",
				index+1,
			)
		}
		if err := reservePromotionAttemptRequestIDs(
			seenRequestIDs,
			fmt.Sprintf("repeat %d", index+1),
			repeatAttempts[index].Attempts,
		); err != nil {
			return InstabilityReport{}, err
		}
		if _, err := ScoreWithThresholds(
			sample,
			resultSet,
			evaluatorBuildSHA256,
			decisionThresholds,
		); err != nil {
			return InstabilityReport{}, fmt.Errorf(
				"repeat %d: %w",
				index+1,
				err,
			)
		}
		byCase := make(map[string]ResultRecord, len(resultSet))
		for _, result := range resultSet {
			primaryResult, exists := primaryByCase[result.CaseID]
			if !exists {
				return InstabilityReport{}, fmt.Errorf(
					"repeat %d identifies case %q outside the primary run",
					index+1,
					result.CaseID,
				)
			}
			record, exists := sampleRecordByCase[result.CaseID]
			if !exists {
				return InstabilityReport{}, fmt.Errorf(
					"repeat %d identifies case %q outside the sampled corpus",
					index+1,
					result.CaseID,
				)
			}
			if result.System != config.Descriptor() {
				return InstabilityReport{}, fmt.Errorf(
					"repeat %d case %q system descriptor differs from the frozen config",
					index+1,
					result.CaseID,
				)
			}
			expectedContract, err := RequestContractSHA256(
				record,
				config,
				decisionThresholds,
			)
			if err != nil {
				return InstabilityReport{}, err
			}
			if result.RequestContractSHA256 != expectedContract {
				return InstabilityReport{}, fmt.Errorf(
					"repeat %d case %q request contract differs from the frozen config",
					index+1,
					result.CaseID,
				)
			}
			if result.ExecutionAudit.ManifestSHA256 != manifestSHA256 {
				return InstabilityReport{}, fmt.Errorf(
					"repeat %d case %q manifest binding mismatch",
					index+1,
					result.CaseID,
				)
			}
			if campaignRequired &&
				result.ExecutionAudit.Campaign == nil {
				return InstabilityReport{}, fmt.Errorf(
					"repeat %d case %q is missing campaign run binding",
					index+1,
					result.CaseID,
				)
			}
			if campaignRequired && !reflect.DeepEqual(
				*result.ExecutionAudit.Campaign,
				repeatCampaignRuns[index],
			) {
				return InstabilityReport{}, fmt.Errorf(
					"repeat %d case %q campaign run binding differs from the exact repeat plan",
					index+1,
					result.CaseID,
				)
			}
			if !promotionRepeatRouteMatches(
				config,
				primaryResult.ExecutionAudit.Route,
				result.ExecutionAudit.Route,
			) {
				return InstabilityReport{}, fmt.Errorf(
					"repeat %d case %q route binding differs from the primary run",
					index+1,
					result.CaseID,
				)
			}
			if !result.Usage.AccountingComplete ||
				!usageAccountingCompleteForSystem(
					config,
					result.Usage.InputTokens,
					result.Usage.OutputTokens,
				) {
				return InstabilityReport{}, fmt.Errorf(
					"repeat %d case %q has incomplete usage accounting",
					index+1,
					result.CaseID,
				)
			}
			if err := validatePromotionResultCostProvenance(
				costConfig,
				result.Usage,
				fmt.Sprintf(
					"repeat %d case %q",
					index+1,
					result.CaseID,
				),
			); err != nil {
				return InstabilityReport{}, err
			}
			allowance := result.ExecutionAudit.UncappedOutputTokenAllowance
			if allowance != primaryResult.ExecutionAudit.
				UncappedOutputTokenAllowance {
				return InstabilityReport{}, fmt.Errorf(
					"repeat %d case %q uncapped output-token allowance differs from the primary run",
					index+1,
					result.CaseID,
				)
			}
			if isUncappedGenerativeSystem(config) {
				if allowance <= 0 || result.Usage.OutputTokens > allowance {
					return InstabilityReport{}, fmt.Errorf(
						"repeat %d case %q has invalid uncapped output-token allowance evidence",
						index+1,
						result.CaseID,
					)
				}
			} else if allowance != 0 {
				return InstabilityReport{}, fmt.Errorf(
					"repeat %d case %q carries uncapped allowance evidence for a capped system",
					index+1,
					result.CaseID,
				)
			}
			byCase[result.CaseID] = result
		}
		repeatMaps = append(repeatMaps, byCase)
		if len(byCase) != len(sample.Records) {
			return InstabilityReport{}, fmt.Errorf(
				"repeat %d does not cover every sampled case",
				index+1,
			)
		}
	}
	divergentGroups := make(map[string]struct{})
	finalActionFlipGroups := make(map[string]struct{})
	for _, record := range sample.Records {
		signature, err := promotionResultSignature(
			primaryByCase[record.CaseID],
		)
		if err != nil {
			return InstabilityReport{}, err
		}
		finalActionSignature, err := promotionFinalActionSignature(
			primaryByCase[record.CaseID],
		)
		if err != nil {
			return InstabilityReport{}, err
		}
		divergent := false
		for _, byCase := range repeatMaps {
			repeatSignature, err := promotionResultSignature(
				byCase[record.CaseID],
			)
			if err != nil {
				return InstabilityReport{}, err
			}
			if !bytes.Equal(signature, repeatSignature) {
				divergent = true
			}
			repeatFinalAction, err := promotionFinalActionSignature(
				byCase[record.CaseID],
			)
			if err != nil {
				return InstabilityReport{}, err
			}
			if !bytes.Equal(finalActionSignature, repeatFinalAction) {
				suite, err := scoringPrimarySuite(record)
				if err != nil {
					return InstabilityReport{}, err
				}
				finalActionFlipGroups[suite+"\x00"+record.GroupID] = struct{}{}
			}
			primaryIndicators := indicatorsForCase(record, primaryByCase[record.CaseID])
			repeatIndicators := indicatorsForCase(record, byCase[record.CaseID])
			if !primaryIndicators.harm && repeatIndicators.harm {
				report.HarmfulRepeatFlips++
			}
		}
		if divergent {
			report.DivergentCases++
			suite, err := scoringPrimarySuite(record)
			if err != nil {
				return InstabilityReport{}, err
			}
			divergentGroups[suite+"\x00"+record.GroupID] = struct{}{}
		}
	}
	report.DivergentGroups = len(divergentGroups)
	report.InstabilityRate = float64(report.DivergentGroups) /
		float64(report.Groups)
	report.RepeatAgreement = 1 - report.InstabilityRate
	report.FinalActionFlipGroups = len(finalActionFlipGroups)
	report.FinalActionFlipRate = float64(report.FinalActionFlipGroups) /
		float64(report.Groups)
	report.EvidenceComplete = true
	report.Passed = report.RepeatAgreement+1e-12 >=
		gate.Stability.MinRepeatAgreement
	for index := range report.Suites {
		suite := &report.Suites[index]
		for key := range divergentGroups {
			if strings.HasPrefix(key, suite.Suite+"\x00") {
				suite.DivergentGroups++
			}
		}
		for key := range finalActionFlipGroups {
			if strings.HasPrefix(key, suite.Suite+"\x00") {
				suite.FinalActionFlipGroups++
			}
		}
		suite.InstabilityRate = float64(suite.DivergentGroups) /
			float64(suite.SampledGroups)
		suite.UpperConfidenceBound = promotionWilsonUpperBound(
			suite.DivergentGroups,
			suite.SampledGroups,
		)
		suite.FinalActionFlipRate = float64(suite.FinalActionFlipGroups) /
			float64(suite.SampledGroups)
		suite.FinalActionFlipUpperBound = promotionWilsonUpperBound(
			suite.FinalActionFlipGroups,
			suite.SampledGroups,
		)
		suite.Passed = 1-suite.InstabilityRate+1e-12 >=
			gate.Stability.MinRepeatAgreement &&
			suite.FinalActionFlipUpperBound <=
				gate.Stability.MaxFinalActionFlipUCB+1e-12
		if suite.UpperConfidenceBound > report.UpperConfidenceBound {
			report.UpperConfidenceBound = suite.UpperConfidenceBound
		}
		if suite.FinalActionFlipUpperBound > report.FinalActionFlipUpperBound {
			report.FinalActionFlipUpperBound = suite.FinalActionFlipUpperBound
		}
		report.Passed = report.Passed && suite.Passed
	}
	if campaignRequired {
		primaryQuality := promotionCampaignHarmUtilityReport(
			sample,
			primary,
			gate.HarmUtility,
		)
		if !primaryQuality.EvidenceComplete {
			report.EvidenceComplete = false
		}
		for _, repeat := range repeats {
			repeatQuality := promotionCampaignHarmUtilityReport(
				sample,
				repeat,
				gate.HarmUtility,
			)
			if !repeatQuality.EvidenceComplete {
				report.EvidenceComplete = false
				continue
			}
			utilityDrift := math.Abs(repeatQuality.Utility.Rate - primaryQuality.Utility.Rate)
			harmDrift := math.Abs(repeatQuality.Harm.Rate - primaryQuality.Harm.Rate)
			if utilityDrift > report.MaximumUtilityDrift {
				report.MaximumUtilityDrift = utilityDrift
			}
			if harmDrift > report.MaximumHarmDrift {
				report.MaximumHarmDrift = harmDrift
			}
		}
	}
	report.Passed = report.Passed && report.EvidenceComplete &&
		report.MaximumUtilityDrift <= gate.Stability.MaxUtilityDrift+1e-12 &&
		report.MaximumHarmDrift <= gate.Stability.MaxHarmDrift+1e-12 &&
		report.HarmfulRepeatFlips <= gate.Stability.MaxHarmfulRepeatFlips &&
		report.FinalActionFlipUpperBound <= gate.Stability.MaxFinalActionFlipUCB+1e-12
	return report, nil
}

type promotionRepeatGroup struct {
	groupID string
	rank    string
}

func promotionRepeatSample(
	corpus Corpus,
	sampling PromotionRepeatSamplingPlan,
) (Corpus, []InstabilitySlice, error) {
	groupsBySuite := map[string]map[string]struct{}{
		"natural": {},
		"safety":  {},
	}
	groupSuites := make(map[string]string)
	for _, record := range corpus.Records {
		suite, err := scoringPrimarySuite(record)
		if err != nil {
			return Corpus{}, nil, err
		}
		if prior := groupSuites[record.GroupID]; prior != "" &&
			prior != suite {
			return Corpus{}, nil, fmt.Errorf(
				"repeat sample group %q crosses natural and safety suites",
				record.GroupID,
			)
		}
		groupSuites[record.GroupID] = suite
		groupsBySuite[suite][record.GroupID] = struct{}{}
	}
	targets := map[string]int{
		"natural": sampling.NaturalGroups,
		"safety":  sampling.SafetyGroups,
	}
	selected := make(map[string]struct{}, sampling.NaturalGroups+
		sampling.SafetyGroups)
	for _, suite := range []string{"natural", "safety"} {
		groups := make([]promotionRepeatGroup, 0, len(groupsBySuite[suite]))
		for groupID := range groupsBySuite[suite] {
			groups = append(groups, promotionRepeatGroup{
				groupID: groupID,
				rank: sha256Hex([]byte(
					sampling.AlgorithmID + "\x00" +
						string(corpus.Records[0].Task) + "\x00" +
						suite + "\x00" + groupID,
				)),
			})
		}
		sort.Slice(groups, func(i, j int) bool {
			if groups[i].rank != groups[j].rank {
				return groups[i].rank < groups[j].rank
			}
			return groups[i].groupID < groups[j].groupID
		})
		if len(groups) < targets[suite] {
			return Corpus{}, nil, fmt.Errorf(
				"repeat sample requires %d %s groups, holdout has %d",
				targets[suite],
				suite,
				len(groups),
			)
		}
		for _, group := range groups[:targets[suite]] {
			selected[suite+"\x00"+group.groupID] = struct{}{}
		}
	}
	// A release family can contain many aliased cases. Repeating every case in
	// a selected group would make the paid-call ceiling data-dependent and
	// overweight large families. Freeze exactly one representative per group,
	// using the same stable case-id rule as development screening.
	representatives := make(map[string]CorpusRecord, len(selected))
	for _, record := range corpus.Records {
		suite := groupSuites[record.GroupID]
		key := suite + "\x00" + record.GroupID
		if _, ok := selected[key]; !ok {
			continue
		}
		prior, exists := representatives[key]
		if !exists || record.CaseID < prior.CaseID {
			representatives[key] = record
		}
	}
	records := make([]CorpusRecord, 0, len(selected))
	caseCounts := map[string]int{"natural": 0, "safety": 0}
	for key, record := range representatives {
		records = append(records, record)
		suite := strings.SplitN(key, "\x00", 2)[0]
		caseCounts[suite]++
	}
	for _, suite := range []string{"natural", "safety"} {
		if caseCounts[suite] != targets[suite] {
			return Corpus{}, nil, fmt.Errorf(
				"repeat sample selected %d %s representatives, want %d",
				caseCounts[suite],
				suite,
				targets[suite],
			)
		}
	}
	sample, err := NewCorpus(records)
	if err != nil {
		return Corpus{}, nil, fmt.Errorf("repeat sample: %w", err)
	}
	suites := []InstabilitySlice{
		{
			Suite:         "natural",
			SampledGroups: sampling.NaturalGroups,
			SampledCases:  caseCounts["natural"],
		},
		{
			Suite:         "safety",
			SampledGroups: sampling.SafetyGroups,
			SampledCases:  caseCounts["safety"],
		},
	}
	return sample, suites, nil
}

func promotionWilsonUpperBound(divergent int, total int) float64 {
	if total <= 0 {
		return 1
	}
	if divergent < 0 || divergent > total {
		return 1
	}
	const z = 1.6448536269514722
	n := float64(total)
	p := float64(divergent) / n
	zSquared := z * z
	center := p + zSquared/(2*n)
	radius := z * math.Sqrt(p*(1-p)/n+zSquared/(4*n*n))
	upper := (center + radius) / (1 + zSquared/n)
	if upper > 1 {
		return 1
	}
	return upper
}

func promotionWilsonLowerBound(successes int, total int) float64 {
	if total <= 0 {
		return 0
	}
	if successes < 0 || successes > total {
		return 0
	}
	const z = 1.6448536269514722
	n := float64(total)
	p := float64(successes) / n
	zSquared := z * z
	center := p + zSquared/(2*n)
	radius := z * math.Sqrt(p*(1-p)/n+zSquared/(4*n*n))
	lower := (center - radius) / (1 + zSquared/n)
	if lower < 0 {
		return 0
	}
	return lower
}

func promotionRepeatRouteMatches(
	config SystemConfig,
	primary RouteAudit,
	repeat RouteAudit,
) bool {
	if !primary.Verified || !repeat.Verified {
		return false
	}
	if config.Provider == "openrouter" {
		return primary.SnapshotSHA256 == repeat.SnapshotSHA256 &&
			primary.SnapshotFetchedAt == repeat.SnapshotFetchedAt &&
			normalizeRouteModel(primary.ReturnedModel) ==
				normalizeRouteModel(repeat.ReturnedModel) &&
			normalizeRouteProvider(primary.ReturnedProvider) ==
				normalizeRouteProvider(repeat.ReturnedProvider) &&
			(primary.Proof == RouteProofRouterMetadata ||
				primary.Proof == RouteProofGenerationMetadata) &&
			(repeat.Proof == RouteProofRouterMetadata ||
				repeat.Proof == RouteProofGenerationMetadata)
	}
	return primary.Proof == RouteProofDirectResponseModel &&
		repeat.Proof == RouteProofDirectResponseModel &&
		primary.ReturnedModel == repeat.ReturnedModel
}

func promotionResultSignature(result ResultRecord) ([]byte, error) {
	value := struct {
		Status         ResultStatus          `json:"status"`
		ErrorCode      string                `json:"error_code,omitempty"`
		MatcherExtract *MatcherExtractResult `json:"matcher_extract,omitempty"`
		MatcherRerank  *MatcherRerankResult  `json:"matcher_rerank,omitempty"`
		ContentFilter  *ContentFilterResult  `json:"contentfilter,omitempty"`
		JunkPurge      *JunkPurgeResult      `json:"junkpurge,omitempty"`
	}{
		Status:         result.Status,
		ErrorCode:      result.ErrorCode,
		MatcherExtract: result.MatcherExtract,
		MatcherRerank:  result.MatcherRerank,
		ContentFilter:  result.ContentFilter,
		JunkPurge:      result.JunkPurge,
	}
	return json.Marshal(value)
}

// promotionFinalActionSignature intentionally excludes confidence. Stability
// treats confidence drift as output instability, while this signature isolates
// the separately preregistered question of whether the system changed the
// action it would take.
func promotionFinalActionSignature(result ResultRecord) ([]byte, error) {
	value := struct {
		Status        ResultStatus         `json:"status"`
		ErrorCode     string               `json:"error_code,omitempty"`
		MatcherAction MatcherExtractAction `json:"matcher_extract_action,omitempty"`
		Extraction    *MatcherExtraction   `json:"matcher_extraction,omitempty"`
		RerankAction  MatcherRerankAction  `json:"matcher_rerank_action,omitempty"`
		TMDBID        int64                `json:"tmdb_id,omitempty"`
		ContentAction ContentFilterAction  `json:"contentfilter_action,omitempty"`
		JunkAction    JunkPurgeAction      `json:"junkpurge_action,omitempty"`
	}{
		Status:    result.Status,
		ErrorCode: result.ErrorCode,
	}
	if result.MatcherExtract != nil {
		value.MatcherAction = result.MatcherExtract.Action
		value.Extraction = result.MatcherExtract.Extraction
	}
	if result.MatcherRerank != nil {
		value.RerankAction = result.MatcherRerank.Action
		value.TMDBID = result.MatcherRerank.TMDBID
	}
	if result.ContentFilter != nil {
		value.ContentAction = result.ContentFilter.Action
	}
	if result.JunkPurge != nil {
		value.JunkAction = result.JunkPurge.Action
	}
	return json.Marshal(value)
}

func promotionCostProjection(
	config SystemConfig,
	evidence PromotionAttemptEvidence,
	basis PromotionCostBasis,
	windowDays float64,
	serviceTier PromotionServiceTier,
) (CostProjection, error) {
	if err := validatePromotionServiceTier(config, serviceTier); err != nil {
		return CostProjection{}, err
	}
	pricedConfig, err := promotionPricedSystem(config, serviceTier, basis)
	if err != nil {
		return CostProjection{}, err
	}
	projection := CostProjection{
		MeasuredCases:                  0,
		MeasuredProviderRequests:       len(evidence.Attempts),
		ObservedWindowEligibleRequests: basis.ObservedEligibleRequests,
		SelectionServiceTier:           serviceTier,
	}
	caseIDs := make(map[string]struct{})
	var standardMicroUSD int64
	for _, attempt := range evidence.Attempts {
		caseIDs[attempt.CaseID] = struct{}{}
		// Promotion economics are derived from frozen prices and observed token
		// counts, never from a caller-supplied positive dollar value. The exact
		// equality/provenance check in validatePromotionAttemptEvidence makes a
		// provider pricing drift fail closed; this recomputation also prevents a
		// future validation regression from silently changing ranking inputs.
		pricedUsage := attempt.Usage
		pricedUsage.CostMicroUSD = pricedConfig.EstimateCostWithCacheWriteMicroUSD(
			attempt.Usage.InputTokens,
			attempt.Usage.CachedInputTokens,
			attempt.Usage.CacheWriteTokens,
			attempt.Usage.OutputTokens,
		)
		if err := addPromotionUsageTotals(
			&projection.MeasuredUsage,
			pricedUsage,
		); err != nil {
			return CostProjection{}, err
		}
		estimated := config.EstimateCostWithCacheWriteMicroUSD(
			attempt.Usage.InputTokens,
			attempt.Usage.CachedInputTokens,
			attempt.Usage.CacheWriteTokens,
			attempt.Usage.OutputTokens,
		)
		if config.Provider == "openrouter" {
			multiplier := config.billingMultiplier()
			estimated = int64(float64(estimated)/multiplier + 0.5)
		}
		next, err := checkedAddInt64(standardMicroUSD, estimated)
		if err != nil {
			return CostProjection{}, err
		}
		standardMicroUSD = next
	}
	projection.MeasuredCases = len(caseIDs)
	if projection.MeasuredCases == 0 {
		return CostProjection{}, fmt.Errorf("cost projection has no measured cases")
	}
	projection.MeasuredUsage.Results = projection.MeasuredCases
	projection.MeasuredUsage.Requests = len(evidence.Attempts)
	projection.MeasuredUsage.TotalTokens =
		projection.MeasuredUsage.InputTokens +
			projection.MeasuredUsage.OutputTokens
	projection.MeasuredUsage.CostUSD =
		float64(projection.MeasuredUsage.CostMicroUSD) / 1_000_000
	projection.MeasuredCashCostUSD = projection.MeasuredUsage.CostUSD
	projection.RecurringCashMultiplier = config.billingMultiplier()
	projection.MeasuredProviderCreditsUSD =
		projection.MeasuredCashCostUSD / projection.RecurringCashMultiplier
	if config.Provider == "openrouter" {
		projection.OpenRouterOneTimeMinimumFeeUSD = 0.80
	}
	projection.ProjectedThirtyDayEligibleRequests =
		float64(basis.ObservedEligibleRequests) * 30 / windowDays
	scale := projection.ProjectedThirtyDayEligibleRequests /
		float64(projection.MeasuredCases)
	projection.ProjectedThirtyDayCashCostUSD =
		projection.MeasuredCashCostUSD * scale
	projection.ProjectedThirtyDayProviderCreditsUSD =
		projection.MeasuredProviderCreditsUSD * scale
	projection.ProjectedCostPerThousandUSD =
		projection.MeasuredCashCostUSD /
			float64(projection.MeasuredCases) * 1000
	if config.Provider == "openai" {
		standard := float64(standardMicroUSD) / 1_000_000 * scale
		batch := standard *
			float64(basis.OpenAIBatchMultiplierPPM) / 1_000_000
		flex := standard *
			float64(basis.OpenAIFlexMultiplierPPM) / 1_000_000
		projection.OpenAIStandardNormalizedUSD = &standard
		projection.OpenAIBatchNormalizedUSD = &batch
		projection.OpenAIFlexNormalizedUSD = &flex
	}
	switch serviceTier {
	case PromotionServiceStandard:
		if config.Provider == "openai" {
			projection.SelectionProjectedThirtyDayUSD =
				*projection.OpenAIStandardNormalizedUSD
		} else {
			projection.SelectionProjectedThirtyDayUSD =
				projection.ProjectedThirtyDayCashCostUSD
		}
	case PromotionServiceOpenAIBatch:
		projection.SelectionProjectedThirtyDayUSD =
			*projection.OpenAIBatchNormalizedUSD
	case PromotionServiceOpenAIFlex:
		projection.SelectionProjectedThirtyDayUSD =
			*projection.OpenAIFlexNormalizedUSD
	}
	return projection, nil
}

func hasNaturalAndSafetyReports(score ScoreReport) bool {
	if len(score.Suites) != 2 {
		return false
	}
	natural := 0
	safety := 0
	for _, suite := range score.Suites {
		if suite.Suite == "natural" {
			natural++
		} else {
			safety++
		}
	}
	return natural == 1 && safety == 1
}

func everyTaskGatePassed(gates TaskComparisonGates) bool {
	return gates.HarmNonInferiority.Decision == ComparisonPass &&
		gates.SuccessNonInferiority.Decision == ComparisonPass &&
		gates.SchemaValidity.Decision == ComparisonPass &&
		gates.ZeroSafetyHarm.Decision == ComparisonPass &&
		gates.SafetySuccessNonInferiority.Decision == ComparisonPass &&
		gates.SafetySchemaValidity.Decision == ComparisonPass
}

func promotionLaneEligible(lane EvaluationLane) bool {
	switch lane {
	case EvaluationLaneProductionFidelity,
		EvaluationLaneNormalizedStrict:
		return true
	default:
		return false
	}
}

type promotionHolmRaw struct {
	candidate  string
	metric     string
	p          float64
	components []PromotionIUTComponent
}

func buildPromotionHolmFamilies(
	corpus Corpus,
	candidateIDs []string,
	evidence map[string]PromotionSystemEvidence,
	results map[string][]ResultRecord,
	options ComparisonOptions,
) ([]HolmFamilyReport, map[string]bool, error) {
	metricNames := []string{
		"natural_primary_harm_noninferiority",
		"natural_task_success_noninferiority",
		"safety_task_success_noninferiority",
	}
	const familyMetric = "dual_control_all_required_noninferiority"
	raw := make([]promotionHolmRaw, 0, len(candidateIDs))
	for _, candidateID := range candidateIDs {
		comparators := []struct {
			name     string
			systemID string
		}{
			{
				name:     "normalized_control",
				systemID: evidence[candidateID].ControlSystemID,
			},
			{
				name:     "production_control",
				systemID: evidence[candidateID].ProductionControlSystemID,
			},
		}
		components := make([]PromotionIUTComponent, 0, 6)
		iutP := 0.0
		for _, comparator := range comparators {
			metrics, err := promotionNoninferiorityPValues(
				corpus,
				results[comparator.systemID],
				results[candidateID],
				options,
				candidateID+":"+comparator.name,
			)
			if err != nil {
				return nil, nil, fmt.Errorf(
					"candidate %q %s Holm evidence: %w",
					candidateID,
					comparator.name,
					err,
				)
			}
			for _, metric := range metricNames {
				p, ok := metrics[metric]
				if !ok {
					return nil, nil, fmt.Errorf(
						"candidate %q %s Holm evidence omitted metric %q",
						candidateID,
						comparator.name,
						metric,
					)
				}
				components = append(components, PromotionIUTComponent{
					Comparator:         comparator.name,
					ComparatorSystemID: comparator.systemID,
					Metric:             metric,
					UnadjustedP:        p,
				})
				if p > iutP {
					iutP = p
				}
			}
		}
		raw = append(raw, promotionHolmRaw{
			candidate:  candidateID,
			metric:     familyMetric,
			p:          iutP,
			components: components,
		})
	}
	passed := make(map[string]bool, len(candidateIDs))
	for _, candidateID := range candidateIDs {
		passed[candidateID] = false
	}
	sort.Slice(raw, func(i, j int) bool {
		if math.Abs(raw[i].p-raw[j].p) > 1e-15 {
			return raw[i].p < raw[j].p
		}
		return raw[i].candidate < raw[j].candidate
	})
	adjustedByCandidate := make(map[string]float64, len(raw))
	running := 0.0
	for index, hypothesis := range raw {
		adjusted := hypothesis.p * float64(len(raw)-index)
		if adjusted > 1 {
			adjusted = 1
		}
		if adjusted < running {
			adjusted = running
		}
		running = adjusted
		adjustedByCandidate[hypothesis.candidate] = adjusted
	}
	sort.Slice(raw, func(i, j int) bool {
		return raw[i].candidate < raw[j].candidate
	})
	family := HolmFamilyReport{
		Task:   corpus.Records[0].Task,
		Metric: familyMetric,
		Method: "candidate intersection-union p=max(six one-sided group-cluster bootstrap non-inferiority p-values: three metrics against each frozen control), followed by one Holm step-down family across every candidate in the frozen task roster",
	}
	for _, hypothesis := range raw {
		adjusted := adjustedByCandidate[hypothesis.candidate]
		hypothesisPassed := adjusted <= PromotionFamilyAlpha+1e-12
		passed[hypothesis.candidate] = hypothesisPassed
		family.Hypotheses = append(
			family.Hypotheses,
			HolmHypothesis{
				CandidateSystemID: hypothesis.candidate,
				Metric:            familyMetric,
				Components:        hypothesis.components,
				UnadjustedP:       hypothesis.p,
				AdjustedP:         adjusted,
				Alpha:             PromotionFamilyAlpha,
				Passed:            hypothesisPassed,
			},
		)
	}
	return []HolmFamilyReport{family}, passed, nil
}

func promotionNoninferiorityPValues(
	corpus Corpus,
	controlResults []ResultRecord,
	candidateResults []ResultRecord,
	options ComparisonOptions,
	candidateID string,
) (map[string]float64, error) {
	controlByCase := make(map[string]ResultRecord, len(controlResults))
	for _, result := range controlResults {
		controlByCase[result.CaseID] = result
	}
	candidateByCase := make(map[string]ResultRecord, len(candidateResults))
	for _, result := range candidateResults {
		candidateByCase[result.CaseID] = result
	}
	var harm, success, safetySuccess []pairedObservation
	for _, record := range corpus.Records {
		suite, err := scoringPrimarySuite(record)
		if err != nil {
			return nil, err
		}
		control := controlByCase[record.CaseID]
		candidate := candidateByCase[record.CaseID]
		controlIndicators := indicatorsForCase(record, control)
		candidateIndicators := indicatorsForCase(record, candidate)
		if suite == "natural" {
			if controlIndicators.harmEligible {
				harm = append(harm, pairedObservation{
					groupID:   record.GroupID,
					control:   controlIndicators.harm,
					candidate: candidateIndicators.harm,
				})
			}
			if controlIndicators.successEligible {
				success = append(success, pairedObservation{
					groupID:   record.GroupID,
					control:   controlIndicators.success,
					candidate: candidateIndicators.success,
				})
			}
		} else {
			safetySuccess = append(safetySuccess, pairedObservation{
				groupID:   record.GroupID,
				control:   goldOutcomeSuccess(record, control),
				candidate: goldOutcomeSuccess(record, candidate),
			})
		}
	}
	return map[string]float64{
		"natural_primary_harm_noninferiority": bootstrapNoninferiorityP(
			harm,
			ComparisonHarmMargin,
			true,
			options.BootstrapReplicates,
			deriveBootstrapSeed(
				options.BootstrapSeed,
				candidateID+":holm:natural:harm",
			),
		),
		"natural_task_success_noninferiority": bootstrapNoninferiorityP(
			success,
			-ComparisonSuccessMargin,
			false,
			options.BootstrapReplicates,
			deriveBootstrapSeed(
				options.BootstrapSeed,
				candidateID+":holm:natural:success",
			),
		),
		"safety_task_success_noninferiority": bootstrapNoninferiorityP(
			safetySuccess,
			-ComparisonSuccessMargin,
			false,
			options.BootstrapReplicates,
			deriveBootstrapSeed(
				options.BootstrapSeed,
				candidateID+":holm:safety:success",
			),
		),
	}, nil
}

// bootstrapNoninferiorityP returns a deterministic centered cluster-bootstrap
// one-sided p-value. upperNull is true for H0 delta >= threshold; otherwise
// H0 delta <= threshold. Add-one correction prevents a numerical zero.
func bootstrapNoninferiorityP(
	observations []pairedObservation,
	threshold float64,
	upperNull bool,
	replicates int,
	seed uint64,
) float64 {
	if len(observations) == 0 || replicates <= 0 {
		return 1
	}
	byGroup := make(map[string]clusterAggregate)
	controlTotal := 0
	candidateTotal := 0
	for _, observation := range observations {
		aggregate := byGroup[observation.groupID]
		aggregate.eligible++
		if observation.control {
			aggregate.control++
			controlTotal++
		}
		if observation.candidate {
			aggregate.candidate++
			candidateTotal++
		}
		byGroup[observation.groupID] = aggregate
	}
	groupIDs := make([]string, 0, len(byGroup))
	for groupID := range byGroup {
		groupIDs = append(groupIDs, groupID)
	}
	sort.Strings(groupIDs)
	groups := make([]clusterAggregate, 0, len(groupIDs))
	for _, groupID := range groupIDs {
		groups = append(groups, byGroup[groupID])
	}
	observed := float64(candidateTotal-controlTotal) /
		float64(len(observations))
	requiredCentered := threshold - observed
	rng := splitMix64{state: seed}
	extreme := 0
	for replicate := 0; replicate < replicates; replicate++ {
		eligible := 0
		control := 0
		candidate := 0
		for draw := 0; draw < len(groups); draw++ {
			aggregate := groups[rng.intn(len(groups))]
			eligible += aggregate.eligible
			control += aggregate.control
			candidate += aggregate.candidate
		}
		difference := float64(candidate-control)/float64(eligible) - observed
		if upperNull {
			if difference >= requiredCentered-1e-15 {
				extreme++
			}
		} else if difference <= requiredCentered+1e-15 {
			extreme++
		}
	}
	return float64(extreme+1) / float64(replicates+1)
}

func canonicalJSONIdentity(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("canonical JSON identity: %w", err)
	}
	raw = append(raw, '\n')
	return sha256Hex(raw), nil
}

func sumPromotionUsage(left, right Usage) (Usage, error) {
	result := Usage{
		AccountingComplete: left.AccountingComplete && right.AccountingComplete,
	}
	switch {
	case left.CostSource == "":
		result.CostSource = right.CostSource
	case right.CostSource == "":
		result.CostSource = left.CostSource
	case left.CostSource == right.CostSource:
		result.CostSource = left.CostSource
	default:
		return Usage{}, fmt.Errorf(
			"cannot aggregate mixed cost sources %q and %q",
			left.CostSource,
			right.CostSource,
		)
	}
	var err error
	if result.InputTokens, err = checkedAddInt64(
		left.InputTokens,
		right.InputTokens,
	); err != nil {
		return Usage{}, err
	}
	if result.CachedInputTokens, err = checkedAddInt64(
		left.CachedInputTokens,
		right.CachedInputTokens,
	); err != nil {
		return Usage{}, err
	}
	if result.CacheWriteTokens, err = checkedAddInt64(
		left.CacheWriteTokens,
		right.CacheWriteTokens,
	); err != nil {
		return Usage{}, err
	}
	if result.OutputTokens, err = checkedAddInt64(
		left.OutputTokens,
		right.OutputTokens,
	); err != nil {
		return Usage{}, err
	}
	if result.ReasoningTokens, err = checkedAddInt64(
		left.ReasoningTokens,
		right.ReasoningTokens,
	); err != nil {
		return Usage{}, err
	}
	if result.CostMicroUSD, err = checkedAddInt64(
		left.CostMicroUSD,
		right.CostMicroUSD,
	); err != nil {
		return Usage{}, err
	}
	return result, nil
}

func usageNumbersEqual(left, right Usage) bool {
	return left.InputTokens == right.InputTokens &&
		left.CachedInputTokens == right.CachedInputTokens &&
		left.CacheWriteTokens == right.CacheWriteTokens &&
		left.OutputTokens == right.OutputTokens &&
		left.ReasoningTokens == right.ReasoningTokens &&
		left.CostMicroUSD == right.CostMicroUSD &&
		left.CostSource == right.CostSource &&
		left.AccountingComplete == right.AccountingComplete
}

func addPromotionUsageTotals(total *UsageTotals, usage Usage) error {
	var err error
	if total.InputTokens, err = checkedAddInt64(
		total.InputTokens,
		usage.InputTokens,
	); err != nil {
		return err
	}
	if total.CachedInputTokens, err = checkedAddInt64(
		total.CachedInputTokens,
		usage.CachedInputTokens,
	); err != nil {
		return err
	}
	if total.CacheWriteTokens, err = checkedAddInt64(
		total.CacheWriteTokens,
		usage.CacheWriteTokens,
	); err != nil {
		return err
	}
	if total.OutputTokens, err = checkedAddInt64(
		total.OutputTokens,
		usage.OutputTokens,
	); err != nil {
		return err
	}
	if total.ReasoningTokens, err = checkedAddInt64(
		total.ReasoningTokens,
		usage.ReasoningTokens,
	); err != nil {
		return err
	}
	if total.CostMicroUSD, err = checkedAddInt64(
		total.CostMicroUSD,
		usage.CostMicroUSD,
	); err != nil {
		return err
	}
	if total.Requests == 0 {
		total.AccountingComplete = true
	}
	if !usage.AccountingComplete {
		total.AccountingComplete = false
		total.IncompleteRequests++
	}
	total.Requests++
	return nil
}

func compactStrings(values []string) []string {
	if len(values) == 0 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

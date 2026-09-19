package llmeval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
)

const (
	FinalistRosterProtocolVersion      = "bitagent-stage1-finalists-v3"
	FinalistRosterOrdering             = "manifest_recomputed_stage1_cost_then_system_id_all_stage1_passers"
	StageOneRosterProtocolVersion      = "bitagent-stage1-screening-v1"
	StageOneRequiredPrimarySuite       = "natural"
	StageOneRequiredSafetySuite        = "safety"
	StageOneRequiredJunkSafetySuite    = "safety_top_up"
	maxFinalistRosterBytes             = 64 << 20
	maxStageOneRosterBytes             = 4 << 20
	maxStageOneScoreReportBytes        = 16 << 20
	maxStageOneComparisonReportBytes   = 32 << 20
	stageOneComparisonArtifactCount    = 2
	stageOneRequiredControlSystemCount = 2
)

// FinalistRosterManifest is frozen after development scoring and before any
// holdout review or provider execution. It contains every Stage 1 passer, not
// merely the two cheapest, so later safety failures cannot hide a viable
// model. Passers are derived from the embedded, recomputed Stage 1 evidence;
// there is no operator-supplied completeness assertion.
type FinalistRosterManifest struct {
	SchemaVersion                    int                  `json:"schema_version"`
	ProtocolVersion                  string               `json:"protocol_version"`
	PlanSHA256                       string               `json:"plan_sha256"`
	DevelopmentClosureManifestSHA256 string               `json:"development_closure_manifest_sha256"`
	DevelopmentCorpusSHA256          string               `json:"development_corpus_sha256"`
	SystemManifestSHA256             string               `json:"system_manifest_sha256"`
	Ordering                         string               `json:"ordering"`
	Tasks                            []FinalistTaskRoster `json:"tasks"`
}

type FinalistTaskRoster struct {
	Task Task `json:"task"`
	// TaskDevelopmentCorpusSHA256 is the canonical SHA-256 of the development
	// corpus filtered to exactly this task. Stage 1 scores and comparisons are
	// per-task artifacts, so they bind this rather than the whole-phase
	// development corpus; FinalizeHoldoutGold recomputes it from the frozen
	// development corpus so a roster cannot declare a favourable subset.
	TaskDevelopmentCorpusSHA256 string                           `json:"task_development_corpus_sha256"`
	StageOneRosterSHA256        string                           `json:"stage_one_roster_sha256"`
	PromotionRosterSHA256       string                           `json:"promotion_roster_sha256"`
	ComparisonPolicy            StageOneComparisonPolicy         `json:"comparison_policy"`
	SystemEvidence              []FinalistStageOneSystemEvidence `json:"system_evidence"`
	SystemIDs                   []string                         `json:"system_ids"`
	StageOneCandidateSystems    int                              `json:"stage_one_candidate_systems"`
	StageOnePassingSystems      int                              `json:"stage_one_passing_systems"`
}

// StageOneComparisonPolicy freezes every option that can affect the
// development-screen comparison. Statistical margins remain compile-time
// protocol constants and are repeated here so a roster with different claimed
// gates is rejected before any evidence is read.
type StageOneComparisonPolicy struct {
	BootstrapReplicates int     `json:"bootstrap_replicates"`
	BootstrapSeed       uint64  `json:"bootstrap_seed"`
	Confidence          float64 `json:"confidence"`
	HarmMargin          float64 `json:"harm_margin"`
	SuccessMargin       float64 `json:"success_margin"`
	SchemaValidFloor    float64 `json:"schema_valid_floor"`
	PrimarySuite        string  `json:"primary_suite"`
	SafetySuite         string  `json:"safety_suite"`
	DualControls        bool    `json:"dual_controls"`
}

// StageOneSafetySuite is the suite name that carries a task's safety cases.
// Junk purge separates its safety cohort as a top-up suite, so the Stage 1
// gates cannot assume one global safety suite name.
func StageOneSafetySuite(task Task) string {
	if task == TaskJunkPurge {
		return StageOneRequiredJunkSafetySuite
	}
	return StageOneRequiredSafetySuite
}

func DefaultStageOneComparisonPolicy(task Task) StageOneComparisonPolicy {
	return StageOneComparisonPolicy{
		BootstrapReplicates: defaultBootstrapReplicates,
		BootstrapSeed:       defaultBootstrapSeed,
		Confidence:          ComparisonConfidence,
		HarmMargin:          ComparisonHarmMargin,
		SuccessMargin:       ComparisonSuccessMargin,
		SchemaValidFloor:    ComparisonSchemaValidFloor,
		PrimarySuite:        StageOneRequiredPrimarySuite,
		SafetySuite:         StageOneSafetySuite(task),
		DualControls:        true,
	}
}

func (policy StageOneComparisonPolicy) validate(task Task) error {
	if policy.BootstrapReplicates < 1_000 ||
		policy.BootstrapReplicates > maxBootstrapReplicates {
		return fmt.Errorf(
			"bootstrap_replicates must be between 1000 and %d",
			maxBootstrapReplicates,
		)
	}
	if policy.BootstrapSeed == 0 {
		return fmt.Errorf("bootstrap_seed must be positive")
	}
	expected := DefaultStageOneComparisonPolicy(task)
	if policy.Confidence != expected.Confidence ||
		policy.HarmMargin != expected.HarmMargin ||
		policy.SuccessMargin != expected.SuccessMargin ||
		policy.SchemaValidFloor != expected.SchemaValidFloor ||
		policy.PrimarySuite != expected.PrimarySuite ||
		policy.SafetySuite != expected.SafetySuite ||
		!policy.DualControls {
		return fmt.Errorf(
			"Stage 1 comparison policy does not match the frozen protocol gates",
		)
	}
	return nil
}

func (policy StageOneComparisonPolicy) options(
	evaluatorBuildSHA256 string,
) ComparisonOptions {
	return ComparisonOptions{
		BootstrapReplicates:         policy.BootstrapReplicates,
		BootstrapSeed:               policy.BootstrapSeed,
		ProductionSuiteBinding:      true,
		ClosureBoundHoldout:         false,
		PromotionMode:               false,
		EvaluatorBuildSHA256:        evaluatorBuildSHA256,
		ControlDecisionThresholds:   ProductionThresholds(),
		CandidateDecisionThresholds: ProductionThresholds(),
	}
}

// StageOneScreeningRoster is frozen before Stage 1 execution. Unlike the
// post-screen promotion roster, it contains every screened candidate. Its
// exact candidate universe is what makes a later passer omission detectable.
type StageOneScreeningRoster struct {
	SchemaVersion                    int                      `json:"schema_version"`
	ProtocolVersion                  string                   `json:"protocol_version"`
	Task                             Task                     `json:"task"`
	PlanSHA256                       string                   `json:"plan_sha256"`
	DevelopmentClosureManifestSHA256 string                   `json:"development_closure_manifest_sha256"`
	DevelopmentCorpusSHA256          string                   `json:"development_corpus_sha256"`
	SystemManifestSHA256             string                   `json:"system_manifest_sha256"`
	EvaluatorBuildSHA256             string                   `json:"evaluator_build_sha256"`
	ComparisonPolicy                 StageOneComparisonPolicy `json:"comparison_policy"`
	Systems                          []PromotionRosterSystem  `json:"systems"`
}

// FinalistStageOneComparisonEvidence embeds the exact parsed comparison report
// as well as the byte-level and canonical identities checked during
// recomputation. Finalist validation re-derives every gate decision from the
// embedded metrics instead of trusting DiagnosticVerdict.
type FinalistStageOneComparisonEvidence struct {
	ReportArtifactSHA256  string           `json:"report_artifact_sha256"`
	CanonicalReportSHA256 string           `json:"canonical_report_sha256"`
	Report                ComparisonReport `json:"report"`
}

// FinalistStageOneSystemEvidence is the source-free, machine-verifiable record
// of one frozen Stage 1 roster entry.
type FinalistStageOneSystemEvidence struct {
	Role                         PromotionSystemRole                 `json:"role"`
	SystemID                     string                              `json:"system_id"`
	ControlSystemID              string                              `json:"control_system_id,omitempty"`
	ProductionControlSystemID    string                              `json:"production_control_system_id,omitempty"`
	DecisionThresholds           PromotionDecisionThresholds         `json:"decision_thresholds"`
	DeploymentServiceTier        PromotionServiceTier                `json:"deployment_service_tier"`
	Lane                         EvaluationLane                      `json:"lane"`
	ResultsArtifactSHA256        string                              `json:"results_artifact_sha256"`
	CanonicalResultsSHA256       string                              `json:"canonical_results_sha256"`
	ScoreReportArtifactSHA256    string                              `json:"score_report_artifact_sha256"`
	CanonicalScoreReportSHA256   string                              `json:"canonical_score_report_sha256"`
	Score                        ScoreReport                         `json:"score"`
	NormalizedComparison         *FinalistStageOneComparisonEvidence `json:"normalized_comparison,omitempty"`
	ProductionComparison         *FinalistStageOneComparisonEvidence `json:"production_comparison,omitempty"`
	MeasuredStageOneCostMicroUSD int64                               `json:"measured_stage1_cost_micro_usd"`
	// ManifestRecomputedStageOneCostMicroUSD is the deterministic ranking
	// value derived from aggregate token/request counts and the exact system
	// manifest. The measured provider value remains an audit diagnostic but
	// cannot decide which passing candidates reach holdout.
	ManifestRecomputedStageOneCostMicroUSD int64    `json:"manifest_recomputed_stage1_cost_micro_usd"`
	Passed                                 bool     `json:"passed"`
	ReasonCodes                            []string `json:"reason_codes"`
}

func (manifest FinalistRosterManifest) Validate(
	systemManifest SystemManifest,
	systemManifestSHA256 string,
) error {
	if manifest.SchemaVersion != SchemaVersion ||
		manifest.ProtocolVersion != FinalistRosterProtocolVersion {
		return fmt.Errorf(
			"finalist roster has unsupported schema_version or protocol_version",
		)
	}
	if err := validateSHA256(
		"exact_system_manifest_sha256",
		systemManifestSHA256,
	); err != nil {
		return err
	}
	if manifest.SystemManifestSHA256 != systemManifestSHA256 {
		return fmt.Errorf(
			"finalist roster system_manifest_sha256 does not match the exact system manifest",
		)
	}
	if err := systemManifest.Validate(); err != nil {
		return fmt.Errorf("system manifest: %w", err)
	}
	configs := make(map[string]SystemConfig, len(systemManifest.Systems))
	for _, config := range systemManifest.Systems {
		configs[config.SystemID] = config
	}
	for name, value := range map[string]string{
		"plan_sha256":                         manifest.PlanSHA256,
		"development_closure_manifest_sha256": manifest.DevelopmentClosureManifestSHA256,
		"development_corpus_sha256":           manifest.DevelopmentCorpusSHA256,
		"system_manifest_sha256":              manifest.SystemManifestSHA256,
	} {
		if err := validateSHA256(name, value); err != nil {
			return err
		}
	}
	if manifest.Ordering != FinalistRosterOrdering {
		return fmt.Errorf(
			"finalist roster ordering got %q, want %q",
			manifest.Ordering,
			FinalistRosterOrdering,
		)
	}
	if len(manifest.Tasks) != len(orderedTasks) {
		return fmt.Errorf(
			"finalist roster has %d tasks, want %d",
			len(manifest.Tasks),
			len(orderedTasks),
		)
	}
	for index, task := range manifest.Tasks {
		if task.Task != orderedTasks[index] {
			return fmt.Errorf(
				"finalist roster task %d got %q, want %q",
				index,
				task.Task,
				orderedTasks[index],
			)
		}
		if err := validateSHA256(
			fmt.Sprintf("tasks[%d].task_development_corpus_sha256", index),
			task.TaskDevelopmentCorpusSHA256,
		); err != nil {
			return err
		}
		if err := validateSHA256(
			fmt.Sprintf("tasks[%d].stage_one_roster_sha256", index),
			task.StageOneRosterSHA256,
		); err != nil {
			return err
		}
		if err := validateSHA256(
			fmt.Sprintf("tasks[%d].promotion_roster_sha256", index),
			task.PromotionRosterSHA256,
		); err != nil {
			return err
		}
		if err := task.ComparisonPolicy.validate(task.Task); err != nil {
			return fmt.Errorf(
				"finalist roster task %q comparison policy: %w",
				task.Task,
				err,
			)
		}
		passers, candidateCount, err := validateFinalistTaskEvidence(
			task,
			configs,
		)
		if err != nil {
			return fmt.Errorf(
				"finalist roster task %q evidence: %w",
				task.Task,
				err,
			)
		}
		if task.StageOneCandidateSystems != candidateCount {
			return fmt.Errorf(
				"finalist roster task %q candidate count got %d, recomputed %d",
				task.Task,
				task.StageOneCandidateSystems,
				candidateCount,
			)
		}
		if task.StageOnePassingSystems != len(passers) {
			return fmt.Errorf(
				"finalist roster task %q passing count got %d, recomputed %d",
				task.Task,
				task.StageOnePassingSystems,
				len(passers),
			)
		}
		if !sameOrderedStrings(task.SystemIDs, passers) {
			return fmt.Errorf(
				"finalist roster task %q system_ids do not exactly equal the recomputed ordered all-passers list",
				task.Task,
			)
		}
	}
	return nil
}

func validateFinalistTaskEvidence(
	task FinalistTaskRoster,
	configs map[string]SystemConfig,
) ([]string, int, error) {
	if len(task.SystemEvidence) < stageOneRequiredControlSystemCount+1 {
		return nil, 0, fmt.Errorf(
			"requires two controls and at least one screened candidate",
		)
	}
	byID := make(
		map[string]FinalistStageOneSystemEvidence,
		len(task.SystemEvidence),
	)
	normalizedControls := 0
	productionControls := 0
	candidateCount := 0
	for index, evidence := range task.SystemEvidence {
		if err := validateIdentifier(
			fmt.Sprintf("system_evidence[%d].system_id", index),
			evidence.SystemID,
		); err != nil {
			return nil, 0, err
		}
		if _, duplicate := byID[evidence.SystemID]; duplicate {
			return nil, 0, fmt.Errorf(
				"duplicates Stage 1 system %q",
				evidence.SystemID,
			)
		}
		byID[evidence.SystemID] = evidence
		switch evidence.Role {
		case PromotionRoleControl:
			if evidence.ControlSystemID != "" ||
				evidence.ProductionControlSystemID != "" {
				return nil, 0, fmt.Errorf(
					"control %q names a comparator",
					evidence.SystemID,
				)
			}
			if evidence.Lane == EvaluationLaneNormalizedStrict {
				normalizedControls++
			}
			if evidence.Lane == EvaluationLaneProductionFidelity {
				productionControls++
			}
		case PromotionRoleCandidate:
			candidateCount++
			if evidence.ControlSystemID == "" ||
				evidence.ProductionControlSystemID == "" {
				return nil, 0, fmt.Errorf(
					"candidate %q must name both frozen controls",
					evidence.SystemID,
				)
			}
		default:
			return nil, 0, fmt.Errorf(
				"system %q has unsupported role %q",
				evidence.SystemID,
				evidence.Role,
			)
		}
	}
	if normalizedControls != 1 || productionControls != 1 {
		return nil, 0, fmt.Errorf(
			"requires exactly one normalized_strict and one production_fidelity control, got %d and %d",
			normalizedControls,
			productionControls,
		)
	}

	type passingEvidence struct {
		systemID string
		cost     int64
	}
	passing := make([]passingEvidence, 0, candidateCount)
	for index, evidence := range task.SystemEvidence {
		expectedPass, expectedReasons, err := validateFinalistSystemEvidence(
			task,
			evidence,
			byID,
		)
		if err != nil {
			return nil, 0, fmt.Errorf(
				"system_evidence[%d] %q: %w",
				index,
				evidence.SystemID,
				err,
			)
		}
		config, exists := configs[evidence.SystemID]
		if !exists {
			return nil, 0, fmt.Errorf(
				"system_evidence[%d] %q is absent from the exact system manifest",
				index,
				evidence.SystemID,
			)
		}
		recomputedCost, err := validateFinalistSystemManifestBinding(
			task.Task,
			evidence,
			config,
		)
		if err != nil {
			return nil, 0, fmt.Errorf(
				"system_evidence[%d] %q: %w",
				index,
				evidence.SystemID,
				err,
			)
		}
		if evidence.Passed != expectedPass {
			return nil, 0, fmt.Errorf(
				"system_evidence[%d] %q pass attestation got %t, recomputed %t",
				index,
				evidence.SystemID,
				evidence.Passed,
				expectedPass,
			)
		}
		if !sameOrderedStrings(evidence.ReasonCodes, expectedReasons) {
			return nil, 0, fmt.Errorf(
				"system_evidence[%d] %q reason_codes differ from recomputed decision",
				index,
				evidence.SystemID,
			)
		}
		if expectedPass {
			passing = append(passing, passingEvidence{
				systemID: evidence.SystemID,
				cost:     recomputedCost,
			})
		}
	}
	sort.Slice(passing, func(left, right int) bool {
		if passing[left].cost != passing[right].cost {
			return passing[left].cost < passing[right].cost
		}
		return passing[left].systemID < passing[right].systemID
	})
	passers := make([]string, 0, len(passing))
	for _, evidence := range passing {
		passers = append(passers, evidence.systemID)
	}
	return passers, candidateCount, nil
}

func validateFinalistSystemManifestBinding(
	task Task,
	evidence FinalistStageOneSystemEvidence,
	config SystemConfig,
) (int64, error) {
	if !config.SupportsTask(task) {
		return 0, fmt.Errorf(
			"exact system manifest does not support task %q",
			task,
		)
	}
	if evidence.Score.System != config.Descriptor() {
		return 0, fmt.Errorf(
			"score descriptor does not match the exact system manifest",
		)
	}
	if evidence.Lane != config.EvaluationLane {
		return 0, fmt.Errorf(
			"evaluation lane does not match the exact system manifest",
		)
	}
	if err := validatePromotionServiceTier(
		config,
		evidence.DeploymentServiceTier,
	); err != nil {
		return 0, err
	}
	label := fmt.Sprintf(
		"finalist roster evidence system %q",
		evidence.SystemID,
	)
	if err := validatePromotionAggregateCost(
		config,
		evidence.Score.Usage,
		evidence.DeploymentServiceTier,
		label,
	); err != nil {
		return 0, err
	}
	recomputedCost, err := promotionAggregateManifestCost(
		config,
		evidence.Score.Usage,
		evidence.DeploymentServiceTier,
		label,
	)
	if err != nil {
		return 0, err
	}
	if evidence.ManifestRecomputedStageOneCostMicroUSD != recomputedCost {
		return 0, fmt.Errorf(
			"manifest-recomputed Stage 1 cost got %d, want %d",
			evidence.ManifestRecomputedStageOneCostMicroUSD,
			recomputedCost,
		)
	}
	return recomputedCost, nil
}

func validateFinalistSystemEvidence(
	task FinalistTaskRoster,
	evidence FinalistStageOneSystemEvidence,
	byID map[string]FinalistStageOneSystemEvidence,
) (bool, []string, error) {
	for name, value := range map[string]string{
		"results_artifact_sha256":       evidence.ResultsArtifactSHA256,
		"canonical_results_sha256":      evidence.CanonicalResultsSHA256,
		"score_report_artifact_sha256":  evidence.ScoreReportArtifactSHA256,
		"canonical_score_report_sha256": evidence.CanonicalScoreReportSHA256,
	} {
		if err := validateSHA256(name, value); err != nil {
			return false, nil, err
		}
	}
	if err := evidence.DecisionThresholds.validate(); err != nil {
		return false, nil, fmt.Errorf("decision thresholds: %w", err)
	}
	switch evidence.DeploymentServiceTier {
	case PromotionServiceStandard,
		PromotionServiceOpenAIBatch,
		PromotionServiceOpenAIFlex:
	default:
		return false, nil, fmt.Errorf(
			"deployment service tier %q is unsupported",
			evidence.DeploymentServiceTier,
		)
	}
	if evidence.MeasuredStageOneCostMicroUSD < 0 {
		return false, nil, fmt.Errorf(
			"measured_stage1_cost_micro_usd must be non-negative",
		)
	}
	if evidence.ManifestRecomputedStageOneCostMicroUSD < 0 {
		return false, nil, fmt.Errorf(
			"manifest_recomputed_stage1_cost_micro_usd must be non-negative",
		)
	}
	if evidence.Score.CorpusSHA256 != task.TaskDevelopmentCorpusSHA256 ||
		evidence.Score.EvaluatorBuildSHA256 == "" ||
		evidence.Score.System.SystemID != evidence.SystemID {
		return false, nil, fmt.Errorf(
			"score does not bind the exact task development corpus, evaluator, and system",
		)
	}
	// Stage 1 scores are suite-separated, so ScoreReport.Tasks is empty by
	// construction (it is only populated for single-suite reports). Read the
	// per-suite task scores instead, and require every suite to cover exactly
	// this task and nothing else.
	if len(evidence.Score.Suites) == 0 {
		return false, nil, fmt.Errorf("score contains no suites")
	}
	for _, suite := range evidence.Score.Suites {
		if len(suite.Tasks) != 1 || suite.Tasks[0].Task != task.Task {
			return false, nil, fmt.Errorf(
				"score suite %q does not contain exactly task %q",
				suite.Suite,
				task.Task,
			)
		}
	}
	if err := validateFinalistScoreUsage(evidence.Score); err != nil {
		return false, nil, err
	}
	scoreCanonical, err := canonicalJSONIdentity(evidence.Score)
	if err != nil {
		return false, nil, err
	}
	if scoreCanonical != evidence.CanonicalScoreReportSHA256 {
		return false, nil, fmt.Errorf(
			"embedded score report canonical identity mismatch",
		)
	}
	if evidence.MeasuredStageOneCostMicroUSD !=
		evidence.Score.Usage.CostMicroUSD {
		return false, nil, fmt.Errorf(
			"measured Stage 1 cost differs from recomputed score usage",
		)
	}
	if !evidence.Score.Usage.AccountingComplete ||
		evidence.Score.Usage.IncompleteRequests != 0 {
		return false, nil, fmt.Errorf(
			"score usage accounting is incomplete and cannot rank promotion cost",
		)
	}
	if evidence.Score.Usage.InputTokens <= 0 ||
		(evidence.Lane != EvaluationLaneSpecialist &&
			evidence.Score.Usage.OutputTokens <= 0) {
		return false, nil, fmt.Errorf(
			"score usage accounting lacks required positive token dimensions",
		)
	}

	if evidence.Role == PromotionRoleControl {
		if evidence.NormalizedComparison != nil ||
			evidence.ProductionComparison != nil ||
			evidence.Passed {
			return false, nil, fmt.Errorf(
				"control cannot carry candidate comparisons or pass",
			)
		}
		return false, nil, nil
	}
	normalizedControl, exists := byID[evidence.ControlSystemID]
	if !exists || normalizedControl.Role != PromotionRoleControl ||
		normalizedControl.Lane != EvaluationLaneNormalizedStrict {
		return false, nil, fmt.Errorf(
			"normalized comparator %q is missing or invalid",
			evidence.ControlSystemID,
		)
	}
	productionControl, exists := byID[evidence.ProductionControlSystemID]
	if !exists || productionControl.Role != PromotionRoleControl ||
		productionControl.Lane != EvaluationLaneProductionFidelity {
		return false, nil, fmt.Errorf(
			"production comparator %q is missing or invalid",
			evidence.ProductionControlSystemID,
		)
	}
	if evidence.NormalizedComparison == nil ||
		evidence.ProductionComparison == nil {
		return false, nil, fmt.Errorf(
			"candidate must carry both recomputed comparison reports",
		)
	}
	normalizedPass, err := validateFinalistComparisonEvidence(
		task,
		evidence,
		normalizedControl,
		*evidence.NormalizedComparison,
	)
	if err != nil {
		return false, nil, fmt.Errorf("normalized comparison: %w", err)
	}
	productionPass, err := validateFinalistComparisonEvidence(
		task,
		evidence,
		productionControl,
		*evidence.ProductionComparison,
	)
	if err != nil {
		return false, nil, fmt.Errorf("production comparison: %w", err)
	}

	reasons := make([]string, 0, 4)
	if !promotionLaneEligible(evidence.Lane) {
		reasons = append(reasons, "evaluation_lane_not_promotion_eligible")
	}
	if !hasNaturalAndSafetyReports(evidence.Score) {
		reasons = append(reasons, "natural_and_safety_reports_required")
	}
	if !normalizedPass {
		reasons = append(reasons, "normalized_control_gates_not_passed")
	}
	if !productionPass {
		reasons = append(reasons, "production_control_gates_not_passed")
	}
	return len(reasons) == 0, reasons, nil
}

func validateFinalistScoreUsage(score ScoreReport) error {
	if err := validateUsageTotals("score.usage", score.Usage); err != nil {
		return err
	}
	var aggregate UsageTotals
	for suiteIndex, suite := range score.Suites {
		suiteName := fmt.Sprintf("score.suites[%d].usage", suiteIndex)
		if err := validateUsageTotals(suiteName, suite.Usage); err != nil {
			return err
		}
		var taskAggregate UsageTotals
		for taskIndex, task := range suite.Tasks {
			taskName := fmt.Sprintf(
				"score.suites[%d].tasks[%d].usage",
				suiteIndex,
				taskIndex,
			)
			if err := validateUsageTotals(taskName, task.Usage); err != nil {
				return err
			}
			var err error
			taskAggregate, err = addUsageTotals(taskAggregate, task.Usage)
			if err != nil {
				return fmt.Errorf("%s aggregate: %w", suiteName, err)
			}
		}
		if !usageTotalsEqual(taskAggregate, suite.Usage) {
			return fmt.Errorf(
				"%s differs from its task usage aggregate",
				suiteName,
			)
		}
		var err error
		aggregate, err = addUsageTotals(aggregate, suite.Usage)
		if err != nil {
			return fmt.Errorf("score usage aggregate: %w", err)
		}
	}
	if !usageTotalsEqual(aggregate, score.Usage) {
		return fmt.Errorf("score.usage differs from its suite usage aggregate")
	}
	return nil
}

func validateUsageTotals(name string, usage UsageTotals) error {
	if usage.Results < 0 || usage.Requests < 0 ||
		usage.IncompleteRequests < 0 || usage.Requests > usage.Results ||
		usage.IncompleteRequests > usage.Requests {
		return fmt.Errorf("%s has invalid request/result cardinality", name)
	}
	for _, field := range []struct {
		name  string
		value int64
	}{
		{"input_tokens", usage.InputTokens},
		{"cached_input_tokens", usage.CachedInputTokens},
		{"cache_write_tokens", usage.CacheWriteTokens},
		{"output_tokens", usage.OutputTokens},
		{"reasoning_tokens", usage.ReasoningTokens},
		{"total_tokens", usage.TotalTokens},
		{"cost_micro_usd", usage.CostMicroUSD},
	} {
		if field.value < 0 {
			return fmt.Errorf("%s.%s must be non-negative", name, field.name)
		}
	}
	if usage.CachedInputTokens > usage.InputTokens ||
		usage.CacheWriteTokens >
			usage.InputTokens-usage.CachedInputTokens {
		return fmt.Errorf("%s has an invalid input-token partition", name)
	}
	if usage.ReasoningTokens > usage.OutputTokens {
		return fmt.Errorf("%s reasoning_tokens exceeds output_tokens", name)
	}
	totalTokens, err := checkedAddInt64(usage.InputTokens, usage.OutputTokens)
	if err != nil || totalTokens != usage.TotalTokens {
		return fmt.Errorf("%s total_tokens is inconsistent", name)
	}
	wantComplete := usage.Requests > 0 && usage.IncompleteRequests == 0
	if usage.AccountingComplete != wantComplete {
		return fmt.Errorf("%s accounting completeness is inconsistent", name)
	}
	wantCostUSD := float64(usage.CostMicroUSD) / 1_000_000
	if math.Abs(usage.CostUSD-wantCostUSD) > 1e-15 {
		return fmt.Errorf("%s cost_usd is inconsistent", name)
	}
	return nil
}

func addUsageTotals(left, right UsageTotals) (UsageTotals, error) {
	maxInt := int(^uint(0) >> 1)
	if left.Results > maxInt-right.Results ||
		left.Requests > maxInt-right.Requests ||
		left.IncompleteRequests > maxInt-right.IncompleteRequests {
		return UsageTotals{}, fmt.Errorf("cardinality overflow")
	}
	result := UsageTotals{
		Results:            left.Results + right.Results,
		Requests:           left.Requests + right.Requests,
		IncompleteRequests: left.IncompleteRequests + right.IncompleteRequests,
	}
	var err error
	if result.InputTokens, err = checkedAddInt64(left.InputTokens, right.InputTokens); err != nil {
		return UsageTotals{}, err
	}
	if result.CachedInputTokens, err = checkedAddInt64(left.CachedInputTokens, right.CachedInputTokens); err != nil {
		return UsageTotals{}, err
	}
	if result.CacheWriteTokens, err = checkedAddInt64(left.CacheWriteTokens, right.CacheWriteTokens); err != nil {
		return UsageTotals{}, err
	}
	if result.OutputTokens, err = checkedAddInt64(left.OutputTokens, right.OutputTokens); err != nil {
		return UsageTotals{}, err
	}
	if result.ReasoningTokens, err = checkedAddInt64(left.ReasoningTokens, right.ReasoningTokens); err != nil {
		return UsageTotals{}, err
	}
	if result.TotalTokens, err = checkedAddInt64(left.TotalTokens, right.TotalTokens); err != nil {
		return UsageTotals{}, err
	}
	if result.CostMicroUSD, err = checkedAddInt64(left.CostMicroUSD, right.CostMicroUSD); err != nil {
		return UsageTotals{}, err
	}
	result.CostUSD = float64(result.CostMicroUSD) / 1_000_000
	result.AccountingComplete = result.Requests > 0 && result.IncompleteRequests == 0
	return result, nil
}

func usageTotalsEqual(left, right UsageTotals) bool {
	return left.Results == right.Results &&
		left.Requests == right.Requests &&
		left.InputTokens == right.InputTokens &&
		left.CachedInputTokens == right.CachedInputTokens &&
		left.CacheWriteTokens == right.CacheWriteTokens &&
		left.OutputTokens == right.OutputTokens &&
		left.ReasoningTokens == right.ReasoningTokens &&
		left.TotalTokens == right.TotalTokens &&
		left.CostMicroUSD == right.CostMicroUSD &&
		math.Abs(left.CostUSD-right.CostUSD) <= 1e-15 &&
		left.AccountingComplete == right.AccountingComplete &&
		left.IncompleteRequests == right.IncompleteRequests
}

func validateFinalistComparisonEvidence(
	task FinalistTaskRoster,
	candidate,
	control FinalistStageOneSystemEvidence,
	evidence FinalistStageOneComparisonEvidence,
) (bool, error) {
	for name, value := range map[string]string{
		"report_artifact_sha256":  evidence.ReportArtifactSHA256,
		"canonical_report_sha256": evidence.CanonicalReportSHA256,
	} {
		if err := validateSHA256(name, value); err != nil {
			return false, err
		}
	}
	report := evidence.Report
	canonical, err := canonicalJSONIdentity(report)
	if err != nil {
		return false, err
	}
	if canonical != evidence.CanonicalReportSHA256 {
		return false, fmt.Errorf(
			"embedded comparison report canonical identity mismatch",
		)
	}
	if report.SchemaVersion != SchemaVersion ||
		report.CorpusSHA256 != task.TaskDevelopmentCorpusSHA256 ||
		report.EvaluatorBuildSHA256 != candidate.Score.EvaluatorBuildSHA256 ||
		report.Control.SystemID != control.SystemID ||
		report.Candidate.SystemID != candidate.SystemID {
		return false, fmt.Errorf(
			"comparison does not bind the exact task development corpus, evaluator, candidate, and control",
		)
	}
	if report.Options.BootstrapReplicates !=
		task.ComparisonPolicy.BootstrapReplicates ||
		report.Options.BootstrapSeed != task.ComparisonPolicy.BootstrapSeed ||
		!report.Options.ProductionSuiteBinding ||
		report.Options.ClosureBoundHoldout ||
		report.Options.PromotionMode ||
		report.Options.ControlDecisionThresholds !=
			control.DecisionThresholds.runtime() ||
		report.Options.CandidateDecisionThresholds !=
			candidate.DecisionThresholds.runtime() {
		return false, fmt.Errorf(
			"comparison options differ from the frozen Stage 1 roster",
		)
	}
	if report.Method.Confidence != task.ComparisonPolicy.Confidence ||
		report.Method.HarmMargin != task.ComparisonPolicy.HarmMargin ||
		report.Method.SuccessMargin != task.ComparisonPolicy.SuccessMargin ||
		report.Method.SchemaValidFloor !=
			task.ComparisonPolicy.SchemaValidFloor {
		return false, fmt.Errorf(
			"comparison method differs from the frozen Stage 1 gates",
		)
	}
	if len(report.Tasks) != 1 || report.Tasks[0].Task != task.Task {
		return false, fmt.Errorf(
			"comparison does not contain exactly task %q",
			task.Task,
		)
	}
	taskReport := report.Tasks[0]
	if taskReport.PrimaryQualitySuite !=
		task.ComparisonPolicy.PrimarySuite ||
		len(taskReport.SafetySuites) != 1 ||
		taskReport.SafetySuites[0] !=
			task.ComparisonPolicy.SafetySuite {
		return false, fmt.Errorf(
			"comparison natural/safety suite binding differs from the frozen policy",
		)
	}
	primaryCases := 0
	primaryGroups := 0
	safetyCases := 0
	safetyGroups := 0
	for _, suite := range taskReport.Suites {
		switch suite.Suite {
		case task.ComparisonPolicy.PrimarySuite:
			primaryCases = suite.Cases
			primaryGroups = suite.UniqueGroups
		case task.ComparisonPolicy.SafetySuite:
			safetyCases = suite.Cases
			safetyGroups = suite.UniqueGroups
		}
	}
	if primaryCases == 0 || primaryGroups == 0 ||
		safetyCases == 0 || safetyGroups == 0 {
		return false, fmt.Errorf(
			"comparison is missing required natural/safety suite counts",
		)
	}
	recomputedGates := TaskComparisonGates{
		HarmNonInferiority: harmGate(
			taskReport.PrimaryHarm,
		),
		SuccessNonInferiority: successGate(
			taskReport.TaskSuccess,
		),
		SchemaValidity: schemaGate(
			taskReport.SchemaValidity,
			primaryCases,
			primaryGroups,
		),
		ZeroSafetyHarm: safetyGate(
			taskReport.SafetyHarm,
		),
		SafetySuccessNonInferiority: successGate(
			taskReport.SafetyTaskSuccess,
		),
		SafetySchemaValidity: schemaGate(
			taskReport.SafetySchemaValidity,
			safetyCases,
			safetyGroups,
		),
	}
	expectedGatesSHA, err := canonicalJSONIdentity(recomputedGates)
	if err != nil {
		return false, err
	}
	actualGatesSHA, err := canonicalJSONIdentity(taskReport.Gates)
	if err != nil {
		return false, err
	}
	if actualGatesSHA != expectedGatesSHA {
		return false, fmt.Errorf(
			"comparison gate decisions do not match the embedded metrics",
		)
	}
	recomputedVerdict := combineComparisonDecisions(
		recomputedGates.HarmNonInferiority.Decision,
		recomputedGates.SuccessNonInferiority.Decision,
		recomputedGates.SchemaValidity.Decision,
		recomputedGates.ZeroSafetyHarm.Decision,
		recomputedGates.SafetySuccessNonInferiority.Decision,
		recomputedGates.SafetySchemaValidity.Decision,
	)
	if taskReport.DiagnosticVerdict != recomputedVerdict ||
		report.DiagnosticVerdict != recomputedVerdict {
		return false, fmt.Errorf(
			"comparison verdict does not match recomputed gates",
		)
	}
	return recomputedVerdict == ComparisonPass &&
		everyTaskGatePassed(recomputedGates), nil
}

// ReadFinalistRosterManifest strictly decodes and hashes the exact
// pre-holdout finalist declaration.
func ReadFinalistRosterManifest(
	r io.Reader,
	systemManifest SystemManifest,
	systemManifestSHA256 string,
) (FinalistRosterManifest, string, error) {
	if r == nil {
		return FinalistRosterManifest{}, "", fmt.Errorf(
			"finalist roster reader is nil",
		)
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxFinalistRosterBytes+1))
	if err != nil {
		return FinalistRosterManifest{}, "", err
	}
	if len(raw) == 0 || len(raw) > maxFinalistRosterBytes {
		return FinalistRosterManifest{}, "", fmt.Errorf(
			"finalist roster is empty or exceeds %d bytes",
			maxFinalistRosterBytes,
		)
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return FinalistRosterManifest{}, "", fmt.Errorf(
			"decode finalist roster: %w",
			err,
		)
	}
	var manifest FinalistRosterManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return FinalistRosterManifest{}, "", fmt.Errorf(
			"decode finalist roster: %w",
			err,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return FinalistRosterManifest{}, "", fmt.Errorf(
			"decode finalist roster: trailing data",
		)
	}
	if err := manifest.Validate(
		systemManifest,
		systemManifestSHA256,
	); err != nil {
		return FinalistRosterManifest{}, "", err
	}
	return manifest, sha256Hex(raw), nil
}

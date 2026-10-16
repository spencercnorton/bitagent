package llmeval

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
)

const (
	// These are the pre-registered, one-sided non-inferiority gates from the
	// evaluation protocol. They are constants rather than command-line
	// options so an evaluator cannot move a boundary after seeing results.
	ComparisonHarmMargin       = 0.0025
	ComparisonSuccessMargin    = 0.01
	ComparisonSchemaValidFloor = 0.995
	ComparisonConfidence       = 0.95

	defaultBootstrapReplicates = 20_000
	defaultBootstrapSeed       = uint64(20_260_724)
	maxBootstrapReplicates     = 1_000_000
	minimumBootstrapGroups     = 30
	defaultSafetySliceID       = "safety"
	defaultHoldoutSliceID      = "holdout"
)

// ComparisonDecision is the promotion decision for one gate, task, or report.
// A confidence interval that does not clear a boundary is inconclusive unless
// the observed point estimate itself is already outside the allowed margin.
type ComparisonDecision string

const (
	ComparisonPass         ComparisonDecision = "pass"
	ComparisonFail         ComparisonDecision = "fail"
	ComparisonInconclusive ComparisonDecision = "inconclusive"
)

// ComparisonOptions controls only the deterministic computation, not the
// pre-registered decision thresholds. Zero values select stable defaults.
type ComparisonOptions struct {
	BootstrapReplicates         int                `json:"bootstrap_replicates"`
	BootstrapSeed               uint64             `json:"bootstrap_seed"`
	SafetySliceID               string             `json:"safety_slice_id,omitempty"`
	ProductionSuiteBinding      bool               `json:"production_suite_binding"`
	ClosureBoundHoldout         bool               `json:"closure_bound_holdout"`
	PromotionMode               bool               `json:"promotion_mode"`
	HoldoutSliceID              string             `json:"holdout_slice_id,omitempty"`
	EvaluatorBuildSHA256        string             `json:"evaluator_build_sha256"`
	ControlDecisionThresholds   DecisionThresholds `json:"control_decision_thresholds"`
	CandidateDecisionThresholds DecisionThresholds `json:"candidate_decision_thresholds"`
}

// DefaultComparisonOptions returns the reproducible production defaults.
func DefaultComparisonOptions() ComparisonOptions {
	thresholds := ProductionThresholds()
	return ComparisonOptions{
		BootstrapReplicates:         defaultBootstrapReplicates,
		BootstrapSeed:               defaultBootstrapSeed,
		SafetySliceID:               defaultSafetySliceID,
		HoldoutSliceID:              defaultHoldoutSliceID,
		ControlDecisionThresholds:   thresholds,
		CandidateDecisionThresholds: thresholds,
	}
}

// ComparisonMethod records the statistical definitions needed to reproduce
// and correctly interpret a report.
type ComparisonMethod struct {
	DifferenceDirection     string  `json:"difference_direction"`
	BootstrapUnit           string  `json:"bootstrap_unit"`
	BootstrapInterval       string  `json:"bootstrap_interval"`
	Confidence              float64 `json:"confidence"`
	HarmMargin              float64 `json:"harm_margin"`
	SuccessMargin           float64 `json:"success_margin"`
	SchemaValidFloor        float64 `json:"schema_valid_floor"`
	McNemarPValue           string  `json:"mcnemar_p_value"`
	MultiplicityAdjustment  string  `json:"multiplicity_adjustment"`
	NonActionOnError        string  `json:"non_action_on_error"`
	SchemaValidityIndicator string  `json:"schema_validity_indicator"`
}

// BinaryRate retains both the rate and its exact denominator.
type BinaryRate struct {
	Count       int     `json:"count"`
	Denominator int     `json:"denominator"`
	Rate        float64 `json:"rate"`
}

// DiscordanceTable is the paired 2x2 information used by McNemar's test.
// "Positive" means harm for the harm metric and success for the success metric.
type DiscordanceTable struct {
	CandidateOnlyPositive int     `json:"candidate_only_positive"`
	ControlOnlyPositive   int     `json:"control_only_positive"`
	Total                 int     `json:"total"`
	McNemarExactTwoSidedP float64 `json:"mcnemar_exact_two_sided_p"`
}

// ClusterBootstrapBounds contains separate one-sided percentile bounds. The
// lower and upper endpoints are each 95% one-sided bounds; they are not a
// simultaneous two-sided 95% interval.
type ClusterBootstrapBounds struct {
	Method     string  `json:"method"`
	Unit       string  `json:"unit"`
	Replicates int     `json:"replicates"`
	Seed       uint64  `json:"seed"`
	Confidence float64 `json:"confidence"`
	Lower      float64 `json:"lower"`
	Upper      float64 `json:"upper"`
}

// PairedIndicatorComparison compares one binary outcome over exactly the same
// eligible frozen cases for the candidate and control.
type PairedIndicatorComparison struct {
	Name                    string                  `json:"name"`
	Definition              string                  `json:"definition"`
	EligibleCases           int                     `json:"eligible_cases"`
	UniqueGroups            int                     `json:"unique_groups"`
	NonzeroEffectGroups     int                     `json:"nonzero_effect_groups"`
	Control                 BinaryRate              `json:"control"`
	Candidate               BinaryRate              `json:"candidate"`
	CandidateMinusControl   float64                 `json:"candidate_minus_control"`
	Discordance             DiscordanceTable        `json:"discordance"`
	GroupClusterBootstrap95 *ClusterBootstrapBounds `json:"group_cluster_bootstrap_95,omitempty"`
}

// SchemaValidityComparison reports status=ok over all frozen task cases. A
// runtime failure is not called a schema error, but it is also not a
// schema-valid usable response and therefore does not enter the numerator.
type SchemaValidityComparison struct {
	Definition string     `json:"definition"`
	Control    BinaryRate `json:"control"`
	Candidate  BinaryRate `json:"candidate"`
}

// SafetyHarmComparison applies the task's primary harm definition only to
// harm-eligible cases carrying the configured safety slice ID.
type SafetyHarmComparison struct {
	SliceID      string     `json:"slice_id"`
	Definition   string     `json:"definition"`
	UniqueGroups int        `json:"unique_groups"`
	Control      BinaryRate `json:"control"`
	Candidate    BinaryRate `json:"candidate"`
}

// ComparisonGate is one pre-registered promotion gate.
type ComparisonGate struct {
	Decision       ComparisonDecision `json:"decision"`
	Rule           string             `json:"rule"`
	ReasonCode     string             `json:"reason_code"`
	Observed       float64            `json:"observed"`
	OneSidedBound  *float64           `json:"one_sided_bound,omitempty"`
	Threshold      float64            `json:"threshold"`
	EligibleCases  int                `json:"eligible_cases"`
	EligibleGroups int                `json:"eligible_groups"`
	RequiredGroups int                `json:"required_groups,omitempty"`
}

type TaskComparisonGates struct {
	HarmNonInferiority          ComparisonGate `json:"harm_non_inferiority"`
	SuccessNonInferiority       ComparisonGate `json:"success_non_inferiority"`
	SchemaValidity              ComparisonGate `json:"schema_validity"`
	ZeroSafetyHarm              ComparisonGate `json:"zero_safety_harm"`
	SafetySuccessNonInferiority ComparisonGate `json:"safety_success_non_inferiority"`
	SafetySchemaValidity        ComparisonGate `json:"safety_schema_validity"`
}

// SuiteComparison reports quality for exactly one reserved primary suite.
// Natural-prevalence and safety-top-up observations are never combined here.
type SuiteComparison struct {
	Suite          string                    `json:"suite"`
	Cases          int                       `json:"cases"`
	UniqueGroups   int                       `json:"unique_groups"`
	PrimaryHarm    PairedIndicatorComparison `json:"primary_harm"`
	TaskSuccess    PairedIndicatorComparison `json:"task_success"`
	SchemaValidity SchemaValidityComparison  `json:"schema_validity"`
}

// TaskComparison is a paired candidate-versus-control analysis for one task.
type TaskComparison struct {
	Task                 Task                      `json:"task"`
	Cases                int                       `json:"cases"`
	UniqueGroups         int                       `json:"unique_groups"`
	PrimaryQualitySuite  string                    `json:"primary_quality_suite"`
	SafetySuites         []string                  `json:"safety_suites"`
	Suites               []SuiteComparison         `json:"suites"`
	PrimaryHarm          PairedIndicatorComparison `json:"primary_harm"`
	TaskSuccess          PairedIndicatorComparison `json:"task_success"`
	SchemaValidity       SchemaValidityComparison  `json:"schema_validity"`
	SafetyHarm           SafetyHarmComparison      `json:"safety_harm"`
	SafetyTaskSuccess    PairedIndicatorComparison `json:"safety_task_success"`
	SafetySchemaValidity SchemaValidityComparison  `json:"safety_schema_validity"`
	Gates                TaskComparisonGates       `json:"gates"`
	// DiagnosticVerdict is the aggregate statistical result before corpus
	// provenance, holdout, and sample-size promotion requirements are applied.
	DiagnosticVerdict ComparisonDecision `json:"diagnostic_verdict"`
}

// ComparisonReport is the deterministic paired evaluation of one candidate
// against one control over one immutable corpus.
type ComparisonReport struct {
	SchemaVersion        int                  `json:"schema_version"`
	CorpusSHA256         string               `json:"corpus_sha256"`
	EvaluatorBuildSHA256 string               `json:"evaluator_build_sha256"`
	Control              SystemDescriptor     `json:"control"`
	Candidate            SystemDescriptor     `json:"candidate"`
	ControlCampaign      *CampaignRunBinding  `json:"control_campaign,omitempty"`
	CandidateCampaign    *CampaignRunBinding  `json:"candidate_campaign,omitempty"`
	Options              ComparisonOptions    `json:"options"`
	Method               ComparisonMethod     `json:"method"`
	Tasks                []TaskComparison     `json:"tasks"`
	DiagnosticVerdict    ComparisonDecision   `json:"diagnostic_verdict"`
	Promotion            PromotionEligibility `json:"promotion"`
}

type pairedObservation struct {
	groupID   string
	control   bool
	candidate bool
}

type caseIndicators struct {
	harmEligible    bool
	harm            bool
	successEligible bool
	success         bool
}

type comparisonTaskAccumulator struct {
	task              Task
	suite             string
	cases             int
	groups            map[string]struct{}
	harm              []pairedObservation
	success           []pairedObservation
	schema            []pairedObservation
	safetyHarm        []pairedObservation
	allCaseSuccess    []pairedObservation
	safetySuccess     []pairedObservation
	safetySchema      []pairedObservation
	harmDefinition    string
	successDefinition string
}

// Compare performs a strict paired comparison. Each result set must contain
// exactly one result for every case in the same frozen corpus. Missing,
// additional, duplicate, mixed-system, mismatched-task, or mismatched-corpus
// results are rejected instead of being silently treated as failures.
func Compare(
	corpus Corpus,
	controlResults []ResultRecord,
	candidateResults []ResultRecord,
	options ComparisonOptions,
) (ComparisonReport, error) {
	thresholds := ProductionThresholds()
	return CompareWithThresholds(
		corpus,
		controlResults,
		candidateResults,
		options,
		thresholds,
		thresholds,
	)
}

// CompareWithThresholds performs the paired comparison after validating each
// side against its own frozen run/roster thresholds. This prevents a
// structurally plausible normalized action from being reinterpreted under the
// default threshold during promotion.
func CompareWithThresholds(
	corpus Corpus,
	controlResults []ResultRecord,
	candidateResults []ResultRecord,
	options ComparisonOptions,
	controlThresholds DecisionThresholds,
	candidateThresholds DecisionThresholds,
) (ComparisonReport, error) {
	if err := controlThresholds.Validate(); err != nil {
		return ComparisonReport{}, fmt.Errorf(
			"control decision thresholds: %w",
			err,
		)
	}
	if err := candidateThresholds.Validate(); err != nil {
		return ComparisonReport{}, fmt.Errorf(
			"candidate decision thresholds: %w",
			err,
		)
	}
	canonical, err := NewCorpus(corpus.Records)
	if err != nil {
		return ComparisonReport{}, fmt.Errorf("corpus: %w", err)
	}
	if corpus.SHA256 != "" && corpus.SHA256 != canonical.SHA256 {
		return ComparisonReport{}, fmt.Errorf(
			"corpus SHA-256 mismatch: provided %s, computed %s",
			corpus.SHA256,
			canonical.SHA256,
		)
	}
	if err := ValidateGoldCorpus(canonical); err != nil {
		return ComparisonReport{}, err
	}

	options, err = normalizeComparisonOptions(options)
	if err != nil {
		return ComparisonReport{}, err
	}
	options.ControlDecisionThresholds = controlThresholds
	options.CandidateDecisionThresholds = candidateThresholds
	control, controlByCase, err := alignComparisonResults(
		"control",
		canonical,
		controlResults,
		options.EvaluatorBuildSHA256,
		controlThresholds,
	)
	if err != nil {
		return ComparisonReport{}, err
	}
	candidate, candidateByCase, err := alignComparisonResults(
		"candidate",
		canonical,
		candidateResults,
		options.EvaluatorBuildSHA256,
		candidateThresholds,
	)
	if err != nil {
		return ComparisonReport{}, err
	}
	if controlResults[0].ExecutionAudit.ManifestSHA256 !=
		candidateResults[0].ExecutionAudit.ManifestSHA256 {
		return ComparisonReport{}, fmt.Errorf(
			"control and candidate results use different manifest identities",
		)
	}
	controlCampaign := controlResults[0].ExecutionAudit.Campaign
	candidateCampaign := candidateResults[0].ExecutionAudit.Campaign
	if (controlCampaign == nil) != (candidateCampaign == nil) {
		return ComparisonReport{}, fmt.Errorf(
			"control and candidate must either both be campaign-bound or both be non-campaign evidence",
		)
	}
	if controlCampaign != nil {
		if controlCampaign.CampaignSHA256 != candidateCampaign.CampaignSHA256 ||
			controlCampaign.CampaignID != candidateCampaign.CampaignID ||
			controlCampaign.Stage != candidateCampaign.Stage {
			return ComparisonReport{}, fmt.Errorf(
				"control and candidate use different campaign identities",
			)
		}
		if controlCampaign.Role != CampaignSystemRoleControl ||
			candidateCampaign.Role != CampaignSystemRoleCandidate {
			return ComparisonReport{}, fmt.Errorf(
				"campaign comparison requires a control run and a candidate run",
			)
		}
	}
	if control == candidate {
		return ComparisonReport{}, fmt.Errorf(
			"control and candidate identify the same system %q",
			control.SystemID,
		)
	}

	accumulators := make(
		map[Task]map[string]*comparisonTaskAccumulator,
	)
	for _, record := range canonical.Records {
		suite, err := scoringPrimarySuite(record)
		if err != nil {
			return ComparisonReport{}, fmt.Errorf(
				"case %q: %w",
				record.CaseID,
				err,
			)
		}
		taskAccs := accumulators[record.Task]
		if taskAccs == nil {
			taskAccs = make(map[string]*comparisonTaskAccumulator)
			accumulators[record.Task] = taskAccs
		}
		acc := taskAccs[suite]
		if acc == nil {
			harmDefinition, successDefinition := comparisonDefinitions(record.Task)
			acc = &comparisonTaskAccumulator{
				task:              record.Task,
				suite:             suite,
				groups:            make(map[string]struct{}),
				harmDefinition:    harmDefinition,
				successDefinition: successDefinition,
			}
			taskAccs[suite] = acc
		}
		acc.cases++
		acc.groups[record.GroupID] = struct{}{}

		controlResult := controlByCase[record.CaseID]
		candidateResult := candidateByCase[record.CaseID]
		controlIndicators := indicatorsForCase(record, controlResult)
		candidateIndicators := indicatorsForCase(record, candidateResult)

		if controlIndicators.harmEligible != candidateIndicators.harmEligible ||
			controlIndicators.successEligible != candidateIndicators.successEligible {
			return ComparisonReport{}, fmt.Errorf(
				"case %q: internal eligibility mismatch",
				record.CaseID,
			)
		}
		if controlIndicators.harmEligible {
			observation := pairedObservation{
				groupID:   record.GroupID,
				control:   controlIndicators.harm,
				candidate: candidateIndicators.harm,
			}
			acc.harm = append(acc.harm, observation)
			if !options.ProductionSuiteBinding &&
				hasSlice(record.SliceIDs, options.SafetySliceID) {
				acc.safetyHarm = append(acc.safetyHarm, observation)
			}
		}
		if controlIndicators.successEligible {
			acc.success = append(acc.success, pairedObservation{
				groupID:   record.GroupID,
				control:   controlIndicators.success,
				candidate: candidateIndicators.success,
			})
		}
		safetySuccessObservation := pairedObservation{
			groupID:   record.GroupID,
			control:   goldOutcomeSuccess(record, controlResult),
			candidate: goldOutcomeSuccess(record, candidateResult),
		}
		acc.allCaseSuccess = append(
			acc.allCaseSuccess,
			safetySuccessObservation,
		)
		schemaObservation := pairedObservation{
			groupID:   record.GroupID,
			control:   controlResult.Status == ResultStatusOK,
			candidate: candidateResult.Status == ResultStatusOK,
		}
		acc.schema = append(acc.schema, schemaObservation)
		if !options.ProductionSuiteBinding &&
			hasSlice(record.SliceIDs, options.SafetySliceID) {
			acc.safetySuccess = append(
				acc.safetySuccess,
				safetySuccessObservation,
			)
			acc.safetySchema = append(acc.safetySchema, schemaObservation)
		}
	}

	report := ComparisonReport{
		SchemaVersion:        SchemaVersion,
		CorpusSHA256:         canonical.SHA256,
		EvaluatorBuildSHA256: options.EvaluatorBuildSHA256,
		Control:              control,
		Candidate:            candidate,
		ControlCampaign:      cloneCampaignRunBinding(controlCampaign),
		CandidateCampaign:    cloneCampaignRunBinding(candidateCampaign),
		Options:              options,
		Method: ComparisonMethod{
			DifferenceDirection:     "candidate_rate_minus_control_rate",
			BootstrapUnit:           "group_id",
			BootstrapInterval:       "deterministic_percentile; separate one-sided bounds",
			Confidence:              ComparisonConfidence,
			HarmMargin:              ComparisonHarmMargin,
			SuccessMargin:           ComparisonSuccessMargin,
			SchemaValidFloor:        ComparisonSchemaValidFloor,
			McNemarPValue:           "exact two-sided binomial on discordant paired cases",
			MultiplicityAdjustment:  "none in this pairwise report; apply Holm within each task across candidates",
			NonActionOnError:        "schema/runtime errors cannot directly count as destructive harm; they count as unsuccessful and schema-invalid",
			SchemaValidityIndicator: "result status is ok",
		},
		DiagnosticVerdict: ComparisonPass,
	}

	for _, task := range orderedTasks {
		taskAccs := accumulators[task]
		if len(taskAccs) == 0 {
			continue
		}
		taskComparison, err := finalizeTaskComparison(taskAccs, options)
		if err != nil {
			return ComparisonReport{}, fmt.Errorf(
				"task %q: %w",
				task,
				err,
			)
		}
		report.Tasks = append(report.Tasks, taskComparison)
		report.DiagnosticVerdict = combineComparisonDecisions(
			report.DiagnosticVerdict,
			taskComparison.DiagnosticVerdict,
		)
	}
	report.Promotion = assessPromotionEligibility(canonical, report.Tasks, options)
	report.Promotion.Decision = promotionSafeDecision(
		report.DiagnosticVerdict,
		report.Promotion.Eligible,
	)

	return report, nil
}

func normalizeComparisonOptions(options ComparisonOptions) (ComparisonOptions, error) {
	defaults := DefaultComparisonOptions()
	if options.BootstrapReplicates == 0 {
		options.BootstrapReplicates = defaults.BootstrapReplicates
	}
	if options.BootstrapSeed == 0 {
		options.BootstrapSeed = defaults.BootstrapSeed
	}
	if options.ProductionSuiteBinding {
		options.SafetySliceID = ""
		options.HoldoutSliceID = ""
	} else {
		if options.SafetySliceID == "" {
			options.SafetySliceID = defaults.SafetySliceID
		}
		if options.HoldoutSliceID == "" {
			options.HoldoutSliceID = defaults.HoldoutSliceID
		}
	}
	if err := validateSHA256(
		"evaluator_build_sha256",
		options.EvaluatorBuildSHA256,
	); err != nil {
		return ComparisonOptions{}, err
	}
	if options.BootstrapReplicates < 1_000 ||
		options.BootstrapReplicates > maxBootstrapReplicates {
		return ComparisonOptions{}, fmt.Errorf(
			"bootstrap_replicates must be between 1000 and %d",
			maxBootstrapReplicates,
		)
	}
	if options.SafetySliceID != "" {
		if err := validateIdentifier("safety_slice_id", options.SafetySliceID); err != nil {
			return ComparisonOptions{}, err
		}
	}
	if options.HoldoutSliceID != "" {
		if err := validateIdentifier("holdout_slice_id", options.HoldoutSliceID); err != nil {
			return ComparisonOptions{}, err
		}
	}
	return options, nil
}

func alignComparisonResults(
	role string,
	corpus Corpus,
	results []ResultRecord,
	evaluatorBuildSHA256 string,
	thresholds DecisionThresholds,
) (SystemDescriptor, map[string]ResultRecord, error) {
	if len(results) != len(corpus.Records) {
		return SystemDescriptor{}, nil, fmt.Errorf(
			"%s results: expected exactly %d frozen cases, got %d",
			role,
			len(corpus.Records),
			len(results),
		)
	}
	if err := ValidateResultsWithThresholds(results, thresholds); err != nil {
		return SystemDescriptor{}, nil, fmt.Errorf("%s results: %w", role, err)
	}

	system := results[0].System
	cases := make(map[string]CorpusRecord, len(corpus.Records))
	for _, record := range corpus.Records {
		cases[record.CaseID] = record
	}
	byCase := make(map[string]ResultRecord, len(results))
	for i, result := range results {
		if result.EvaluatorBuildSHA256 != evaluatorBuildSHA256 {
			return SystemDescriptor{}, nil, fmt.Errorf(
				"%s result %d case %q belongs to a different evaluator build",
				role,
				i+1,
				result.CaseID,
			)
		}
		if result.System != system {
			return SystemDescriptor{}, nil, fmt.Errorf(
				"%s result %d: mixed systems %q and %q",
				role,
				i+1,
				system.SystemID,
				result.System.SystemID,
			)
		}
		if result.CorpusSHA256 != corpus.SHA256 {
			return SystemDescriptor{}, nil, fmt.Errorf(
				"%s result %d case %q: corpus_sha256 %s does not match %s",
				role,
				i+1,
				result.CaseID,
				result.CorpusSHA256,
				corpus.SHA256,
			)
		}
		record, exists := cases[result.CaseID]
		if !exists {
			return SystemDescriptor{}, nil, fmt.Errorf(
				"%s result %d: unknown case_id %q",
				role,
				i+1,
				result.CaseID,
			)
		}
		if result.Task != record.Task {
			return SystemDescriptor{}, nil, fmt.Errorf(
				"%s result %d case %q: task %q does not match corpus task %q",
				role,
				i+1,
				result.CaseID,
				result.Task,
				record.Task,
			)
		}
		if err := validateResultForCase(record, result); err != nil {
			return SystemDescriptor{}, nil, fmt.Errorf(
				"%s result %d case %q: %w",
				role,
				i+1,
				result.CaseID,
				err,
			)
		}
		byCase[result.CaseID] = result
	}
	for _, record := range corpus.Records {
		if _, exists := byCase[record.CaseID]; !exists {
			return SystemDescriptor{}, nil, fmt.Errorf(
				"%s results: missing frozen case %q",
				role,
				record.CaseID,
			)
		}
	}
	return system, byCase, nil
}

func indicatorsForCase(record CorpusRecord, result ResultRecord) caseIndicators {
	switch record.Task {
	case TaskMatcherExtract:
		required := len(record.MatcherExtract.Expected.Acceptable) > 0 &&
			!record.MatcherExtract.Expected.AllowAbstain
		if result.Status != ResultStatusOK {
			return caseIndicators{harmEligible: true, successEligible: required}
		}
		if result.MatcherExtract.Action == MatcherExtractActionAbstain {
			return caseIndicators{harmEligible: true, successEligible: required}
		}
		matched := false
		for _, acceptable := range record.MatcherExtract.Expected.Acceptable {
			if extractionMatchesNormalized(
				*result.MatcherExtract.Extraction,
				acceptable,
			) {
				matched = true
				break
			}
		}
		return caseIndicators{
			harmEligible:    true,
			harm:            !matched,
			successEligible: required,
			success:         required && matched,
		}

	case TaskMatcherRerank:
		required := len(record.MatcherRerank.Expected.AcceptableTMDBIDs) > 0 &&
			!record.MatcherRerank.Expected.AllowAbstain
		if result.Status != ResultStatusOK {
			return caseIndicators{harmEligible: true, successEligible: required}
		}
		if result.MatcherRerank.Action == MatcherRerankActionAbstain {
			return caseIndicators{harmEligible: true, successEligible: required}
		}
		matched := false
		for _, acceptable := range record.MatcherRerank.Expected.AcceptableTMDBIDs {
			if result.MatcherRerank.TMDBID == acceptable {
				matched = true
				break
			}
		}
		return caseIndicators{
			harmEligible:    true,
			harm:            !matched,
			successEligible: required,
			success:         required && matched,
		}

	case TaskContentFilter:
		if record.ContentFilter.Expected.AllowAbstain {
			return caseIndicators{
				successEligible: true,
				success: result.Status == ResultStatusOK &&
					result.ContentFilter.Action == ContentFilterActionAbstain,
			}
		}
		isEnglish := record.ContentFilter.Expected.Language == LanguageEnglish
		if result.Status != ResultStatusOK {
			return caseIndicators{
				harmEligible:    isEnglish,
				successEligible: true,
			}
		}
		harm := isEnglish && result.ContentFilter.Action == ContentFilterActionDrop
		success := (isEnglish && result.ContentFilter.Action == ContentFilterActionKeep) ||
			(!isEnglish && result.ContentFilter.Action == ContentFilterActionDrop)
		return caseIndicators{
			harmEligible:    isEnglish,
			harm:            harm,
			successEligible: true,
			success:         success,
		}

	case TaskJunkPurge:
		if record.JunkPurge.Expected.Disposition == JunkDispositionAbstain {
			return caseIndicators{
				successEligible: true,
				success: result.Status == ResultStatusOK &&
					result.JunkPurge.Action == JunkPurgeActionAbstain,
			}
		}
		// keep is the harm denominator, and the ONLY harm denominator. It was
		// previously "the expected verdict is real_mangled or real_absent",
		// i.e. cataloguedness — which made a correctly-deleted live sport
		// broadcast score as harm.
		keep := record.JunkPurge.Expected.Disposition == JunkDispositionKeep
		if result.Status != ResultStatusOK {
			return caseIndicators{
				harmEligible:    keep,
				successEligible: true,
			}
		}
		harm := keep && result.JunkPurge.Action == JunkPurgeActionJunk
		success := (keep && result.JunkPurge.Action == JunkPurgeActionKeep) ||
			(!keep && result.JunkPurge.Action == JunkPurgeActionJunk)
		return caseIndicators{
			harmEligible:    keep,
			harm:            harm,
			successEligible: true,
			success:         success,
		}
	default:
		panic(fmt.Sprintf("unsupported validated task %q", record.Task))
	}
}

// goldOutcomeSuccess scores every safety-suite case against its reviewed gold,
// including cases where abstention is the gold-success action. Unlike the
// natural matcher success metric, it has no "required-action only" denominator:
// wholesale non-action on a safety suite must remain visible.
func goldOutcomeSuccess(record CorpusRecord, result ResultRecord) bool {
	if result.Status != ResultStatusOK {
		return false
	}
	switch record.Task {
	case TaskMatcherExtract:
		if result.MatcherExtract.Action == MatcherExtractActionAbstain {
			return record.MatcherExtract.Expected.AllowAbstain
		}
		for _, acceptable := range record.MatcherExtract.Expected.Acceptable {
			if extractionMatchesNormalized(
				*result.MatcherExtract.Extraction,
				acceptable,
			) {
				return true
			}
		}
		return false
	case TaskMatcherRerank:
		if result.MatcherRerank.Action == MatcherRerankActionAbstain {
			return record.MatcherRerank.Expected.AllowAbstain
		}
		for _, acceptable := range record.MatcherRerank.Expected.AcceptableTMDBIDs {
			if result.MatcherRerank.TMDBID == acceptable {
				return true
			}
		}
		return false
	case TaskContentFilter:
		if record.ContentFilter.Expected.AllowAbstain {
			return result.ContentFilter.Action == ContentFilterActionAbstain
		}
		if record.ContentFilter.Expected.Language == LanguageEnglish {
			return result.ContentFilter.Action == ContentFilterActionKeep
		}
		return result.ContentFilter.Action == ContentFilterActionDrop
	case TaskJunkPurge:
		switch record.JunkPurge.Expected.Disposition {
		case JunkDispositionAbstain:
			return result.JunkPurge.Action == JunkPurgeActionAbstain
		case JunkDispositionKeep:
			return result.JunkPurge.Action == JunkPurgeActionKeep
		case JunkDispositionDelete:
			return result.JunkPurge.Action == JunkPurgeActionJunk
		default:
			panic(fmt.Sprintf(
				"unsupported validated junk disposition %q",
				record.JunkPurge.Expected.Disposition,
			))
		}
	default:
		panic(fmt.Sprintf("unsupported validated task %q", record.Task))
	}
}

func comparisonDefinitions(task Task) (harm, success string) {
	switch task {
	case TaskMatcherExtract:
		return "an emitted extraction does not production-normalize to one gold acceptable identity with all non-title fields equal", "an extraction-required case emits an identity that production-normalizes to one gold acceptable identity with all non-title fields equal"
	case TaskMatcherRerank:
		return "an attachment selects no gold acceptable TMDB identity", "an attachment-required case selects a gold acceptable TMDB identity"
	case TaskContentFilter:
		return "a gold English must-keep title is dropped", "the case is kept when English, dropped when non-English, or abstained when gold is uncertain"
	case TaskJunkPurge:
		return "a gold keep-disposition case is deleted", "a gold keep-disposition case is kept, a gold delete-disposition case is deleted, or a gold abstain-disposition case is abstained"
	default:
		panic(fmt.Sprintf("unsupported validated task %q", task))
	}
}

func safetySuccessDefinition(task Task) string {
	switch task {
	case TaskMatcherExtract:
		return "every safety case emits a gold acceptable normalized identity or abstains only when reviewed gold allows abstention"
	case TaskMatcherRerank:
		return "every safety case attaches a gold acceptable identity or abstains only when reviewed gold allows abstention"
	case TaskContentFilter:
		return "every safety case is kept when gold English, dropped when gold non-English, or abstained when reviewed gold is uncertain"
	case TaskJunkPurge:
		return "every safety case keeps a gold keep-disposition title, deletes a gold delete-disposition title, or abstains when reviewed gold derives an abstain disposition"
	default:
		panic(fmt.Sprintf("unsupported validated task %q", task))
	}
}

func finalizeTaskComparison(
	accs map[string]*comparisonTaskAccumulator,
	options ComparisonOptions,
) (TaskComparison, error) {
	suiteNames := make([]string, 0, len(accs))
	for suite := range accs {
		suiteNames = append(suiteNames, suite)
	}
	sort.Strings(suiteNames)
	if len(suiteNames) == 0 {
		return TaskComparison{}, fmt.Errorf("has no primary suites")
	}
	primarySuite := suiteNames[0]
	if _, exists := accs["natural"]; exists {
		primarySuite = "natural"
	} else if options.ProductionSuiteBinding && len(suiteNames) != 1 {
		return TaskComparison{}, fmt.Errorf(
			"production-bound comparison has no natural suite",
		)
	}
	primary := accs[primarySuite]

	var (
		cases         int
		allGroups     = make(map[string]struct{})
		safetySuites  []string
		safetyHarm    []pairedObservation
		safetySuccess []pairedObservation
		safetySchema  []pairedObservation
		suites        []SuiteComparison
	)
	for _, suite := range suiteNames {
		acc := accs[suite]
		cases += acc.cases
		for group := range acc.groups {
			allGroups[group] = struct{}{}
		}
		suites = append(suites, finalizeSuiteComparison(acc, options))
		if suite != primarySuite {
			safetySuites = append(safetySuites, suite)
			safetyHarm = append(safetyHarm, acc.harm...)
			safetySuccess = append(safetySuccess, acc.allCaseSuccess...)
			safetySchema = append(safetySchema, acc.schema...)
		}
	}
	if options.ProductionSuiteBinding && len(safetySuites) != 1 {
		return TaskComparison{}, fmt.Errorf(
			"production-bound comparison requires exactly one separate safety suite, got %d",
			len(safetySuites),
		)
	}
	if !options.ProductionSuiteBinding && len(safetySuites) == 0 {
		safetyHarm = append(safetyHarm, primary.safetyHarm...)
		safetySuccess = append(safetySuccess, primary.safetySuccess...)
		safetySchema = append(safetySchema, primary.safetySchema...)
		if len(safetyHarm) > 0 ||
			len(safetySuccess) > 0 ||
			len(safetySchema) > 0 {
			safetySuites = []string{"legacy_slice:" + options.SafetySliceID}
		}
	}

	harm := summarizePairedIndicator(
		"primary_harm",
		primary.harmDefinition,
		primary.harm,
		options,
		string(primary.task)+":"+primarySuite+":harm",
	)
	success := summarizePairedIndicator(
		"task_success",
		primary.successDefinition,
		primary.success,
		options,
		string(primary.task)+":"+primarySuite+":success",
	)
	schema := SchemaValidityComparison{
		Definition: "result status is ok; schema/runtime failures are not usable schema-valid responses",
		Control:    summarizeBinary(primary.schema, false),
		Candidate:  summarizeBinary(primary.schema, true),
	}
	safety := SafetyHarmComparison{
		SliceID:      strings.Join(safetySuites, ","),
		Definition:   primary.harmDefinition,
		UniqueGroups: countObservationGroups(safetyHarm),
		Control:      summarizeBinary(safetyHarm, false),
		Candidate:    summarizeBinary(safetyHarm, true),
	}
	safetyTaskSuccess := summarizePairedIndicator(
		"safety_task_success",
		safetySuccessDefinition(primary.task),
		safetySuccess,
		options,
		string(primary.task)+":"+strings.Join(safetySuites, ",")+
			":safety-success",
	)
	safetySchemaValidity := SchemaValidityComparison{
		Definition: "result status is ok on the safety suite; schema/runtime failures are not usable schema-valid responses",
		Control:    summarizeBinary(safetySchema, false),
		Candidate:  summarizeBinary(safetySchema, true),
	}
	gates := TaskComparisonGates{
		HarmNonInferiority:          harmGate(harm),
		SuccessNonInferiority:       successGate(success),
		SchemaValidity:              schemaGate(schema, len(primary.schema), len(primary.groups)),
		ZeroSafetyHarm:              safetyGate(safety),
		SafetySuccessNonInferiority: successGate(safetyTaskSuccess),
		SafetySchemaValidity: schemaGate(
			safetySchemaValidity,
			len(safetySchema),
			countObservationGroups(safetySchema),
		),
	}
	verdict := combineComparisonDecisions(
		gates.HarmNonInferiority.Decision,
		gates.SuccessNonInferiority.Decision,
		gates.SchemaValidity.Decision,
		gates.ZeroSafetyHarm.Decision,
		gates.SafetySuccessNonInferiority.Decision,
		gates.SafetySchemaValidity.Decision,
	)
	return TaskComparison{
		Task:                 primary.task,
		Cases:                cases,
		UniqueGroups:         len(allGroups),
		PrimaryQualitySuite:  primarySuite,
		SafetySuites:         safetySuites,
		Suites:               suites,
		PrimaryHarm:          harm,
		TaskSuccess:          success,
		SchemaValidity:       schema,
		SafetyHarm:           safety,
		SafetyTaskSuccess:    safetyTaskSuccess,
		SafetySchemaValidity: safetySchemaValidity,
		Gates:                gates,
		DiagnosticVerdict:    verdict,
	}, nil
}

func finalizeSuiteComparison(
	acc *comparisonTaskAccumulator,
	options ComparisonOptions,
) SuiteComparison {
	return SuiteComparison{
		Suite:        acc.suite,
		Cases:        acc.cases,
		UniqueGroups: len(acc.groups),
		PrimaryHarm: summarizePairedIndicator(
			"primary_harm",
			acc.harmDefinition,
			acc.harm,
			options,
			string(acc.task)+":"+acc.suite+":suite-harm",
		),
		TaskSuccess: summarizePairedIndicator(
			"task_success",
			acc.successDefinition,
			acc.success,
			options,
			string(acc.task)+":"+acc.suite+":suite-success",
		),
		SchemaValidity: SchemaValidityComparison{
			Definition: "result status is ok; schema/runtime failures are not usable schema-valid responses",
			Control:    summarizeBinary(acc.schema, false),
			Candidate:  summarizeBinary(acc.schema, true),
		},
	}
}

func summarizePairedIndicator(
	name string,
	definition string,
	observations []pairedObservation,
	options ComparisonOptions,
	seedDomain string,
) PairedIndicatorComparison {
	control := summarizeBinary(observations, false)
	candidate := summarizeBinary(observations, true)
	candidateOnly, controlOnly := discordances(observations)
	summary := PairedIndicatorComparison{
		Name:                  name,
		Definition:            definition,
		EligibleCases:         len(observations),
		UniqueGroups:          countObservationGroups(observations),
		NonzeroEffectGroups:   countNonzeroGroupEffects(observations),
		Control:               control,
		Candidate:             candidate,
		CandidateMinusControl: candidate.Rate - control.Rate,
		Discordance: DiscordanceTable{
			CandidateOnlyPositive: candidateOnly,
			ControlOnlyPositive:   controlOnly,
			Total:                 candidateOnly + controlOnly,
			McNemarExactTwoSidedP: ExactMcNemarTwoSided(candidateOnly, controlOnly),
		},
	}
	if len(observations) > 0 {
		seed := deriveBootstrapSeed(options.BootstrapSeed, seedDomain)
		lower, upper := clusterBootstrapBounds(
			observations,
			options.BootstrapReplicates,
			seed,
		)
		summary.GroupClusterBootstrap95 = &ClusterBootstrapBounds{
			Method:     "percentile",
			Unit:       "group_id",
			Replicates: options.BootstrapReplicates,
			Seed:       seed,
			Confidence: ComparisonConfidence,
			Lower:      lower,
			Upper:      upper,
		}
	}
	return summary
}

func summarizeBinary(observations []pairedObservation, candidate bool) BinaryRate {
	count := 0
	for _, observation := range observations {
		value := observation.control
		if candidate {
			value = observation.candidate
		}
		if value {
			count++
		}
	}
	rate := 0.0
	if len(observations) > 0 {
		rate = float64(count) / float64(len(observations))
	}
	return BinaryRate{Count: count, Denominator: len(observations), Rate: rate}
}

func discordances(observations []pairedObservation) (candidateOnly, controlOnly int) {
	for _, observation := range observations {
		switch {
		case observation.candidate && !observation.control:
			candidateOnly++
		case observation.control && !observation.candidate:
			controlOnly++
		}
	}
	return candidateOnly, controlOnly
}

// ExactMcNemarTwoSided returns the conventional exact two-sided McNemar
// p-value: twice the lower binomial(0.5) tail at the smaller discordant count,
// clipped at one. Log-space summation remains stable for large holdouts.
func ExactMcNemarTwoSided(candidateOnlyPositive, controlOnlyPositive int) float64 {
	if candidateOnlyPositive < 0 || controlOnlyPositive < 0 {
		return math.NaN()
	}
	n := candidateOnlyPositive + controlOnlyPositive
	if n == 0 {
		return 1
	}
	k := candidateOnlyPositive
	if controlOnlyPositive < k {
		k = controlOnlyPositive
	}

	logSum := math.Inf(-1)
	for i := 0; i <= k; i++ {
		logProbability := logBinomialCoefficient(n, i) - float64(n)*math.Ln2
		logSum = logAddExp(logSum, logProbability)
	}
	p := 2 * math.Exp(logSum)
	if p > 1 {
		return 1
	}
	return p
}

func logBinomialCoefficient(n, k int) float64 {
	numerator, _ := math.Lgamma(float64(n + 1))
	left, _ := math.Lgamma(float64(k + 1))
	right, _ := math.Lgamma(float64(n - k + 1))
	return numerator - left - right
}

func logAddExp(a, b float64) float64 {
	if math.IsInf(a, -1) {
		return b
	}
	if a < b {
		a, b = b, a
	}
	return a + math.Log1p(math.Exp(b-a))
}

type clusterAggregate struct {
	eligible  int
	control   int
	candidate int
}

func clusterBootstrapBounds(
	observations []pairedObservation,
	replicates int,
	seed uint64,
) (float64, float64) {
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
	allGroupDifferencesZero := true
	for _, groupID := range groupIDs {
		aggregate := byGroup[groupID]
		groups = append(groups, aggregate)
		if aggregate.candidate != aggregate.control {
			allGroupDifferencesZero = false
		}
	}
	if allGroupDifferencesZero {
		return 0, 0
	}

	rng := splitMix64{state: seed}
	differences := make([]float64, replicates)
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
		differences[replicate] =
			float64(candidate)/float64(eligible) -
				float64(control)/float64(eligible)
	}
	sort.Float64s(differences)
	alpha := 1 - ComparisonConfidence
	return percentile(differences, alpha), percentile(differences, ComparisonConfidence)
}

func percentile(sortedValues []float64, probability float64) float64 {
	if len(sortedValues) == 1 {
		return sortedValues[0]
	}
	position := probability * float64(len(sortedValues)-1)
	lowerIndex := int(math.Floor(position))
	upperIndex := int(math.Ceil(position))
	if lowerIndex == upperIndex {
		return sortedValues[lowerIndex]
	}
	weight := position - float64(lowerIndex)
	return sortedValues[lowerIndex]*(1-weight) + sortedValues[upperIndex]*weight
}

type splitMix64 struct {
	state uint64
}

func (r *splitMix64) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	value := r.state
	value = (value ^ (value >> 30)) * 0xbf58476d1ce4e5b9
	value = (value ^ (value >> 27)) * 0x94d049bb133111eb
	return value ^ (value >> 31)
}

func (r *splitMix64) intn(n int) int {
	if n <= 0 {
		panic("splitMix64.intn called with a non-positive bound")
	}
	bound := uint64(n)
	threshold := -bound % bound
	for {
		value := r.next()
		if value >= threshold {
			return int(value % bound)
		}
	}
}

func deriveBootstrapSeed(base uint64, domain string) uint64 {
	digest := sha256.Sum256([]byte(domain))
	return base ^ binary.LittleEndian.Uint64(digest[:8])
}

func harmGate(metric PairedIndicatorComparison) ComparisonGate {
	gate := ComparisonGate{
		Rule:           "one-sided 95% upper bound(candidate harm rate - control harm rate) <= 0.0025",
		Threshold:      ComparisonHarmMargin,
		Observed:       metric.CandidateMinusControl,
		EligibleCases:  metric.EligibleCases,
		EligibleGroups: metric.UniqueGroups,
	}
	if metric.EligibleCases == 0 {
		gate.Decision = ComparisonInconclusive
		gate.ReasonCode = "insufficient_eligible_cases"
		return gate
	}
	if metric.CandidateMinusControl > ComparisonHarmMargin+1e-12 {
		gate.Decision = ComparisonFail
		gate.ReasonCode = "point_estimate_outside_margin"
		gate.OneSidedBound = floatPointer(metric.GroupClusterBootstrap95.Upper)
		return gate
	}
	if reason, required := underpoweredReason(metric, ComparisonHarmMargin); reason != "" {
		gate.Decision = ComparisonInconclusive
		gate.ReasonCode = reason
		gate.RequiredGroups = required
		gate.OneSidedBound = floatPointer(metric.GroupClusterBootstrap95.Upper)
		return gate
	}
	gate.OneSidedBound = floatPointer(metric.GroupClusterBootstrap95.Upper)
	if metric.GroupClusterBootstrap95.Upper <= ComparisonHarmMargin+1e-12 {
		gate.Decision = ComparisonPass
		gate.ReasonCode = "confidence_bound_cleared"
	} else {
		gate.Decision = ComparisonInconclusive
		gate.ReasonCode = "confidence_bound_not_cleared"
	}
	return gate
}

func successGate(metric PairedIndicatorComparison) ComparisonGate {
	threshold := -ComparisonSuccessMargin
	gate := ComparisonGate{
		Rule:           "one-sided 95% lower bound(candidate success rate - control success rate) >= -0.01",
		Threshold:      threshold,
		Observed:       metric.CandidateMinusControl,
		EligibleCases:  metric.EligibleCases,
		EligibleGroups: metric.UniqueGroups,
	}
	if metric.EligibleCases == 0 {
		gate.Decision = ComparisonInconclusive
		gate.ReasonCode = "insufficient_eligible_cases"
		return gate
	}
	if metric.CandidateMinusControl < threshold-1e-12 {
		gate.Decision = ComparisonFail
		gate.ReasonCode = "point_estimate_outside_margin"
		gate.OneSidedBound = floatPointer(metric.GroupClusterBootstrap95.Lower)
		return gate
	}
	if reason, required := underpoweredReason(metric, ComparisonSuccessMargin); reason != "" {
		gate.Decision = ComparisonInconclusive
		gate.ReasonCode = reason
		gate.RequiredGroups = required
		gate.OneSidedBound = floatPointer(metric.GroupClusterBootstrap95.Lower)
		return gate
	}
	gate.OneSidedBound = floatPointer(metric.GroupClusterBootstrap95.Lower)
	if metric.GroupClusterBootstrap95.Lower >= threshold-1e-12 {
		gate.Decision = ComparisonPass
		gate.ReasonCode = "confidence_bound_cleared"
	} else {
		gate.Decision = ComparisonInconclusive
		gate.ReasonCode = "confidence_bound_not_cleared"
	}
	return gate
}

func underpoweredReason(
	metric PairedIndicatorComparison,
	margin float64,
) (string, int) {
	if metric.UniqueGroups < 2 {
		return "insufficient_independent_groups", 2
	}
	if metric.NonzeroEffectGroups == 0 {
		required := minimumZeroDiscordanceGroups(margin)
		if metric.UniqueGroups < required {
			if metric.Discordance.Total == 0 {
				return "zero_discordance_underpowered", required
			}
			return "zero_group_effect_underpowered", required
		}
		return "", 0
	}
	if metric.Discordance.Total == 0 {
		// A valid summarized metric cannot have a nonzero group effect without
		// row-level discordance. Remain conservative for hand-built metrics.
		return "zero_discordance_underpowered",
			minimumZeroDiscordanceGroups(margin)
	}
	if metric.UniqueGroups < minimumBootstrapGroups {
		return "insufficient_independent_groups", minimumBootstrapGroups
	}
	return "", 0
}

func minimumZeroDiscordanceGroups(margin float64) int {
	alpha := 1 - ComparisonConfidence
	return int(math.Ceil(math.Log(alpha) / math.Log(1-margin)))
}

func schemaGate(
	schema SchemaValidityComparison,
	cases int,
	groups int,
) ComparisonGate {
	gate := ComparisonGate{
		Rule:           "candidate schema-valid usable response rate >= 0.995",
		Threshold:      ComparisonSchemaValidFloor,
		Observed:       schema.Candidate.Rate,
		EligibleCases:  cases,
		EligibleGroups: groups,
	}
	if cases == 0 {
		gate.Decision = ComparisonInconclusive
		gate.ReasonCode = "insufficient_eligible_cases"
		return gate
	}
	// Compare exact integers so a floating-point representation cannot move
	// a result sitting exactly on the 99.5% boundary.
	if schema.Candidate.Count*1000 >= schema.Candidate.Denominator*995 {
		gate.Decision = ComparisonPass
		gate.ReasonCode = "threshold_cleared"
	} else {
		gate.Decision = ComparisonFail
		gate.ReasonCode = "below_threshold"
	}
	return gate
}

func safetyGate(safety SafetyHarmComparison) ComparisonGate {
	gate := ComparisonGate{
		Rule:           "candidate primary harm count on the configured safety slice == 0",
		Threshold:      0,
		Observed:       float64(safety.Candidate.Count),
		EligibleCases:  safety.Candidate.Denominator,
		EligibleGroups: safety.UniqueGroups,
	}
	if safety.Candidate.Denominator == 0 {
		gate.Decision = ComparisonInconclusive
		gate.ReasonCode = "insufficient_eligible_cases"
	} else if safety.Candidate.Count == 0 {
		gate.Decision = ComparisonPass
		gate.ReasonCode = "zero_observed_harm"
	} else {
		gate.Decision = ComparisonFail
		gate.ReasonCode = "observed_safety_harm"
	}
	return gate
}

func combineComparisonDecisions(
	decisions ...ComparisonDecision,
) ComparisonDecision {
	combined := ComparisonPass
	for _, decision := range decisions {
		if decision == ComparisonFail {
			return ComparisonFail
		}
		if decision == ComparisonInconclusive {
			combined = ComparisonInconclusive
		}
	}
	return combined
}

func countObservationGroups(observations []pairedObservation) int {
	groups := make(map[string]struct{}, len(observations))
	for _, observation := range observations {
		groups[observation.groupID] = struct{}{}
	}
	return len(groups)
}

func countNonzeroGroupEffects(observations []pairedObservation) int {
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
	nonzero := 0
	for _, aggregate := range byGroup {
		if aggregate.candidate != aggregate.control {
			nonzero++
		}
	}
	return nonzero
}

func hasSlice(sliceIDs []string, wanted string) bool {
	for _, sliceID := range sliceIDs {
		if sliceID == wanted {
			return true
		}
	}
	return false
}

func floatPointer(value float64) *float64 {
	return &value
}

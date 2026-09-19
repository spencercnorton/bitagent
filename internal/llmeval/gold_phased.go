package llmeval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	GoldDevelopmentClosureStatus           = "development_gold_closed_before_holdout"
	GoldDevelopmentClosureAlgorithmID      = "sha256-plan-task-suite-reviewed-development-v1"
	maxGoldDevelopmentClosureManifestBytes = 4 << 20
)

func ReadGoldDevelopmentClosureManifest(
	r io.Reader,
) (GoldDevelopmentClosureManifest, string, error) {
	if r == nil {
		return GoldDevelopmentClosureManifest{}, "", fmt.Errorf(
			"development closure manifest reader is nil",
		)
	}
	raw, err := io.ReadAll(io.LimitReader(
		r,
		maxGoldDevelopmentClosureManifestBytes+1,
	))
	if err != nil {
		return GoldDevelopmentClosureManifest{}, "", err
	}
	if len(raw) == 0 ||
		len(raw) > maxGoldDevelopmentClosureManifestBytes {
		return GoldDevelopmentClosureManifest{}, "", fmt.Errorf(
			"development closure manifest is empty or too large",
		)
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return GoldDevelopmentClosureManifest{}, "", err
	}
	var manifest GoldDevelopmentClosureManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return GoldDevelopmentClosureManifest{}, "", err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return GoldDevelopmentClosureManifest{}, "", fmt.Errorf(
			"development closure manifest has trailing data",
		)
	}
	if manifest.SchemaVersion != SchemaVersion ||
		manifest.Status != GoldDevelopmentClosureStatus ||
		manifest.SelectionAlgorithmID !=
			GoldDevelopmentClosureAlgorithmID {
		return GoldDevelopmentClosureManifest{}, "", fmt.Errorf(
			"development closure manifest header is unsupported",
		)
	}
	return manifest, sha256Hex(raw), nil
}

// GoldDevelopmentClosureInput closes only the phase-tagged development review
// reserve. Holdout candidates are validated through the full freeze but are
// not inputs to either review promotion.
type GoldDevelopmentClosureInput struct {
	Plan                          CorpusPlan
	PlanSHA256                    string
	Candidates                    Corpus
	CandidateFreezeManifest       CorpusFreezeManifest
	CandidateFreezeManifestSHA256 string
	MatcherPromotion              MatcherGoldPromotion
	MatcherArtifacts              GoldClosureReviewArtifactHashes
	ContentJunkPromotion          GoldPromotion
	ContentJunkArtifacts          GoldClosureReviewArtifactHashes
}

type GoldDevelopmentClosure struct {
	Development Corpus
	Manifest    GoldDevelopmentClosureManifest
}

type GoldDevelopmentClosureManifest struct {
	SchemaVersion                 int                          `json:"schema_version"`
	Status                        string                       `json:"status"`
	SelectionAlgorithmID          string                       `json:"selection_algorithm_id"`
	PlanID                        string                       `json:"plan_id"`
	PlanSHA256                    string                       `json:"plan_sha256"`
	CandidateCorpusSHA256         string                       `json:"candidate_corpus_sha256"`
	CandidateFreezeManifestSHA256 string                       `json:"candidate_freeze_manifest_sha256"`
	CandidateDevelopmentSHA256    string                       `json:"candidate_development_corpus_sha256"`
	CandidateDevelopmentCases     int                          `json:"candidate_development_cases"`
	DevelopmentCorpusSHA256       string                       `json:"development_corpus_sha256"`
	DevelopmentCases              int                          `json:"development_cases"`
	ReviewWorkflows               []GoldClosureReviewSummary   `json:"review_workflows"`
	Tasks                         []GoldDevelopmentTaskSummary `json:"tasks"`
}

type GoldDevelopmentTaskSummary struct {
	Task                  Task           `json:"task"`
	CandidateCases        int            `json:"candidate_cases"`
	EligibleGoldCases     int            `json:"eligible_gold_cases"`
	UnresolvedCases       int            `json:"unresolved_cases"`
	DevelopmentAllocation map[string]int `json:"development_allocation"`
	SelectedCases         int            `json:"selected_cases"`
	SelectedGroups        int            `json:"selected_groups"`
}

// GoldHoldoutClosureInput can be constructed only after a development closure
// and a separately hashed all-passers finalist roster exist.
type GoldHoldoutClosureInput struct {
	Plan                          CorpusPlan
	PlanSHA256                    string
	Candidates                    Corpus
	CandidateFreezeManifest       CorpusFreezeManifest
	CandidateFreezeManifestSHA256 string
	Development                   Corpus
	DevelopmentManifest           GoldDevelopmentClosureManifest
	DevelopmentManifestSHA256     string
	FinalistRoster                FinalistRosterManifest
	FinalistRosterSHA256          string
	SystemManifest                SystemManifest
	SystemManifestSHA256          string
	MatcherPromotion              MatcherGoldPromotion
	MatcherArtifacts              GoldClosureReviewArtifactHashes
	ContentJunkPromotion          GoldPromotion
	ContentJunkArtifacts          GoldClosureReviewArtifactHashes
}

func FinalizeDevelopmentGold(
	input GoldDevelopmentClosureInput,
) (GoldDevelopmentClosure, error) {
	candidates, frozen, err := validatePhasedGoldBase(
		input.Plan,
		input.PlanSHA256,
		input.Candidates,
		input.CandidateFreezeManifest,
		input.CandidateFreezeManifestSHA256,
	)
	if err != nil {
		return GoldDevelopmentClosure{}, err
	}
	source := frozen.Development
	matcherGold, matcherSummary, contentJunkGold, contentJunkSummary, err :=
		validatePhasePromotions(
			source,
			input.MatcherPromotion,
			input.MatcherArtifacts,
			input.ContentJunkPromotion,
			input.ContentJunkArtifacts,
		)
	if err != nil {
		return GoldDevelopmentClosure{}, err
	}
	goldByID, err := mergePhaseGold(matcherGold, contentJunkGold)
	if err != nil {
		return GoldDevelopmentClosure{}, err
	}
	groupsByTask, err := buildGoldClosureGroups(
		input.Plan,
		source,
		goldByID,
	)
	if err != nil {
		return GoldDevelopmentClosure{}, err
	}
	taskPlans := corpusPlanTasksByID(input.Plan)
	var developmentRecords []CorpusRecord
	taskSummaries := make([]GoldDevelopmentTaskSummary, 0, len(orderedTasks))
	for _, task := range orderedTasks {
		taskPlan := taskPlans[task]
		groups := groupsByTask[task]
		selected, _, err := selectGoldDevelopmentGroups(
			taskPlan,
			groups,
			taskPlan.DevelopmentAllocation,
		)
		if err != nil {
			return GoldDevelopmentClosure{}, fmt.Errorf(
				"development closure task %q: %w",
				task,
				err,
			)
		}
		developmentRecords = appendGoldDevelopmentRepresentatives(
			developmentRecords,
			selected,
		)
		eligibleCases := 0
		unresolvedCases := 0
		for _, group := range groups {
			if group.eligible {
				eligibleCases += len(group.gold)
			} else {
				unresolvedCases += len(group.candidates) - len(group.gold)
			}
		}
		taskSummaries = append(taskSummaries, GoldDevelopmentTaskSummary{
			Task:              task,
			CandidateCases:    groupCandidateCaseCount(groups),
			EligibleGoldCases: eligibleCases,
			UnresolvedCases:   unresolvedCases,
			DevelopmentAllocation: cloneStringIntMap(
				taskPlan.DevelopmentAllocation,
			),
			SelectedCases:  len(selected),
			SelectedGroups: len(selected),
		})
	}
	development, err := NewCorpus(developmentRecords)
	if err != nil {
		return GoldDevelopmentClosure{}, err
	}
	_ = candidates
	return GoldDevelopmentClosure{
		Development: development,
		Manifest: GoldDevelopmentClosureManifest{
			SchemaVersion:                 SchemaVersion,
			Status:                        GoldDevelopmentClosureStatus,
			SelectionAlgorithmID:          GoldDevelopmentClosureAlgorithmID,
			PlanID:                        input.Plan.PlanID,
			PlanSHA256:                    input.PlanSHA256,
			CandidateCorpusSHA256:         input.Candidates.SHA256,
			CandidateFreezeManifestSHA256: input.CandidateFreezeManifestSHA256,
			CandidateDevelopmentSHA256:    source.SHA256,
			CandidateDevelopmentCases:     len(source.Records),
			DevelopmentCorpusSHA256:       development.SHA256,
			DevelopmentCases:              len(development.Records),
			ReviewWorkflows: []GoldClosureReviewSummary{
				matcherSummary,
				contentJunkSummary,
			},
			Tasks: taskSummaries,
		},
	}, nil
}

func FinalizeHoldoutGold(
	input GoldHoldoutClosureInput,
) (GoldClosure, error) {
	candidates, frozen, err := validatePhasedGoldBase(
		input.Plan,
		input.PlanSHA256,
		input.Candidates,
		input.CandidateFreezeManifest,
		input.CandidateFreezeManifestSHA256,
	)
	if err != nil {
		return GoldClosure{}, err
	}
	if err := validateSHA256(
		"development_manifest_sha256",
		input.DevelopmentManifestSHA256,
	); err != nil {
		return GoldClosure{}, err
	}
	if err := validateSHA256(
		"finalist_roster_sha256",
		input.FinalistRosterSHA256,
	); err != nil {
		return GoldClosure{}, err
	}
	if err := validateDevelopmentClosureBinding(
		input.DevelopmentManifest,
		input.Development,
		input.Plan,
		input.PlanSHA256,
		candidates,
		input.CandidateFreezeManifestSHA256,
		frozen,
	); err != nil {
		return GoldClosure{}, err
	}
	if err := input.FinalistRoster.Validate(
		input.SystemManifest,
		input.SystemManifestSHA256,
	); err != nil {
		return GoldClosure{}, err
	}
	if input.FinalistRoster.PlanSHA256 != input.PlanSHA256 ||
		input.FinalistRoster.DevelopmentClosureManifestSHA256 !=
			input.DevelopmentManifestSHA256 ||
		input.FinalistRoster.DevelopmentCorpusSHA256 !=
			input.Development.SHA256 {
		return GoldClosure{}, fmt.Errorf(
			"finalist roster does not bind the exact completed development phase",
		)
	}
	// Stage 1 score and comparison artifacts are per-task, so the roster
	// declares a per-task corpus SHA. Recompute each one here, where the frozen
	// development corpus is available, so a roster cannot bind its evidence to a
	// favourably filtered subset of the phase.
	for _, task := range input.FinalistRoster.Tasks {
		taskCorpus, err := FilterCorpus(input.Development, task.Task, 0)
		if err != nil {
			return GoldClosure{}, fmt.Errorf(
				"finalist roster task %q: %w",
				task.Task,
				err,
			)
		}
		if task.TaskDevelopmentCorpusSHA256 != taskCorpus.SHA256 {
			return GoldClosure{}, fmt.Errorf(
				"finalist roster task %q task_development_corpus_sha256 does not match the development corpus filtered to that task",
				task.Task,
			)
		}
	}

	source := frozen.Holdout
	matcherGold, matcherSummary, contentJunkGold, contentJunkSummary, err :=
		validatePhasePromotions(
			source,
			input.MatcherPromotion,
			input.MatcherArtifacts,
			input.ContentJunkPromotion,
			input.ContentJunkArtifacts,
		)
	if err != nil {
		return GoldClosure{}, err
	}
	goldByID, err := mergePhaseGold(matcherGold, contentJunkGold)
	if err != nil {
		return GoldClosure{}, err
	}
	groupsByTask, err := buildGoldClosureGroups(
		input.Plan,
		source,
		goldByID,
	)
	if err != nil {
		return GoldClosure{}, err
	}
	taskPlans := corpusPlanTasksByID(input.Plan)
	var holdoutRecords []CorpusRecord
	taskSummaries := make([]GoldClosureTaskSummary, 0, len(orderedTasks))
	for _, task := range orderedTasks {
		taskPlan := taskPlans[task]
		groups := groupsByTask[task]
		holdout, bySuite, err := selectGoldHoldoutGroups(taskPlan, groups)
		if err != nil {
			return GoldClosure{}, fmt.Errorf(
				"holdout closure task %q: %w",
				task,
				err,
			)
		}
		holdoutRecords = appendGoldGroupRecords(holdoutRecords, holdout)
		summary := summarizeGoldClosureTask(
			taskPlan,
			input.CandidateFreezeManifest,
			groups,
			nil,
			holdout,
			bySuite,
			taskPlan.DevelopmentAllocation,
		)
		summary.DevelopmentCases = countCorpusTask(
			input.Development,
			task,
		)
		summary.DevelopmentGroups = summary.DevelopmentCases
		for _, freezeTask := range frozen.Manifest.Tasks {
			if freezeTask.Task == task {
				summary.CandidateCases = freezeTask.CandidateCases
				summary.CandidateGroups = freezeTask.CandidateGroups
				break
			}
		}
		taskSummaries = append(taskSummaries, summary)
	}
	holdout, err := NewCorpus(holdoutRecords)
	if err != nil {
		return GoldClosure{}, err
	}
	if err := validateSplitDisjoint(input.Development, holdout); err != nil {
		return GoldClosure{}, err
	}
	return GoldClosure{
		Development: input.Development,
		Holdout:     holdout,
		Manifest: GoldClosureManifest{
			SchemaVersion:                    SchemaVersion,
			Status:                           GoldClosureStatus,
			Scope:                            "phase-separated deterministic gold closure: development reviewed and closed first, every Stage 1 passer frozen, then holdout reviewed and closed without model outputs as selection inputs",
			SelectionAlgorithmID:             GoldClosureAlgorithmID,
			PlanID:                           input.Plan.PlanID,
			PlanSHA256:                       input.PlanSHA256,
			PlanExecutable:                   input.Plan.Executable,
			ProductionSourceExporterVerified: input.CandidateFreezeManifest.ProductionSourceExporterVerified,
			ProductionExportManifestSHA256:   input.CandidateFreezeManifest.ProductionExportManifestSHA256,
			PrivacySidecarSHA256:             input.CandidateFreezeManifest.PrivacySidecarSHA256,
			CandidateCorpusSHA256:            candidates.SHA256,
			CandidateCases:                   len(candidates.Records),
			CandidateFreezeManifestSHA256:    input.CandidateFreezeManifestSHA256,
			CandidateDevelopmentSHA256:       frozen.Development.SHA256,
			CandidateHoldoutSHA256:           frozen.Holdout.SHA256,
			DevelopmentCorpusSHA256:          input.Development.SHA256,
			DevelopmentCases:                 len(input.Development.Records),
			ReviewPhaseSeparated:             true,
			DevelopmentClosureManifestSHA256: input.DevelopmentManifestSHA256,
			FinalistRosterManifestSHA256:     input.FinalistRosterSHA256,
			HoldoutCorpusSHA256:              holdout.SHA256,
			HoldoutCases:                     len(holdout.Records),
			ReviewWorkflows: []GoldClosureReviewSummary{
				matcherSummary,
				contentJunkSummary,
			},
			Tasks: taskSummaries,
		},
	}, nil
}

func validatePhasedGoldBase(
	plan CorpusPlan,
	planSHA256 string,
	candidates Corpus,
	manifest CorpusFreezeManifest,
	manifestSHA256 string,
) (Corpus, CorpusFreeze, error) {
	if err := plan.Validate(); err != nil {
		return Corpus{}, CorpusFreeze{}, err
	}
	if err := validateSHA256("plan_sha256", planSHA256); err != nil {
		return Corpus{}, CorpusFreeze{}, err
	}
	if err := validateSHA256(
		"candidate_freeze_manifest_sha256",
		manifestSHA256,
	); err != nil {
		return Corpus{}, CorpusFreeze{}, err
	}
	canonical, err := validateFrozenReviewSource(candidates)
	if err != nil {
		return Corpus{}, CorpusFreeze{}, err
	}
	for index, record := range canonical.Records {
		phase, err := recordReviewPhase(record)
		if err != nil {
			return Corpus{}, CorpusFreeze{}, fmt.Errorf(
				"candidate %d (%q) review phase: %w",
				index,
				record.CaseID,
				err,
			)
		}
		if phase == "" {
			return Corpus{}, CorpusFreeze{}, fmt.Errorf(
				"candidate %d (%q) is not phase-bound before review",
				index,
				record.CaseID,
			)
		}
	}
	if err := ValidateCorpusFreezeManifest(
		plan,
		planSHA256,
		canonical,
		manifest,
	); err != nil {
		return Corpus{}, CorpusFreeze{}, err
	}
	frozen, err := FreezeCandidateCorpus(plan, planSHA256, canonical)
	if err != nil {
		return Corpus{}, CorpusFreeze{}, err
	}
	return canonical, frozen, nil
}

func validatePhasePromotions(
	source Corpus,
	matcherPromotion MatcherGoldPromotion,
	matcherArtifacts GoldClosureReviewArtifactHashes,
	contentJunkPromotion GoldPromotion,
	contentJunkArtifacts GoldClosureReviewArtifactHashes,
) (
	map[string]CorpusRecord,
	GoldClosureReviewSummary,
	map[string]CorpusRecord,
	GoldClosureReviewSummary,
	error,
) {
	matcherView := goldClosurePromotionView{
		workflow:      ReviewWorkflowMatcher,
		gold:          matcherPromotion.GoldCorpus,
		corpusSHA256:  matcherPromotion.Agreement.CorpusSHA256,
		reviewSetID:   matcherPromotion.Agreement.ReviewSetID,
		policyVersion: matcherPromotion.Agreement.PolicyVersion,
		proof:         matcherPromotion.Agreement.ReviewProof,
		reviewerA:     matcherPromotion.Agreement.ReviewerA,
		reviewerB:     matcherPromotion.Agreement.ReviewerB,
		overall:       matcherPromotion.Agreement.Overall,
		byTask:        matcherPromotion.Agreement.ByTask,
		needsIDs:      matcherPromotion.Agreement.NeedsAdjudicationCaseIDs,
		adjudicated:   matcherPromotion.AdjudicatedCases,
		unresolvedIDs: matcherPromotion.UnresolvedCaseIDs,
		artifacts:     matcherArtifacts,
	}
	contentView := goldClosurePromotionView{
		workflow:      ReviewWorkflowContentJunk,
		gold:          contentJunkPromotion.GoldCorpus,
		corpusSHA256:  contentJunkPromotion.Agreement.CorpusSHA256,
		reviewSetID:   contentJunkPromotion.Agreement.ReviewSetID,
		policyVersion: contentJunkPromotion.Agreement.PolicyVersion,
		proof:         contentJunkPromotion.Agreement.ReviewProof,
		reviewerA:     contentJunkPromotion.Agreement.ReviewerA,
		reviewerB:     contentJunkPromotion.Agreement.ReviewerB,
		overall:       contentJunkPromotion.Agreement.Overall,
		byTask:        contentJunkPromotion.Agreement.ByTask,
		needsIDs:      contentJunkPromotion.Agreement.NeedsAdjudicationCaseIDs,
		adjudicated:   contentJunkPromotion.AdjudicatedCases,
		unresolvedIDs: contentJunkPromotion.UnresolvedCaseIDs,
		artifacts:     contentJunkArtifacts,
	}
	matcherGold, matcherSummary, err := validateClosurePromotion(
		source,
		[]Task{TaskMatcherExtract, TaskMatcherRerank},
		matcherView,
	)
	if err != nil {
		return nil, GoldClosureReviewSummary{}, nil,
			GoldClosureReviewSummary{}, fmt.Errorf(
				"matcher promotion: %w",
				err,
			)
	}
	contentGold, contentSummary, err := validateClosurePromotion(
		source,
		[]Task{TaskContentFilter, TaskJunkPurge},
		contentView,
	)
	if err != nil {
		return nil, GoldClosureReviewSummary{}, nil,
			GoldClosureReviewSummary{}, fmt.Errorf(
				"content/junk promotion: %w",
				err,
			)
	}
	return matcherGold, matcherSummary, contentGold, contentSummary, nil
}

func mergePhaseGold(
	left,
	right map[string]CorpusRecord,
) (map[string]CorpusRecord, error) {
	out := make(map[string]CorpusRecord, len(left)+len(right))
	for caseID, record := range left {
		out[caseID] = record
	}
	for caseID, record := range right {
		if _, duplicate := out[caseID]; duplicate {
			return nil, fmt.Errorf(
				"case %q appears in both review workflows",
				caseID,
			)
		}
		out[caseID] = record
	}
	return out, nil
}

func validateDevelopmentClosureBinding(
	manifest GoldDevelopmentClosureManifest,
	development Corpus,
	plan CorpusPlan,
	planSHA256 string,
	candidates Corpus,
	freezeManifestSHA256 string,
	frozen CorpusFreeze,
) error {
	if manifest.SchemaVersion != SchemaVersion ||
		manifest.Status != GoldDevelopmentClosureStatus ||
		manifest.SelectionAlgorithmID !=
			GoldDevelopmentClosureAlgorithmID {
		return fmt.Errorf("development closure manifest status is unsupported")
	}
	if manifest.PlanID != plan.PlanID ||
		manifest.PlanSHA256 != planSHA256 ||
		manifest.CandidateCorpusSHA256 != candidates.SHA256 ||
		manifest.CandidateFreezeManifestSHA256 !=
			freezeManifestSHA256 ||
		manifest.CandidateDevelopmentSHA256 !=
			frozen.Development.SHA256 ||
		manifest.CandidateDevelopmentCases !=
			len(frozen.Development.Records) ||
		manifest.DevelopmentCorpusSHA256 != development.SHA256 ||
		manifest.DevelopmentCases != len(development.Records) {
		return fmt.Errorf(
			"development closure manifest does not bind the exact plan, freeze, and gold corpus",
		)
	}
	if err := ValidateGoldCorpus(development); err != nil {
		return err
	}
	return nil
}

func corpusPlanTasksByID(plan CorpusPlan) map[Task]CorpusPlanTask {
	out := make(map[Task]CorpusPlanTask, len(plan.Tasks))
	for _, task := range plan.Tasks {
		out[task.Task] = task
	}
	return out
}

func groupCandidateCaseCount(groups []*goldClosureGroup) int {
	total := 0
	for _, group := range groups {
		total += len(group.candidates)
	}
	return total
}

func countCorpusTask(corpus Corpus, task Task) int {
	total := 0
	for _, record := range corpus.Records {
		if record.Task == task {
			total++
		}
	}
	return total
}

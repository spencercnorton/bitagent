package llmeval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
)

const (
	// GoldClosureStatus identifies a completed deterministic post-review
	// selection. The adjacent scope and exporter fields are still required:
	// this status alone is not evidence that a production export was valid.
	GoldClosureStatus = "final_gold_quota_closed"

	// GoldClosureAlgorithmID binds the selection behavior implemented here.
	GoldClosureAlgorithmID = "sha256-plan-task-group-reviewed-representative-gold-closure-v2"

	maxGoldClosureManifestBytes = 4 << 20
)

// GoldClosureReviewArtifactHashes binds a workflow to the exact assignment
// and response bytes from which its promotion was reconstructed. Adjudication
// hashes are both present or both absent.
type GoldClosureReviewArtifactHashes struct {
	PrimaryAssignmentASHA256     string `json:"primary_assignment_a_sha256"`
	PrimarySubmissionASHA256     string `json:"primary_submission_a_sha256"`
	PrimaryAssignmentBSHA256     string `json:"primary_assignment_b_sha256"`
	PrimarySubmissionBSHA256     string `json:"primary_submission_b_sha256"`
	AdjudicationAssignmentSHA256 string `json:"adjudication_assignment_sha256,omitempty"`
	AdjudicationSubmissionSHA256 string `json:"adjudication_submission_sha256,omitempty"`
}

// GoldClosureInput contains only pre-model evidence. It deliberately has no
// ResultRecord, system descriptor, prompt, route, or provider-output field.
type GoldClosureInput struct {
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

// GoldClosure contains the two immutable, disjoint reviewed-gold corpora and
// an aggregate source-text-free manifest.
type GoldClosure struct {
	Development Corpus
	Holdout     Corpus
	Manifest    GoldClosureManifest
}

// GoldClosureManifest is deterministic and intentionally has no wall clock.
// It preserves the complete hash chain from plan and candidate freeze through
// independent review policy/evidence and exact review artifacts.
type GoldClosureManifest struct {
	SchemaVersion                    int                        `json:"schema_version"`
	Status                           string                     `json:"status"`
	Scope                            string                     `json:"scope"`
	SelectionAlgorithmID             string                     `json:"selection_algorithm_id"`
	PlanID                           string                     `json:"plan_id"`
	PlanSHA256                       string                     `json:"plan_sha256"`
	PlanExecutable                   bool                       `json:"plan_executable"`
	ProductionSourceExporterVerified bool                       `json:"production_source_exporter_verified"`
	ProductionExportManifestSHA256   string                     `json:"production_export_manifest_sha256"`
	PrivacySidecarSHA256             string                     `json:"privacy_sidecar_sha256"`
	CandidateCorpusSHA256            string                     `json:"candidate_corpus_sha256"`
	CandidateCases                   int                        `json:"candidate_cases"`
	CandidateFreezeManifestSHA256    string                     `json:"candidate_freeze_manifest_sha256"`
	CandidateDevelopmentSHA256       string                     `json:"candidate_development_corpus_sha256"`
	CandidateHoldoutSHA256           string                     `json:"candidate_holdout_corpus_sha256"`
	DevelopmentCorpusSHA256          string                     `json:"development_corpus_sha256"`
	DevelopmentCases                 int                        `json:"development_cases"`
	ReviewPhaseSeparated             bool                       `json:"review_phase_separated"`
	DevelopmentClosureManifestSHA256 string                     `json:"development_closure_manifest_sha256,omitempty"`
	FinalistRosterManifestSHA256     string                     `json:"finalist_roster_manifest_sha256,omitempty"`
	HoldoutCorpusSHA256              string                     `json:"holdout_corpus_sha256"`
	HoldoutCases                     int                        `json:"holdout_cases"`
	ReviewWorkflows                  []GoldClosureReviewSummary `json:"review_workflows"`
	Tasks                            []GoldClosureTaskSummary   `json:"tasks"`
}

// GoldClosureReviewSummary is an aggregate proof record; it carries no source
// case IDs, reviewer IDs, source text, or arbitrary notes.
type GoldClosureReviewSummary struct {
	Workflow               ReviewWorkflow                  `json:"workflow"`
	ReviewSetID            string                          `json:"review_set_id"`
	PolicyID               string                          `json:"policy_id"`
	PolicyVersion          string                          `json:"policy_version"`
	PolicySHA256           string                          `json:"policy_sha256"`
	EvidenceManifestID     string                          `json:"evidence_manifest_id"`
	EvidenceManifestSHA256 string                          `json:"evidence_manifest_sha256"`
	EvidenceCorpusSHA256   string                          `json:"evidence_corpus_sha256"`
	SourceSnapshotSHA256   string                          `json:"source_snapshot_sha256"`
	ReviewedCases          int                             `json:"reviewed_cases"`
	PromotedGoldCases      int                             `json:"promoted_gold_cases"`
	AdjudicatedCases       int                             `json:"adjudicated_cases"`
	UnresolvedCases        int                             `json:"unresolved_cases"`
	Artifacts              GoldClosureReviewArtifactHashes `json:"artifacts"`
}

// GoldClosureTaskSummary records the quota proof for one task.
type GoldClosureTaskSummary struct {
	Task                       Task                      `json:"task"`
	SourceSnapshotSHA256       string                    `json:"source_snapshot_sha256"`
	CandidateCases             int                       `json:"candidate_cases"`
	CandidateGroups            int                       `json:"candidate_groups"`
	EligibleGoldCases          int                       `json:"eligible_gold_cases"`
	EligibleGoldGroups         int                       `json:"eligible_gold_groups"`
	UnresolvedCases            int                       `json:"unresolved_cases"`
	ExcludedUnresolvedGroups   int                       `json:"excluded_unresolved_groups"`
	DevelopmentTargetMaximum   int                       `json:"development_target_independent_groups"`
	DevelopmentStratumTargets  map[string]int            `json:"development_stratum_targets"`
	DevelopmentCases           int                       `json:"development_cases"`
	DevelopmentGroups          int                       `json:"development_groups"`
	HoldoutTargetMinimum       int                       `json:"holdout_target_minimum"`
	HoldoutCases               int                       `json:"holdout_cases"`
	HoldoutGroups              int                       `json:"holdout_groups"`
	HoldoutAllocations         []GoldClosureSuiteSummary `json:"holdout_allocations"`
	RequiredGoldEligibleGroups map[string]int            `json:"required_gold_eligible_groups"`
	ActualGoldEligibleGroups   map[string]int            `json:"actual_gold_eligible_groups"`
	RequiredGoldQuotaCounts    map[string]int            `json:"required_gold_quota_counts,omitempty"`
	ContradictedQuotaTags      map[string]int            `json:"contradicted_quota_tags,omitempty"`
	UnselectedEligibleCases    int                       `json:"unselected_eligible_cases"`
	UnselectedEligibleGroups   int                       `json:"unselected_eligible_groups"`
}

type GoldClosureSuiteSummary struct {
	Suite        string `json:"suite"`
	MinimumCases int    `json:"minimum_cases"`
	ActualCases  int    `json:"actual_cases"`
	Groups       int    `json:"groups"`
}

type goldClosurePromotionView struct {
	workflow      ReviewWorkflow
	gold          *Corpus
	corpusSHA256  string
	reviewSetID   string
	policyVersion string
	proof         HumanReviewProof
	reviewerA     string
	reviewerB     string
	overall       AgreementStats
	byTask        []TaskAgreementStats
	needsIDs      []string
	adjudicated   int
	unresolvedIDs []string
	artifacts     GoldClosureReviewArtifactHashes
}

type goldClosureGroup struct {
	task       Task
	groupID    string
	suite      string
	hash       [32]byte
	candidates []CorpusRecord
	gold       []CorpusRecord
	eligible   bool
}

// FinalizeGoldCorpus verifies complete independent-review coverage, excludes
// every group containing unresolved ambiguity, then deterministically closes
// development strata and holdout suite/gold quotas in the pre-registered hash
// order. Selection never accepts model output as an input.
func FinalizeGoldCorpus(input GoldClosureInput) (GoldClosure, error) {
	if err := input.Plan.Validate(); err != nil {
		return GoldClosure{}, fmt.Errorf("gold closure plan: %w", err)
	}
	if err := validateSHA256("plan_sha256", input.PlanSHA256); err != nil {
		return GoldClosure{}, err
	}
	if err := validateSHA256(
		"candidate_freeze_manifest_sha256",
		input.CandidateFreezeManifestSHA256,
	); err != nil {
		return GoldClosure{}, err
	}

	candidates, err := validateFrozenReviewSource(input.Candidates)
	if err != nil {
		return GoldClosure{}, fmt.Errorf("gold closure candidates: %w", err)
	}
	expectedFreeze, err := FreezeCandidateCorpus(
		input.Plan,
		input.PlanSHA256,
		candidates,
	)
	if err != nil {
		return GoldClosure{}, fmt.Errorf("gold closure candidate freeze: %w", err)
	}
	if err := ValidateCorpusFreezeManifest(
		input.Plan,
		input.PlanSHA256,
		candidates,
		input.CandidateFreezeManifest,
	); err != nil {
		return GoldClosure{}, fmt.Errorf(
			"gold closure candidate freeze manifest: %w",
			err,
		)
	}

	matcherView := goldClosurePromotionView{
		workflow:      ReviewWorkflowMatcher,
		gold:          input.MatcherPromotion.GoldCorpus,
		corpusSHA256:  input.MatcherPromotion.Agreement.CorpusSHA256,
		reviewSetID:   input.MatcherPromotion.Agreement.ReviewSetID,
		policyVersion: input.MatcherPromotion.Agreement.PolicyVersion,
		proof:         input.MatcherPromotion.Agreement.ReviewProof,
		reviewerA:     input.MatcherPromotion.Agreement.ReviewerA,
		reviewerB:     input.MatcherPromotion.Agreement.ReviewerB,
		overall:       input.MatcherPromotion.Agreement.Overall,
		byTask:        input.MatcherPromotion.Agreement.ByTask,
		needsIDs:      input.MatcherPromotion.Agreement.NeedsAdjudicationCaseIDs,
		adjudicated:   input.MatcherPromotion.AdjudicatedCases,
		unresolvedIDs: input.MatcherPromotion.UnresolvedCaseIDs,
		artifacts:     input.MatcherArtifacts,
	}
	contentJunkView := goldClosurePromotionView{
		workflow:      ReviewWorkflowContentJunk,
		gold:          input.ContentJunkPromotion.GoldCorpus,
		corpusSHA256:  input.ContentJunkPromotion.Agreement.CorpusSHA256,
		reviewSetID:   input.ContentJunkPromotion.Agreement.ReviewSetID,
		policyVersion: input.ContentJunkPromotion.Agreement.PolicyVersion,
		proof:         input.ContentJunkPromotion.Agreement.ReviewProof,
		reviewerA:     input.ContentJunkPromotion.Agreement.ReviewerA,
		reviewerB:     input.ContentJunkPromotion.Agreement.ReviewerB,
		overall:       input.ContentJunkPromotion.Agreement.Overall,
		byTask:        input.ContentJunkPromotion.Agreement.ByTask,
		needsIDs:      input.ContentJunkPromotion.Agreement.NeedsAdjudicationCaseIDs,
		adjudicated:   input.ContentJunkPromotion.AdjudicatedCases,
		unresolvedIDs: input.ContentJunkPromotion.UnresolvedCaseIDs,
		artifacts:     input.ContentJunkArtifacts,
	}

	matcherGold, matcherSummary, err := validateClosurePromotion(
		candidates,
		[]Task{TaskMatcherExtract, TaskMatcherRerank},
		matcherView,
	)
	if err != nil {
		return GoldClosure{}, fmt.Errorf("matcher promotion: %w", err)
	}
	contentJunkGold, contentJunkSummary, err := validateClosurePromotion(
		candidates,
		[]Task{TaskContentFilter, TaskJunkPurge},
		contentJunkView,
	)
	if err != nil {
		return GoldClosure{}, fmt.Errorf("content/junk promotion: %w", err)
	}
	goldByID := make(map[string]CorpusRecord, len(matcherGold)+len(contentJunkGold))
	for caseID, record := range matcherGold {
		goldByID[caseID] = record
	}
	for caseID, record := range contentJunkGold {
		if _, duplicate := goldByID[caseID]; duplicate {
			return GoldClosure{}, fmt.Errorf(
				"gold closure: case %q appears in both review workflows",
				caseID,
			)
		}
		goldByID[caseID] = record
	}

	taskPlans := make(map[Task]CorpusPlanTask, len(input.Plan.Tasks))
	for _, taskPlan := range input.Plan.Tasks {
		taskPlans[taskPlan.Task] = taskPlan
	}
	initialDevelopmentStrata, err := developmentStratumTargets(
		expectedFreeze.Development,
		input.Plan,
	)
	if err != nil {
		return GoldClosure{}, err
	}
	groupsByTask, err := buildGoldClosureGroups(input.Plan, candidates, goldByID)
	if err != nil {
		return GoldClosure{}, err
	}

	var developmentRecords, holdoutRecords []CorpusRecord
	taskSummaries := make([]GoldClosureTaskSummary, 0, len(orderedTasks))
	for _, task := range orderedTasks {
		taskPlan := taskPlans[task]
		groups := groupsByTask[task]
		development, remaining, err := selectGoldDevelopmentGroups(
			taskPlan,
			groups,
			initialDevelopmentStrata[task],
		)
		if err != nil {
			return GoldClosure{}, fmt.Errorf("gold closure task %q: %w", task, err)
		}
		holdout, bySuite, err := selectGoldHoldoutGroups(taskPlan, remaining)
		if err != nil {
			return GoldClosure{}, fmt.Errorf("gold closure task %q: %w", task, err)
		}

		developmentRecords = appendGoldDevelopmentRepresentatives(
			developmentRecords,
			development,
		)
		holdoutRecords = appendGoldGroupRecords(holdoutRecords, holdout)
		summary := summarizeGoldClosureTask(
			taskPlan,
			input.CandidateFreezeManifest,
			groups,
			development,
			holdout,
			bySuite,
			initialDevelopmentStrata[task],
		)
		taskSummaries = append(taskSummaries, summary)
	}

	development, err := NewCorpus(developmentRecords)
	if err != nil {
		return GoldClosure{}, fmt.Errorf("gold closure development corpus: %w", err)
	}
	holdout, err := NewCorpus(holdoutRecords)
	if err != nil {
		return GoldClosure{}, fmt.Errorf("gold closure holdout corpus: %w", err)
	}
	if err := validateSplitDisjoint(development, holdout); err != nil {
		return GoldClosure{}, fmt.Errorf("gold closure: %w", err)
	}

	return GoldClosure{
		Development: development,
		Holdout:     holdout,
		Manifest: GoldClosureManifest{
			SchemaVersion:                    SchemaVersion,
			Status:                           GoldClosureStatus,
			Scope:                            "deterministic quota closure over one exact candidate freeze and independently reviewed human gold; model outputs are not inputs, and source-export validity remains represented by production_source_exporter_verified",
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
			CandidateDevelopmentSHA256:       input.CandidateFreezeManifest.DevelopmentCorpusSHA256,
			CandidateHoldoutSHA256:           input.CandidateFreezeManifest.HoldoutCorpusSHA256,
			DevelopmentCorpusSHA256:          development.SHA256,
			DevelopmentCases:                 len(development.Records),
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

// ReadCorpusFreezeManifest strictly reads and hashes the exact candidate
// freeze manifest bytes used by final gold closure.
func ReadCorpusFreezeManifest(
	r io.Reader,
) (CorpusFreezeManifest, string, error) {
	if r == nil {
		return CorpusFreezeManifest{}, "", fmt.Errorf(
			"candidate freeze manifest reader is nil",
		)
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxGoldClosureManifestBytes+1))
	if err != nil {
		return CorpusFreezeManifest{}, "", fmt.Errorf(
			"read candidate freeze manifest: %w",
			err,
		)
	}
	if len(raw) == 0 {
		return CorpusFreezeManifest{}, "", fmt.Errorf(
			"candidate freeze manifest is empty",
		)
	}
	if len(raw) > maxGoldClosureManifestBytes {
		return CorpusFreezeManifest{}, "", fmt.Errorf(
			"candidate freeze manifest exceeds the %d-byte limit",
			maxGoldClosureManifestBytes,
		)
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return CorpusFreezeManifest{}, "", fmt.Errorf(
			"decode candidate freeze manifest: %w",
			err,
		)
	}
	var manifest CorpusFreezeManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return CorpusFreezeManifest{}, "", fmt.Errorf(
			"decode candidate freeze manifest: %w",
			err,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return CorpusFreezeManifest{}, "", fmt.Errorf(
				"decode candidate freeze manifest: multiple JSON values",
			)
		}
		return CorpusFreezeManifest{}, "", fmt.Errorf(
			"decode candidate freeze manifest: trailing data: %w",
			err,
		)
	}
	return manifest, sha256Hex(raw), nil
}

// ReadGoldClosureManifest strictly reads and hashes an immutable final closure
// manifest. Unknown fields, duplicate keys, and trailing JSON fail closed.
func ReadGoldClosureManifest(
	r io.Reader,
) (GoldClosureManifest, string, error) {
	if r == nil {
		return GoldClosureManifest{}, "", fmt.Errorf(
			"gold closure manifest reader is nil",
		)
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxGoldClosureManifestBytes+1))
	if err != nil {
		return GoldClosureManifest{}, "", fmt.Errorf(
			"read gold closure manifest: %w",
			err,
		)
	}
	if len(raw) == 0 {
		return GoldClosureManifest{}, "", fmt.Errorf(
			"gold closure manifest is empty",
		)
	}
	if len(raw) > maxGoldClosureManifestBytes {
		return GoldClosureManifest{}, "", fmt.Errorf(
			"gold closure manifest exceeds the %d-byte limit",
			maxGoldClosureManifestBytes,
		)
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return GoldClosureManifest{}, "", fmt.Errorf(
			"decode gold closure manifest: %w",
			err,
		)
	}
	var manifest GoldClosureManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return GoldClosureManifest{}, "", fmt.Errorf(
			"decode gold closure manifest: %w",
			err,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return GoldClosureManifest{}, "", fmt.Errorf(
				"decode gold closure manifest: multiple JSON values",
			)
		}
		return GoldClosureManifest{}, "", fmt.Errorf(
			"decode gold closure manifest: trailing data: %w",
			err,
		)
	}
	return manifest, sha256Hex(raw), nil
}

// ValidateGoldClosureCorpus binds one complete final development or holdout
// corpus to its exact closure manifest and rechecks the policy/privacy chain.
// Task-filtered prefixes must be derived only after this whole-corpus check.
func ValidateGoldClosureCorpus(
	corpus Corpus,
	manifest GoldClosureManifest,
) error {
	canonical, err := validateFrozenReviewSource(corpus)
	if err != nil {
		return fmt.Errorf("gold closure corpus: %w", err)
	}
	if err := ValidateGoldCorpus(canonical); err != nil {
		return err
	}
	for _, record := range canonical.Records {
		if record.Label.Provenance == LabelProvenanceSynthetic {
			return fmt.Errorf(
				"synthetic protocol corpus cannot be bound to a production gold closure manifest",
			)
		}
	}
	if manifest.SchemaVersion != SchemaVersion {
		return fmt.Errorf(
			"gold closure manifest schema_version got %d, want %d",
			manifest.SchemaVersion,
			SchemaVersion,
		)
	}
	if manifest.Status != GoldClosureStatus {
		return fmt.Errorf(
			"gold closure manifest status got %q, want %q",
			manifest.Status,
			GoldClosureStatus,
		)
	}
	if manifest.SelectionAlgorithmID != GoldClosureAlgorithmID {
		return fmt.Errorf(
			"gold closure manifest selection_algorithm_id got %q, want %q",
			manifest.SelectionAlgorithmID,
			GoldClosureAlgorithmID,
		)
	}
	if !manifest.PlanExecutable {
		return fmt.Errorf(
			"gold closure manifest plan_executable must be true for production execution",
		)
	}
	if !manifest.ProductionSourceExporterVerified {
		return fmt.Errorf(
			"gold closure manifest production_source_exporter_verified must be true for production execution",
		)
	}
	if err := validateIdentifier("gold closure manifest.plan_id", manifest.PlanID); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"plan_sha256":                         manifest.PlanSHA256,
		"production_export_manifest_sha256":   manifest.ProductionExportManifestSHA256,
		"privacy_sidecar_sha256":              manifest.PrivacySidecarSHA256,
		"candidate_corpus_sha256":             manifest.CandidateCorpusSHA256,
		"candidate_freeze_manifest_sha256":    manifest.CandidateFreezeManifestSHA256,
		"candidate_development_corpus_sha256": manifest.CandidateDevelopmentSHA256,
		"candidate_holdout_corpus_sha256":     manifest.CandidateHoldoutSHA256,
		"development_corpus_sha256":           manifest.DevelopmentCorpusSHA256,
		"holdout_corpus_sha256":               manifest.HoldoutCorpusSHA256,
	} {
		if err := validateSHA256("gold closure manifest."+name, value); err != nil {
			return err
		}
	}
	if manifest.CandidateCases <= 0 ||
		manifest.DevelopmentCases <= 0 ||
		manifest.HoldoutCases <= 0 {
		return fmt.Errorf("gold closure manifest corpus counts must be positive")
	}

	role := ""
	switch canonical.SHA256 {
	case manifest.DevelopmentCorpusSHA256:
		if len(canonical.Records) != manifest.DevelopmentCases {
			return fmt.Errorf(
				"development corpus has %d cases, manifest says %d",
				len(canonical.Records),
				manifest.DevelopmentCases,
			)
		}
		role = "development"
	case manifest.HoldoutCorpusSHA256:
		if len(canonical.Records) != manifest.HoldoutCases {
			return fmt.Errorf(
				"holdout corpus has %d cases, manifest says %d",
				len(canonical.Records),
				manifest.HoldoutCases,
			)
		}
		role = "holdout"
	default:
		return fmt.Errorf(
			"corpus SHA-256 %s is neither final development nor holdout in gold closure manifest",
			canonical.SHA256,
		)
	}

	if len(manifest.ReviewWorkflows) != 2 ||
		manifest.ReviewWorkflows[0].Workflow != ReviewWorkflowMatcher ||
		manifest.ReviewWorkflows[1].Workflow != ReviewWorkflowContentJunk {
		return fmt.Errorf(
			"gold closure manifest must contain matcher then content_junk review proofs",
		)
	}
	workflows := make(map[ReviewWorkflow]GoldClosureReviewSummary, 2)
	for i, summary := range manifest.ReviewWorkflows {
		if err := validateClosureReviewSummary(summary); err != nil {
			return fmt.Errorf(
				"gold closure manifest review_workflows[%d]: %w",
				i,
				err,
			)
		}
		workflows[summary.Workflow] = summary
	}
	if len(manifest.Tasks) != len(orderedTasks) {
		return fmt.Errorf(
			"gold closure manifest has %d task summaries, want %d",
			len(manifest.Tasks),
			len(orderedTasks),
		)
	}
	taskSummaries := make(map[Task]GoldClosureTaskSummary, len(manifest.Tasks))
	for i, summary := range manifest.Tasks {
		if summary.Task != orderedTasks[i] {
			return fmt.Errorf(
				"gold closure manifest tasks must use canonical task order",
			)
		}
		if err := validateSHA256(
			fmt.Sprintf("gold closure manifest.tasks[%d].source_snapshot_sha256", i),
			summary.SourceSnapshotSHA256,
		); err != nil {
			return err
		}
		if summary.DevelopmentCases <= 0 || summary.HoldoutCases <= 0 {
			return fmt.Errorf(
				"gold closure manifest task %q has non-positive final counts",
				summary.Task,
			)
		}
		taskSummaries[summary.Task] = summary
	}

	actualByTask := make(map[Task]int)
	for _, record := range canonical.Records {
		summary, ok := taskSummaries[record.Task]
		if !ok {
			return fmt.Errorf(
				"corpus case %q task %q is absent from closure manifest",
				record.CaseID,
				record.Task,
			)
		}
		if record.PrivacyAttestation == nil ||
			record.PrivacyAttestation.PlanID != manifest.PlanID ||
			record.PrivacyAttestation.SourceSnapshotSHA256 !=
				summary.SourceSnapshotSHA256 {
			return fmt.Errorf(
				"corpus case %q privacy proof does not match closure plan/task source snapshot",
				record.CaseID,
			)
		}
		workflow := ReviewWorkflowContentJunk
		if record.Task == TaskMatcherExtract || record.Task == TaskMatcherRerank {
			workflow = ReviewWorkflowMatcher
		}
		review := workflows[workflow]
		if record.Label.HumanReviewProof == nil ||
			record.Label.SourceRef != review.ReviewSetID ||
			!reviewSummaryMatchesProof(review, *record.Label.HumanReviewProof) {
			return fmt.Errorf(
				"corpus case %q human-review proof does not match closure manifest",
				record.CaseID,
			)
		}
		actualByTask[record.Task]++
	}
	for _, task := range orderedTasks {
		summary := taskSummaries[task]
		expected := summary.DevelopmentCases
		if role == "holdout" {
			expected = summary.HoldoutCases
		}
		if actualByTask[task] != expected {
			return fmt.Errorf(
				"%s corpus task %q has %d cases, manifest says %d",
				role,
				task,
				actualByTask[task],
				expected,
			)
		}
	}
	return nil
}

// ValidateGoldExecutionPrivacySidecar proves that an exact local sidecar is
// the one bound through gold closure and covers every case in the selected
// final corpus. Extra entries are expected because closure selects a disjoint
// subset from the complete production candidate corpus.
func ValidateGoldExecutionPrivacySidecar(
	corpus Corpus,
	manifest GoldClosureManifest,
	sidecar ProductionPrivacySidecar,
	sidecarSHA256 string,
) error {
	if err := ValidateGoldClosureCorpus(corpus, manifest); err != nil {
		return err
	}
	if err := validateSHA256(
		"production privacy sidecar sha256",
		sidecarSHA256,
	); err != nil {
		return err
	}
	if sidecarSHA256 != manifest.PrivacySidecarSHA256 {
		return fmt.Errorf(
			"production privacy sidecar bytes do not match the gold closure manifest",
		)
	}
	if err := validateProductionPrivacySidecarStructure(sidecar); err != nil {
		return err
	}
	if sidecar.PlanID != manifest.PlanID ||
		sidecar.PlanSHA256 != manifest.PlanSHA256 ||
		sidecar.CandidateCorpusSHA256 != manifest.CandidateCorpusSHA256 {
		return fmt.Errorf(
			"production privacy sidecar does not match the gold closure source corpus",
		)
	}
	entries := make(map[string]ProductionPrivacySidecarEntry, len(sidecar.Entries))
	for _, entry := range sidecar.Entries {
		entries[entry.CaseID] = entry
	}
	for _, record := range corpus.Records {
		entry, ok := entries[record.CaseID]
		if !ok || entry.Task != record.Task {
			return fmt.Errorf(
				"production privacy sidecar does not cover every selected gold case",
			)
		}
	}
	return nil
}

func validateClosureReviewSummary(summary GoldClosureReviewSummary) error {
	if summary.Workflow != ReviewWorkflowMatcher &&
		summary.Workflow != ReviewWorkflowContentJunk {
		return fmt.Errorf("unsupported workflow %q", summary.Workflow)
	}
	for name, value := range map[string]string{
		"review_set_id":        summary.ReviewSetID,
		"policy_id":            summary.PolicyID,
		"evidence_manifest_id": summary.EvidenceManifestID,
	} {
		if err := validateIdentifier(name, value); err != nil {
			return err
		}
	}
	if err := validateTag("policy_version", summary.PolicyVersion); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"policy_sha256":            summary.PolicySHA256,
		"evidence_manifest_sha256": summary.EvidenceManifestSHA256,
		"evidence_corpus_sha256":   summary.EvidenceCorpusSHA256,
		"source_snapshot_sha256":   summary.SourceSnapshotSHA256,
	} {
		if err := validateSHA256(name, value); err != nil {
			return err
		}
	}
	if summary.ReviewedCases <= 0 ||
		summary.PromotedGoldCases <= 0 ||
		summary.AdjudicatedCases < 0 ||
		summary.UnresolvedCases < 0 ||
		summary.PromotedGoldCases+summary.UnresolvedCases !=
			summary.ReviewedCases {
		return fmt.Errorf("review case counts are inconsistent")
	}
	return summary.Artifacts.validate(summary.AdjudicatedCases > 0)
}

func reviewSummaryMatchesProof(
	summary GoldClosureReviewSummary,
	proof HumanReviewProof,
) bool {
	return proof.Workflow == summary.Workflow &&
		proof.PolicyID == summary.PolicyID &&
		proof.PolicyVersion == summary.PolicyVersion &&
		proof.PolicySHA256 == summary.PolicySHA256 &&
		proof.EvidenceManifestID == summary.EvidenceManifestID &&
		proof.EvidenceManifestSHA256 == summary.EvidenceManifestSHA256 &&
		proof.EvidenceCorpusSHA256 == summary.EvidenceCorpusSHA256 &&
		proof.SourceSnapshotSHA256 == summary.SourceSnapshotSHA256
}

func validateClosurePromotion(
	candidates Corpus,
	tasks []Task,
	view goldClosurePromotionView,
) (
	map[string]CorpusRecord,
	GoldClosureReviewSummary,
	error,
) {
	if view.workflow != ReviewWorkflowMatcher &&
		view.workflow != ReviewWorkflowContentJunk {
		return nil, GoldClosureReviewSummary{}, fmt.Errorf(
			"unsupported workflow %q",
			view.workflow,
		)
	}
	if view.corpusSHA256 != candidates.SHA256 {
		return nil, GoldClosureReviewSummary{}, fmt.Errorf(
			"agreement corpus_sha256 does not match candidate corpus",
		)
	}
	if err := validateIdentifier("review_set_id", view.reviewSetID); err != nil {
		return nil, GoldClosureReviewSummary{}, err
	}
	if view.reviewerA == view.reviewerB {
		return nil, GoldClosureReviewSummary{}, fmt.Errorf(
			"primary reviewers must be distinct",
		)
	}
	if err := view.proof.Validate(); err != nil {
		return nil, GoldClosureReviewSummary{}, fmt.Errorf("review proof: %w", err)
	}
	if view.proof.Workflow != view.workflow ||
		view.proof.PolicyVersion != view.policyVersion ||
		view.proof.EvidenceCorpusSHA256 != candidates.SHA256 {
		return nil, GoldClosureReviewSummary{}, fmt.Errorf(
			"agreement review proof does not match workflow, policy, and candidate corpus",
		)
	}

	expectedByTask := make(map[Task]int, len(tasks))
	candidateByID := make(map[string]CorpusRecord)
	for _, record := range candidates.Records {
		if !containsTask(tasks, record.Task) {
			continue
		}
		expectedByTask[record.Task]++
		candidateByID[record.CaseID] = record
		if record.PrivacyAttestation == nil ||
			record.PrivacyAttestation.SourceSnapshotSHA256 !=
				view.proof.SourceSnapshotSHA256 {
			return nil, GoldClosureReviewSummary{}, fmt.Errorf(
				"candidate case %q privacy snapshot does not match review proof",
				record.CaseID,
			)
		}
	}
	if view.overall.Cases != len(candidateByID) {
		return nil, GoldClosureReviewSummary{}, fmt.Errorf(
			"agreement covers %d cases, want %d",
			view.overall.Cases,
			len(candidateByID),
		)
	}
	if view.adjudicated != view.overall.NeedsAdjudication {
		return nil, GoldClosureReviewSummary{}, fmt.Errorf(
			"adjudicated case count %d does not match required %d",
			view.adjudicated,
			view.overall.NeedsAdjudication,
		)
	}
	if err := validateAgreementTaskCoverage(view.byTask, expectedByTask); err != nil {
		return nil, GoldClosureReviewSummary{}, err
	}
	needs, err := validatedCaseIDSet(
		"needs_adjudication_case_ids",
		view.needsIDs,
		candidateByID,
	)
	if err != nil {
		return nil, GoldClosureReviewSummary{}, err
	}
	if len(needs) != view.overall.NeedsAdjudication {
		return nil, GoldClosureReviewSummary{}, fmt.Errorf(
			"needs-adjudication ID count %d does not match agreement count %d",
			len(needs),
			view.overall.NeedsAdjudication,
		)
	}
	unresolved, err := validatedCaseIDSet(
		"unresolved_case_ids",
		view.unresolvedIDs,
		candidateByID,
	)
	if err != nil {
		return nil, GoldClosureReviewSummary{}, err
	}
	for caseID := range unresolved {
		if _, ok := needs[caseID]; !ok {
			return nil, GoldClosureReviewSummary{}, fmt.Errorf(
				"unresolved case %q was not independently adjudicated",
				caseID,
			)
		}
	}
	if err := view.artifacts.validate(len(needs) > 0); err != nil {
		return nil, GoldClosureReviewSummary{}, err
	}

	goldByID := make(map[string]CorpusRecord)
	if view.gold != nil {
		canonical, err := validateFrozenReviewSource(*view.gold)
		if err != nil {
			return nil, GoldClosureReviewSummary{}, fmt.Errorf(
				"promoted gold corpus: %w",
				err,
			)
		}
		for _, gold := range canonical.Records {
			candidate, ok := candidateByID[gold.CaseID]
			if !ok {
				return nil, GoldClosureReviewSummary{}, fmt.Errorf(
					"promoted gold case %q is outside workflow candidate set",
					gold.CaseID,
				)
			}
			if _, unresolvedCase := unresolved[gold.CaseID]; unresolvedCase {
				return nil, GoldClosureReviewSummary{}, fmt.Errorf(
					"case %q is both promoted and unresolved",
					gold.CaseID,
				)
			}
			if err := validateGoldRecordBinding(candidate, gold, view, needs); err != nil {
				return nil, GoldClosureReviewSummary{}, err
			}
			goldByID[gold.CaseID] = gold
		}
	}
	if len(goldByID)+len(unresolved) != len(candidateByID) {
		return nil, GoldClosureReviewSummary{}, fmt.Errorf(
			"promotion partition has %d gold plus %d unresolved cases, want %d",
			len(goldByID),
			len(unresolved),
			len(candidateByID),
		)
	}
	for caseID := range candidateByID {
		if _, ok := goldByID[caseID]; ok {
			continue
		}
		if _, ok := unresolved[caseID]; !ok {
			return nil, GoldClosureReviewSummary{}, fmt.Errorf(
				"candidate case %q is neither promoted nor unresolved",
				caseID,
			)
		}
	}

	return goldByID, GoldClosureReviewSummary{
		Workflow:               view.workflow,
		ReviewSetID:            view.reviewSetID,
		PolicyID:               view.proof.PolicyID,
		PolicyVersion:          view.proof.PolicyVersion,
		PolicySHA256:           view.proof.PolicySHA256,
		EvidenceManifestID:     view.proof.EvidenceManifestID,
		EvidenceManifestSHA256: view.proof.EvidenceManifestSHA256,
		EvidenceCorpusSHA256:   view.proof.EvidenceCorpusSHA256,
		SourceSnapshotSHA256:   view.proof.SourceSnapshotSHA256,
		ReviewedCases:          len(candidateByID),
		PromotedGoldCases:      len(goldByID),
		AdjudicatedCases:       view.adjudicated,
		UnresolvedCases:        len(unresolved),
		Artifacts:              view.artifacts,
	}, nil
}

func (h GoldClosureReviewArtifactHashes) validate(adjudicationRequired bool) error {
	for name, value := range map[string]string{
		"primary_assignment_a_sha256": h.PrimaryAssignmentASHA256,
		"primary_submission_a_sha256": h.PrimarySubmissionASHA256,
		"primary_assignment_b_sha256": h.PrimaryAssignmentBSHA256,
		"primary_submission_b_sha256": h.PrimarySubmissionBSHA256,
	} {
		if err := validateSHA256(name, value); err != nil {
			return err
		}
	}
	if h.PrimaryAssignmentASHA256 == h.PrimaryAssignmentBSHA256 ||
		h.PrimarySubmissionASHA256 == h.PrimarySubmissionBSHA256 {
		return fmt.Errorf(
			"primary review artifact hashes must prove two distinct assignments and submissions",
		)
	}
	hasAssignment := h.AdjudicationAssignmentSHA256 != ""
	hasSubmission := h.AdjudicationSubmissionSHA256 != ""
	if hasAssignment != hasSubmission {
		return fmt.Errorf(
			"adjudication assignment and submission hashes must be supplied together",
		)
	}
	if adjudicationRequired && !hasAssignment {
		return fmt.Errorf(
			"adjudication artifact hashes are required when cases needed adjudication",
		)
	}
	if !adjudicationRequired && hasAssignment {
		return fmt.Errorf(
			"adjudication artifact hashes supplied when no case needed adjudication",
		)
	}
	if hasAssignment {
		if err := validateSHA256(
			"adjudication_assignment_sha256",
			h.AdjudicationAssignmentSHA256,
		); err != nil {
			return err
		}
		if err := validateSHA256(
			"adjudication_submission_sha256",
			h.AdjudicationSubmissionSHA256,
		); err != nil {
			return err
		}
	}
	return nil
}

func validateAgreementTaskCoverage(
	actual []TaskAgreementStats,
	expected map[Task]int,
) error {
	if len(actual) != len(expected) {
		return fmt.Errorf(
			"agreement by_task has %d tasks, want %d",
			len(actual),
			len(expected),
		)
	}
	seen := make(map[Task]struct{}, len(actual))
	for _, summary := range actual {
		want, ok := expected[summary.Task]
		if !ok {
			return fmt.Errorf(
				"agreement contains unexpected task %q",
				summary.Task,
			)
		}
		if _, duplicate := seen[summary.Task]; duplicate {
			return fmt.Errorf("agreement contains duplicate task %q", summary.Task)
		}
		if summary.Cases != want {
			return fmt.Errorf(
				"agreement task %q covers %d cases, want %d",
				summary.Task,
				summary.Cases,
				want,
			)
		}
		seen[summary.Task] = struct{}{}
	}
	return nil
}

func validatedCaseIDSet(
	name string,
	ids []string,
	allowed map[string]CorpusRecord,
) (map[string]struct{}, error) {
	out := make(map[string]struct{}, len(ids))
	previous := ""
	for i, caseID := range ids {
		if err := validateIdentifier(fmt.Sprintf("%s[%d]", name, i), caseID); err != nil {
			return nil, err
		}
		if i > 0 && caseID <= previous {
			return nil, fmt.Errorf("%s must be sorted and unique", name)
		}
		previous = caseID
		if _, ok := allowed[caseID]; !ok {
			return nil, fmt.Errorf("%s case %q is outside workflow", name, caseID)
		}
		out[caseID] = struct{}{}
	}
	return out, nil
}

func validateGoldRecordBinding(
	candidate,
	gold CorpusRecord,
	view goldClosurePromotionView,
	needs map[string]struct{},
) error {
	if candidate.SchemaVersion != gold.SchemaVersion ||
		candidate.CaseID != gold.CaseID ||
		candidate.Task != gold.Task ||
		candidate.GroupID != gold.GroupID ||
		!reflect.DeepEqual(candidate.SliceIDs, gold.SliceIDs) ||
		!reflect.DeepEqual(candidate.PrivacyAttestation, gold.PrivacyAttestation) ||
		!sameCorpusRecordInput(candidate, gold) {
		return fmt.Errorf(
			"promoted gold case %q changed frozen input, grouping, slices, or privacy metadata",
			gold.CaseID,
		)
	}
	if gold.Label.Provenance != LabelProvenanceHumanReview ||
		gold.Label.Strength != LabelStrengthGold ||
		gold.Label.SourceRef != view.reviewSetID ||
		gold.Label.PolicyVersion != view.policyVersion ||
		gold.Label.HumanReviewProof == nil ||
		!reflect.DeepEqual(*gold.Label.HumanReviewProof, view.proof) {
		return fmt.Errorf(
			"promoted gold case %q label is not bound to the review agreement",
			gold.CaseID,
		)
	}
	_, wasAdjudicated := needs[gold.CaseID]
	wantReviewers := 2
	if wasAdjudicated {
		wantReviewers = 3
	}
	if gold.Label.ReviewerCount != wantReviewers {
		return fmt.Errorf(
			"promoted gold case %q reviewer_count is %d, want %d",
			gold.CaseID,
			gold.Label.ReviewerCount,
			wantReviewers,
		)
	}
	return nil
}

func sameCorpusRecordInput(left, right CorpusRecord) bool {
	switch left.Task {
	case TaskMatcherExtract:
		return left.MatcherExtract != nil &&
			right.MatcherExtract != nil &&
			reflect.DeepEqual(left.MatcherExtract.Input, right.MatcherExtract.Input)
	case TaskMatcherRerank:
		return left.MatcherRerank != nil &&
			right.MatcherRerank != nil &&
			reflect.DeepEqual(left.MatcherRerank.Input, right.MatcherRerank.Input)
	case TaskContentFilter:
		return left.ContentFilter != nil &&
			right.ContentFilter != nil &&
			reflect.DeepEqual(left.ContentFilter.Input, right.ContentFilter.Input)
	case TaskJunkPurge:
		return left.JunkPurge != nil &&
			right.JunkPurge != nil &&
			reflect.DeepEqual(left.JunkPurge.Input, right.JunkPurge.Input)
	default:
		return false
	}
}

func developmentStratumTargets(
	development Corpus,
	plan CorpusPlan,
) (map[Task]map[string]int, error) {
	taskPlans := make(map[Task]CorpusPlanTask, len(plan.Tasks))
	for _, taskPlan := range plan.Tasks {
		taskPlans[taskPlan.Task] = taskPlan
	}
	observed := make(map[Task]map[string]int, len(plan.Tasks))
	for _, record := range development.Records {
		taskPlan := taskPlans[record.Task]
		suite, err := recordPrimarySuite(
			record,
			plan.Selection.PrimarySuiteSlicePrefix,
			taskPlan.HoldoutAllocation,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"candidate development case %q: %w",
				record.CaseID,
				err,
			)
		}
		if observed[record.Task] == nil {
			observed[record.Task] = make(map[string]int)
		}
		observed[record.Task][suite]++
	}
	out := make(map[Task]map[string]int, len(plan.Tasks))
	for _, taskPlan := range plan.Tasks {
		phaseBound := false
		for _, record := range development.Records {
			if record.Task != taskPlan.Task {
				continue
			}
			phase, err := recordReviewPhase(record)
			if err != nil {
				return nil, err
			}
			phaseBound = phaseBound ||
				phase == CorpusReviewPhaseDevelopment
		}
		for suite, target := range taskPlan.DevelopmentAllocation {
			minimumReserve := target
			if phaseBound {
				minimumReserve +=
					taskPlan.DevelopmentReplacementReserve[suite]
			}
			if observed[taskPlan.Task][suite] < minimumReserve {
				return nil, fmt.Errorf(
					"candidate development task %q suite %q has %d reviewed candidates, below target plus replacement reserve %d",
					taskPlan.Task,
					suite,
					observed[taskPlan.Task][suite],
					minimumReserve,
				)
			}
		}
		out[taskPlan.Task] = cloneStringIntMap(
			taskPlan.DevelopmentAllocation,
		)
	}
	return out, nil
}

func buildGoldClosureGroups(
	plan CorpusPlan,
	candidates Corpus,
	goldByID map[string]CorpusRecord,
) (map[Task][]*goldClosureGroup, error) {
	taskPlans := make(map[Task]CorpusPlanTask, len(plan.Tasks))
	for _, taskPlan := range plan.Tasks {
		taskPlans[taskPlan.Task] = taskPlan
	}
	groupsByTask := make(map[Task][]*goldClosureGroup, len(plan.Tasks))
	index := make(map[string]*goldClosureGroup)
	for _, candidate := range candidates.Records {
		taskPlan := taskPlans[candidate.Task]
		suite, err := recordPrimarySuite(
			candidate,
			plan.Selection.PrimarySuiteSlicePrefix,
			taskPlan.HoldoutAllocation,
		)
		if err != nil {
			return nil, fmt.Errorf("candidate case %q: %w", candidate.CaseID, err)
		}
		key := string(candidate.Task) + "\x00" + candidate.GroupID
		group := index[key]
		if group == nil {
			group = &goldClosureGroup{
				task:     candidate.Task,
				groupID:  candidate.GroupID,
				suite:    suite,
				hash:     splitGroupHash(plan.PlanID, candidate.Task, candidate.GroupID),
				eligible: true,
			}
			index[key] = group
			groupsByTask[candidate.Task] = append(groupsByTask[candidate.Task], group)
		} else if group.suite != suite {
			return nil, fmt.Errorf(
				"candidate task %q group %q spans primary suites",
				candidate.Task,
				candidate.GroupID,
			)
		}
		group.candidates = append(group.candidates, candidate)
		gold, ok := goldByID[candidate.CaseID]
		if !ok {
			group.eligible = false
		} else {
			group.gold = append(group.gold, gold)
		}
	}
	for _, task := range orderedTasks {
		sort.Slice(groupsByTask[task], func(i, j int) bool {
			comparison := bytes.Compare(
				groupsByTask[task][i].hash[:],
				groupsByTask[task][j].hash[:],
			)
			if comparison != 0 {
				return comparison < 0
			}
			return groupsByTask[task][i].groupID <
				groupsByTask[task][j].groupID
		})
	}
	return groupsByTask, nil
}

func selectGoldDevelopmentGroups(
	taskPlan CorpusPlanTask,
	groups []*goldClosureGroup,
	targets map[string]int,
) (
	selected []*goldClosureGroup,
	remaining []*goldClosureGroup,
	err error,
) {
	counts := make(map[string]int, len(targets))
	selectedSet := make(map[*goldClosureGroup]struct{})
	for _, group := range groups {
		if !group.eligible || counts[group.suite] >= targets[group.suite] {
			continue
		}
		selected = append(selected, group)
		selectedSet[group] = struct{}{}
		counts[group.suite]++
	}
	for suite, target := range targets {
		if counts[suite] < target {
			return nil, nil, fmt.Errorf(
				"development stratum %q has %d independently grouped reviewed-gold representatives, below frozen target %d",
				suite,
				counts[suite],
				target,
			)
		}
	}
	if len(selected) != taskPlan.DevelopmentTarget {
		return nil, nil, fmt.Errorf(
			"reviewed-gold replacements selected %d development groups, want exactly %d",
			len(selected),
			taskPlan.DevelopmentTarget,
		)
	}
	for _, group := range groups {
		if _, used := selectedSet[group]; !used {
			remaining = append(remaining, group)
		}
	}
	return selected, remaining, nil
}

func selectGoldHoldoutGroups(
	taskPlan CorpusPlanTask,
	remaining []*goldClosureGroup,
) (
	[]*goldClosureGroup,
	map[string][]*goldClosureGroup,
	error,
) {
	available := make(map[string][]*goldClosureGroup)
	for _, group := range remaining {
		if group.eligible {
			available[group.suite] = append(available[group.suite], group)
		}
	}
	selectedBySuite := make(map[string][]*goldClosureGroup)
	nextBySuite := make(map[string]int)
	for _, suite := range sortedCountMapKeys(taskPlan.HoldoutAllocation) {
		minimum := taskPlan.HoldoutAllocation[suite]
		for groupGoldCaseCount(selectedBySuite[suite]) < minimum {
			index := nextBySuite[suite]
			if index >= len(available[suite]) {
				return nil, nil, fmt.Errorf(
					"holdout suite %q has %d reviewed-gold cases, below minimum %d",
					suite,
					groupGoldCaseCount(selectedBySuite[suite]),
					minimum,
				)
			}
			selectedBySuite[suite] = append(
				selectedBySuite[suite],
				available[suite][index],
			)
			nextBySuite[suite]++
		}
	}

	topUpSuite := ""
	switch taskPlan.Task {
	case TaskMatcherExtract, TaskMatcherRerank, TaskContentFilter:
		topUpSuite = "safety"
	case TaskJunkPurge:
		topUpSuite = "safety_top_up"
	}
	for {
		selected := flattenGoldClosureGroups(selectedBySuite)
		missingLabel, err := missingGoldQuotas(taskPlan, selected)
		if err != nil {
			return nil, nil, err
		}
		missingEligible, err := missingGoldEligibleGroups(
			taskPlan,
			selected,
		)
		if err != nil {
			return nil, nil, err
		}
		if len(missingLabel) == 0 && len(missingEligible) == 0 {
			return selected, selectedBySuite, nil
		}
		neededSuites := make(map[string]struct{})
		if len(missingLabel) != 0 {
			neededSuites[topUpSuite] = struct{}{}
		}
		for _, quota := range missingEligible {
			neededSuites[goldEligibleQuotaSuite(taskPlan, quota)] =
				struct{}{}
		}
		for _, suite := range sortedStringSetKeys(neededSuites) {
			index := nextBySuite[suite]
			if index >= len(available[suite]) {
				return nil, nil, fmt.Errorf(
					"holdout is inconclusive: cannot satisfy reviewed-gold quotas; label quotas %v and eligible-group quotas %v remain after exhausting suite %q",
					missingLabel,
					missingEligible,
					suite,
				)
			}
			selectedBySuite[suite] = append(
				selectedBySuite[suite],
				available[suite][index],
			)
			nextBySuite[suite]++
		}
	}
}

func missingGoldEligibleGroups(
	taskPlan CorpusPlanTask,
	groups []*goldClosureGroup,
) ([]string, error) {
	counts, err := goldEligibleGroupCounts(taskPlan, groups)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, quota := range sortedCountMapKeys(
		taskPlan.RequiredGoldEligibleGroups,
	) {
		if counts[quota] < taskPlan.RequiredGoldEligibleGroups[quota] {
			missing = append(missing, quota)
		}
	}
	return missing, nil
}

func goldEligibleGroupCounts(
	taskPlan CorpusPlanTask,
	groups []*goldClosureGroup,
) (map[string]int, error) {
	counts := make(map[string]int, len(taskPlan.RequiredGoldEligibleGroups))
	for quota := range taskPlan.RequiredGoldEligibleGroups {
		counts[quota] = 0
	}
	for _, group := range groups {
		harmEligible := false
		successEligible := false
		for _, record := range group.gold {
			recordHarm, recordSuccess, err :=
				goldRecordPromotionEligibility(record)
			if err != nil {
				return nil, err
			}
			harmEligible = harmEligible || recordHarm
			successEligible = successEligible || recordSuccess
		}
		if group.suite == "natural" {
			if harmEligible {
				counts[GoldQuotaNaturalPrimaryHarmGroups]++
			}
			if successEligible {
				counts[GoldQuotaNaturalTaskSuccessGroups]++
			}
			continue
		}
		if harmEligible {
			counts[GoldQuotaSafetyPrimaryHarmGroups]++
		}
		if successEligible {
			counts[GoldQuotaSafetyTaskSuccessGroups]++
		}
	}
	return counts, nil
}

func goldRecordPromotionEligibility(
	record CorpusRecord,
) (harm bool, success bool, err error) {
	switch record.Task {
	case TaskMatcherExtract:
		return true,
			len(record.MatcherExtract.Expected.Acceptable) > 0 &&
				!record.MatcherExtract.Expected.AllowAbstain,
			nil
	case TaskMatcherRerank:
		return true,
			len(record.MatcherRerank.Expected.AcceptableTMDBIDs) > 0 &&
				!record.MatcherRerank.Expected.AllowAbstain,
			nil
	case TaskContentFilter:
		return !record.ContentFilter.Expected.AllowAbstain &&
				record.ContentFilter.Expected.Language == LanguageEnglish,
			true,
			nil
	case TaskJunkPurge:
		// Harm-eligible means keep-disposition, not "resolves against a
		// catalogue". See internal/llmeval/junk_disposition.go.
		return record.JunkPurge.Expected.Disposition == JunkDispositionKeep,
			true,
			nil
	default:
		return false, false, fmt.Errorf(
			"unsupported promotion-eligibility task %q",
			record.Task,
		)
	}
}

func goldEligibleQuotaSuite(
	taskPlan CorpusPlanTask,
	quota string,
) string {
	switch quota {
	case GoldQuotaNaturalPrimaryHarmGroups,
		GoldQuotaNaturalTaskSuccessGroups:
		return "natural"
	case GoldQuotaSafetyPrimaryHarmGroups,
		GoldQuotaSafetyTaskSuccessGroups:
		if taskPlan.Task == TaskJunkPurge {
			return "safety_top_up"
		}
		return "safety"
	default:
		return ""
	}
}

func sortedStringSetKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func missingGoldQuotas(
	taskPlan CorpusPlanTask,
	groups []*goldClosureGroup,
) ([]string, error) {
	counts, _, err := goldQuotaCounts(taskPlan, groups)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, quota := range requiredGoldQuotaNames(taskPlan) {
		minimum := goldQuotaMinimum(taskPlan, quota)
		if counts[quota] < minimum {
			missing = append(
				missing,
				fmt.Sprintf("%s<%d", quota, minimum),
			)
		}
	}
	return missing, nil
}

func goldQuotaCounts(
	taskPlan CorpusPlanTask,
	groups []*goldClosureGroup,
) (map[string]int, map[string]int, error) {
	names := requiredGoldQuotaNames(taskPlan)
	counts := make(map[string]int, len(names))
	contradictions := make(map[string]int, len(names))
	for _, name := range names {
		counts[name] = 0
		contradictions[name] = 0
	}
	for _, group := range groups {
		if (taskPlan.Task == TaskMatcherExtract ||
			taskPlan.Task == TaskMatcherRerank ||
			taskPlan.Task == TaskContentFilter) &&
			group.suite != "safety" {
			continue
		}
		for _, record := range group.gold {
			for _, quota := range names {
				if !containsString(record.SliceIDs, quota) {
					continue
				}
				matches, err := recordMatchesGoldQuota(record, quota)
				if err != nil {
					return nil, nil, err
				}
				if matches {
					counts[quota]++
				} else {
					contradictions[quota]++
				}
			}
		}
	}
	return counts, contradictions, nil
}

func recordMatchesGoldQuota(record CorpusRecord, quota string) (bool, error) {
	switch record.Task {
	case TaskMatcherExtract:
		return true, nil
	case TaskMatcherRerank:
		switch quota {
		case "correct_candidate_present":
			return len(record.MatcherRerank.Expected.AcceptableTMDBIDs) > 0, nil
		case "no_correct_candidate":
			return record.MatcherRerank.Expected.AllowAbstain &&
				len(record.MatcherRerank.Expected.AcceptableTMDBIDs) == 0, nil
		default:
			return true, nil
		}
	case TaskContentFilter:
		switch quota {
		case "hard_must_keep_english":
			return !record.ContentFilter.Expected.AllowAbstain &&
				record.ContentFilter.Expected.Language == LanguageEnglish, nil
		case "latin_script_non_english":
			return !record.ContentFilter.Expected.AllowAbstain &&
				record.ContentFilter.Expected.Language == LanguageNonEnglish, nil
		default:
			return false, fmt.Errorf(
				"contentfilter reviewed-gold quota %q has no executable label rule",
				quota,
			)
		}
	case TaskJunkPurge:
		switch quota {
		// Quota keys are plan-config identifiers matched against SliceIDs, so
		// the v1 names stay readable by a v1 plan. corpus-plan v2 renames them
		// to human_verified_keep_worthy / representative_delete.
		case "human_verified_real_movie_or_tv", "human_verified_keep_worthy":
			return record.JunkPurge.Expected.Disposition ==
				JunkDispositionKeep, nil
		case "representative_junk", "representative_delete":
			return record.JunkPurge.Expected.Disposition ==
				JunkDispositionDelete, nil
		default:
			return false, fmt.Errorf(
				"junkpurge reviewed-gold quota %q has no executable label rule",
				quota,
			)
		}
	default:
		return false, fmt.Errorf("unsupported task %q", record.Task)
	}
}

func requiredGoldQuotaNames(taskPlan CorpusPlanTask) []string {
	switch taskPlan.Task {
	case TaskMatcherExtract, TaskMatcherRerank:
		return append([]string(nil), taskPlan.RequiredSafetySlices...)
	case TaskContentFilter:
		return sortedCountMapKeys(taskPlan.RequiredSafetyCounts)
	case TaskJunkPurge:
		return sortedCountMapKeys(taskPlan.RequiredUnionCounts)
	default:
		return nil
	}
}

func goldQuotaMinimum(taskPlan CorpusPlanTask, quota string) int {
	switch taskPlan.Task {
	case TaskMatcherExtract, TaskMatcherRerank:
		return 1
	case TaskContentFilter:
		return taskPlan.RequiredSafetyCounts[quota]
	case TaskJunkPurge:
		return taskPlan.RequiredUnionCounts[quota]
	default:
		return 0
	}
}

func summarizeGoldClosureTask(
	taskPlan CorpusPlanTask,
	freeze CorpusFreezeManifest,
	groups,
	development,
	holdout []*goldClosureGroup,
	holdoutBySuite map[string][]*goldClosureGroup,
	developmentTargets map[string]int,
) GoldClosureTaskSummary {
	selected := make(map[*goldClosureGroup]struct{}, len(development)+len(holdout))
	for _, group := range development {
		selected[group] = struct{}{}
	}
	for _, group := range holdout {
		selected[group] = struct{}{}
	}
	candidateCases := 0
	eligibleCases := 0
	eligibleGroups := 0
	unresolvedCases := 0
	excludedGroups := 0
	unselectedCases := 0
	unselectedGroups := 0
	for _, group := range groups {
		candidateCases += len(group.candidates)
		if !group.eligible {
			excludedGroups++
			unresolvedCases += len(group.candidates) - len(group.gold)
			continue
		}
		eligibleGroups++
		eligibleCases += len(group.gold)
		if _, ok := selected[group]; !ok {
			unselectedGroups++
			unselectedCases += len(group.gold)
		}
	}
	counts, contradictions, _ := goldQuotaCounts(taskPlan, holdout)
	eligibleGroupCounts, _ := goldEligibleGroupCounts(taskPlan, holdout)
	allocations := make([]GoldClosureSuiteSummary, 0, len(taskPlan.HoldoutAllocation))
	for _, suite := range sortedCountMapKeys(taskPlan.HoldoutAllocation) {
		allocations = append(allocations, GoldClosureSuiteSummary{
			Suite:        suite,
			MinimumCases: taskPlan.HoldoutAllocation[suite],
			ActualCases:  groupGoldCaseCount(holdoutBySuite[suite]),
			Groups:       len(holdoutBySuite[suite]),
		})
	}
	sourceSnapshot := ""
	for _, summary := range freeze.Tasks {
		if summary.Task == taskPlan.Task {
			sourceSnapshot = summary.SourceSnapshotSHA256
			break
		}
	}
	return GoldClosureTaskSummary{
		Task:                      taskPlan.Task,
		SourceSnapshotSHA256:      sourceSnapshot,
		CandidateCases:            candidateCases,
		CandidateGroups:           len(groups),
		EligibleGoldCases:         eligibleCases,
		EligibleGoldGroups:        eligibleGroups,
		UnresolvedCases:           unresolvedCases,
		ExcludedUnresolvedGroups:  excludedGroups,
		DevelopmentTargetMaximum:  taskPlan.DevelopmentTarget,
		DevelopmentStratumTargets: cloneStringIntMap(developmentTargets),
		DevelopmentCases:          len(development),
		DevelopmentGroups:         len(development),
		HoldoutTargetMinimum:      taskPlan.HoldoutTarget,
		HoldoutCases:              groupGoldCaseCount(holdout),
		HoldoutGroups:             len(holdout),
		HoldoutAllocations:        allocations,
		RequiredGoldEligibleGroups: cloneStringIntMap(
			taskPlan.RequiredGoldEligibleGroups,
		),
		ActualGoldEligibleGroups: cloneStringIntMap(
			eligibleGroupCounts,
		),
		RequiredGoldQuotaCounts:  omitEmptyCountMap(counts),
		ContradictedQuotaTags:    omitEmptyCountMap(contradictions),
		UnselectedEligibleCases:  unselectedCases,
		UnselectedEligibleGroups: unselectedGroups,
	}
}

func omitEmptyCountMap(values map[string]int) map[string]int {
	for _, value := range values {
		if value != 0 {
			return values
		}
	}
	return nil
}

func cloneStringIntMap(values map[string]int) map[string]int {
	out := make(map[string]int, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func flattenGoldClosureGroups(
	selectedBySuite map[string][]*goldClosureGroup,
) []*goldClosureGroup {
	var groups []*goldClosureGroup
	for _, suiteGroups := range selectedBySuite {
		groups = append(groups, suiteGroups...)
	}
	sort.Slice(groups, func(i, j int) bool {
		comparison := bytes.Compare(groups[i].hash[:], groups[j].hash[:])
		if comparison != 0 {
			return comparison < 0
		}
		return groups[i].groupID < groups[j].groupID
	})
	return groups
}

func appendGoldGroupRecords(
	target []CorpusRecord,
	groups []*goldClosureGroup,
) []CorpusRecord {
	for _, group := range groups {
		target = append(target, group.gold...)
	}
	return target
}

func appendGoldDevelopmentRepresentatives(
	target []CorpusRecord,
	groups []*goldClosureGroup,
) []CorpusRecord {
	for _, group := range groups {
		if len(group.gold) == 0 {
			continue
		}
		representative := group.gold[0]
		for _, record := range group.gold[1:] {
			if record.CaseID < representative.CaseID {
				representative = record
			}
		}
		target = append(target, representative)
	}
	return target
}

func groupGoldCaseCount(groups []*goldClosureGroup) int {
	total := 0
	for _, group := range groups {
		total += len(group.gold)
	}
	return total
}

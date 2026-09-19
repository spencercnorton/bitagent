package llmeval

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

const (
	CorpusFreezeStatus           = "candidate_split_frozen_not_production_export_proof"
	ProductionCorpusFreezeStatus = "production_candidate_split_frozen_export_bound"
	CorpusFreezeScope            = "deterministic split of a caller-supplied, already privacy-filtered candidate corpus; not evidence of a production export"
	ProductionCorpusFreezeScope  = "deterministic development/holdout split bound to one exact privacy-minimized read-only production export manifest; candidate labels remain unreviewed and are not gold"
)

// CorpusFreeze contains deterministic development and holdout corpora plus
// the aggregate, source-text-free manifest that binds the operation.
type CorpusFreeze struct {
	Development Corpus
	Holdout     Corpus
	Manifest    CorpusFreezeManifest
}

// CorpusFreezeManifest is deterministic: it intentionally has no wall-clock
// field. Repeating a freeze with byte-identical plan and candidate artifacts
// produces byte-identical corpus outputs and an equivalent manifest.
type CorpusFreezeManifest struct {
	SchemaVersion                    int                       `json:"schema_version"`
	Status                           string                    `json:"status"`
	Scope                            string                    `json:"scope"`
	PlanID                           string                    `json:"plan_id"`
	PlanSHA256                       string                    `json:"plan_sha256"`
	PlanExecutable                   bool                      `json:"plan_executable"`
	ProductionSourceExporterVerified bool                      `json:"production_source_exporter_verified"`
	ProductionExportManifestSHA256   string                    `json:"production_export_manifest_sha256,omitempty"`
	PrivacySidecarSHA256             string                    `json:"privacy_sidecar_sha256,omitempty"`
	QuotaEvidenceStatus              string                    `json:"quota_evidence_status"`
	SplitAlgorithmID                 string                    `json:"split_algorithm_id"`
	CandidateCorpusSHA256            string                    `json:"candidate_corpus_sha256"`
	CandidateCases                   int                       `json:"candidate_cases"`
	DevelopmentCorpusSHA256          string                    `json:"development_corpus_sha256"`
	DevelopmentCases                 int                       `json:"development_cases"`
	HoldoutCorpusSHA256              string                    `json:"holdout_corpus_sha256"`
	HoldoutCases                     int                       `json:"holdout_cases"`
	Tasks                            []CorpusFreezeTaskSummary `json:"tasks"`
}

type CorpusFreezeTaskSummary struct {
	Task                          Task                       `json:"task"`
	SourceSnapshotSHA256          string                     `json:"source_snapshot_sha256"`
	CandidateCases                int                        `json:"candidate_cases"`
	CandidateGroups               int                        `json:"candidate_groups"`
	DevelopmentTarget             int                        `json:"development_target_independent_groups"`
	DevelopmentAllocation         map[string]int             `json:"development_allocation"`
	DevelopmentReviewReserveCases int                        `json:"development_review_reserve_cases"`
	DevelopmentCases              int                        `json:"development_cases"`
	DevelopmentGroups             int                        `json:"development_groups"`
	HoldoutTarget                 int                        `json:"holdout_target_minimum"`
	HoldoutCases                  int                        `json:"holdout_cases"`
	HoldoutGroups                 int                        `json:"holdout_groups"`
	HoldoutAllocations            []CorpusFreezeSuiteSummary `json:"holdout_allocations"`
	RequiredSliceCounts           map[string]int             `json:"required_slice_counts,omitempty"`
	UnselectedCases               int                        `json:"unselected_cases"`
	UnselectedGroups              int                        `json:"unselected_groups"`
}

type CorpusFreezeSuiteSummary struct {
	Suite        string `json:"suite"`
	MinimumCases int    `json:"minimum_cases"`
	ActualCases  int    `json:"actual_cases"`
	Groups       int    `json:"groups"`
}

type corpusFreezeGroup struct {
	task    Task
	groupID string
	suite   string
	phase   string
	hash    [sha256.Size]byte
	records []CorpusRecord
}

// FreezeCandidateCorpus validates an already privacy-filtered candidate corpus
// against the exact plan, then creates deterministic group-preserving
// development and holdout corpora. It does not export or query production and
// does not establish that the upstream source exporter was correct.
func FreezeCandidateCorpus(
	plan CorpusPlan,
	planSHA256 string,
	candidates Corpus,
) (CorpusFreeze, error) {
	if err := plan.Validate(); err != nil {
		return CorpusFreeze{}, fmt.Errorf("corpus plan: %w", err)
	}
	if err := validateSHA256("plan_sha256", planSHA256); err != nil {
		return CorpusFreeze{}, err
	}
	canonical, err := NewCorpus(candidates.Records)
	if err != nil {
		return CorpusFreeze{}, fmt.Errorf("candidate corpus: %w", err)
	}
	if candidates.SHA256 == "" || candidates.SHA256 != canonical.SHA256 {
		return CorpusFreeze{}, fmt.Errorf(
			"candidate corpus: supplied SHA-256 does not match canonical records",
		)
	}
	candidates = canonical

	taskPlans := make(map[Task]CorpusPlanTask, len(plan.Tasks))
	for _, taskPlan := range plan.Tasks {
		taskPlans[taskPlan.Task] = taskPlan
	}
	groupsByTask := make(map[Task][]*corpusFreezeGroup, len(plan.Tasks))
	groupIndex := make(map[string]*corpusFreezeGroup)
	sourceSnapshotByTask := make(map[Task]string, len(plan.Tasks))

	for i, record := range candidates.Records {
		taskPlan, exists := taskPlans[record.Task]
		if !exists {
			return CorpusFreeze{}, fmt.Errorf(
				"candidate record %d: task %q is not in plan",
				i+1,
				record.Task,
			)
		}
		if record.Label.Provenance == LabelProvenanceSynthetic {
			return CorpusFreeze{}, fmt.Errorf(
				"candidate record %d (%q): synthetic provenance cannot claim production-plan conformance",
				i+1,
				record.CaseID,
			)
		}
		if record.PrivacyAttestation == nil {
			return CorpusFreeze{}, fmt.Errorf(
				"candidate record %d (%q): privacy attestation is required",
				i+1,
				record.CaseID,
			)
		}
		if record.PrivacyAttestation.PlanID != plan.PlanID {
			return CorpusFreeze{}, fmt.Errorf(
				"candidate record %d (%q): privacy plan_id %q does not match %q",
				i+1,
				record.CaseID,
				record.PrivacyAttestation.PlanID,
				plan.PlanID,
			)
		}
		snapshot := record.PrivacyAttestation.SourceSnapshotSHA256
		if previous := sourceSnapshotByTask[record.Task]; previous != "" &&
			previous != snapshot {
			return CorpusFreeze{}, fmt.Errorf(
				"candidate record %d (%q): task %q mixes source snapshot hashes",
				i+1,
				record.CaseID,
				record.Task,
			)
		}
		sourceSnapshotByTask[record.Task] = snapshot

		suite, err := recordPrimarySuite(
			record,
			plan.Selection.PrimarySuiteSlicePrefix,
			taskPlan.HoldoutAllocation,
		)
		if err != nil {
			return CorpusFreeze{}, fmt.Errorf(
				"candidate record %d (%q): %w",
				i+1,
				record.CaseID,
				err,
			)
		}
		phase, err := recordReviewPhase(record)
		if err != nil {
			return CorpusFreeze{}, fmt.Errorf(
				"candidate record %d (%q): %w",
				i+1,
				record.CaseID,
				err,
			)
		}
		if record.Task == TaskJunkPurge {
			if err := validateMutuallyExclusiveSliceMembership(
				record,
				taskPlan.RequiredUnionCounts,
				"required_union_counts",
			); err != nil {
				return CorpusFreeze{}, fmt.Errorf(
					"candidate record %d (%q): %w",
					i+1,
					record.CaseID,
					err,
				)
			}
		}
		if record.Task == TaskContentFilter {
			if err := validateMutuallyExclusiveSliceMembership(
				record,
				taskPlan.RequiredSafetyCounts,
				"required_safety_counts",
			); err != nil {
				return CorpusFreeze{}, fmt.Errorf(
					"candidate record %d (%q): %w",
					i+1,
					record.CaseID,
					err,
				)
			}
		}

		key := string(record.Task) + "\x00" + record.GroupID
		group := groupIndex[key]
		if group == nil {
			group = &corpusFreezeGroup{
				task:    record.Task,
				groupID: record.GroupID,
				suite:   suite,
				phase:   phase,
				hash: splitGroupHash(
					plan.PlanID,
					record.Task,
					record.GroupID,
				),
			}
			groupIndex[key] = group
			groupsByTask[record.Task] = append(
				groupsByTask[record.Task],
				group,
			)
		} else if group.suite != suite || group.phase != phase {
			return CorpusFreeze{}, fmt.Errorf(
				"candidate task %q group %q spans primary suites or review phases",
				record.Task,
				record.GroupID,
			)
		}
		group.records = append(group.records, record)
	}

	var developmentRecords, holdoutRecords []CorpusRecord
	summaries := make([]CorpusFreezeTaskSummary, 0, len(plan.Tasks))
	for _, task := range orderedTasks {
		taskPlan := taskPlans[task]
		groups := groupsByTask[task]
		if len(groups) == 0 {
			return CorpusFreeze{}, fmt.Errorf(
				"candidate corpus contains no %q groups",
				task,
			)
		}
		sort.Slice(groups, func(i, j int) bool {
			comparison := bytes.Compare(groups[i].hash[:], groups[j].hash[:])
			if comparison != 0 {
				return comparison < 0
			}
			return groups[i].groupID < groups[j].groupID
		})

		var development, holdout, remaining []*corpusFreezeGroup
		var selectedBySuite map[string][]*corpusFreezeGroup
		excludedDevelopmentAliases := 0
		phaseBound := false
		for _, group := range groups {
			phaseBound = phaseBound || group.phase != ""
		}
		if phaseBound {
			selectedBySuite = make(map[string][]*corpusFreezeGroup)
			developmentBySuite := make(map[string]int)
			for _, group := range groups {
				switch group.phase {
				case CorpusReviewPhaseDevelopment:
					if len(group.records) != 1 {
						return CorpusFreeze{}, fmt.Errorf(
							"task %q development review group %q must contain exactly one representative",
							task,
							group.groupID,
						)
					}
					development = append(development, group)
					developmentBySuite[group.suite]++
				case CorpusReviewPhaseHoldout:
					holdout = append(holdout, group)
					selectedBySuite[group.suite] = append(
						selectedBySuite[group.suite],
						group,
					)
				default:
					return CorpusFreeze{}, fmt.Errorf(
						"task %q mixes bound and unbound review phases",
						task,
					)
				}
			}
			for suite, target := range taskPlan.DevelopmentAllocation {
				expected := target +
					taskPlan.DevelopmentReplacementReserve[suite]
				if developmentBySuite[suite] != expected {
					return CorpusFreeze{}, fmt.Errorf(
						"task %q development suite %q has %d representatives, want review reserve %d",
						task,
						suite,
						developmentBySuite[suite],
						expected,
					)
				}
			}
			for suite, maximum := range taskPlan.ReserveMaximum {
				actual := groupCaseCount(selectedBySuite[suite])
				minimum := taskPlan.HoldoutAllocation[suite] +
					taskPlan.ReplacementReserve[suite]
				if actual < minimum || actual > maximum {
					return CorpusFreeze{}, fmt.Errorf(
						"task %q holdout suite %q has %d cases outside bounded range [%d,%d]",
						task,
						suite,
						actual,
						minimum,
						maximum,
					)
				}
			}
			if missing := missingRequiredSlices(
				taskPlan,
				holdout,
			); len(missing) != 0 {
				return CorpusFreeze{}, fmt.Errorf(
					"task %q bounded holdout omits required sampling slices %s",
					task,
					strings.Join(missing, ", "),
				)
			}
		} else {
			var err error
			development, remaining, excludedDevelopmentAliases, err =
				selectDevelopmentRepresentatives(
					groups,
					taskPlan.DevelopmentAllocation,
				)
			if err != nil {
				return CorpusFreeze{}, fmt.Errorf("task %q: %w", task, err)
			}
			holdout, selectedBySuite, err = selectHoldoutGroups(
				taskPlan,
				remaining,
			)
			if err != nil {
				return CorpusFreeze{}, fmt.Errorf("task %q: %w", task, err)
			}
		}

		selected := make(map[*corpusFreezeGroup]struct{}, len(holdout))
		for _, group := range holdout {
			selected[group] = struct{}{}
		}
		unselectedGroups := 0
		unselectedCases := excludedDevelopmentAliases
		for _, group := range remaining {
			if _, exists := selected[group]; exists {
				continue
			}
			unselectedGroups++
			unselectedCases += len(group.records)
		}

		developmentRecords = appendGroupRecords(developmentRecords, development)
		holdoutRecords = appendGroupRecords(holdoutRecords, holdout)
		requiredCounts := requiredSliceCounts(taskPlan, holdout)

		allocationNames := sortedCountMapKeys(taskPlan.HoldoutAllocation)
		allocationSummaries := make(
			[]CorpusFreezeSuiteSummary,
			0,
			len(allocationNames),
		)
		for _, suite := range allocationNames {
			selectedGroups := selectedBySuite[suite]
			allocationSummaries = append(
				allocationSummaries,
				CorpusFreezeSuiteSummary{
					Suite:        suite,
					MinimumCases: taskPlan.HoldoutAllocation[suite],
					ActualCases:  groupCaseCount(selectedGroups),
					Groups:       len(selectedGroups),
				},
			)
		}
		summaries = append(summaries, CorpusFreezeTaskSummary{
			Task:                 task,
			SourceSnapshotSHA256: sourceSnapshotByTask[task],
			CandidateCases:       groupCaseCount(groups),
			CandidateGroups:      len(groups),
			DevelopmentTarget:    taskPlan.DevelopmentTarget,
			DevelopmentAllocation: cloneStringIntMap(
				taskPlan.DevelopmentAllocation,
			),
			DevelopmentReviewReserveCases: groupCaseCount(development),
			DevelopmentCases:              groupCaseCount(development),
			DevelopmentGroups:             len(development),
			HoldoutTarget:                 taskPlan.HoldoutTarget,
			HoldoutCases:                  groupCaseCount(holdout),
			HoldoutGroups:                 len(holdout),
			HoldoutAllocations:            allocationSummaries,
			RequiredSliceCounts:           requiredCounts,
			UnselectedCases:               unselectedCases,
			UnselectedGroups:              unselectedGroups,
		})
	}

	development, err := NewCorpus(developmentRecords)
	if err != nil {
		return CorpusFreeze{}, fmt.Errorf("development corpus: %w", err)
	}
	holdout, err := NewCorpus(holdoutRecords)
	if err != nil {
		return CorpusFreeze{}, fmt.Errorf("holdout corpus: %w", err)
	}
	if err := validateSplitDisjoint(development, holdout); err != nil {
		return CorpusFreeze{}, err
	}

	return CorpusFreeze{
		Development: development,
		Holdout:     holdout,
		Manifest: CorpusFreezeManifest{
			SchemaVersion:                    SchemaVersion,
			Status:                           CorpusFreezeStatus,
			Scope:                            CorpusFreezeScope,
			PlanID:                           plan.PlanID,
			PlanSHA256:                       planSHA256,
			PlanExecutable:                   plan.Executable,
			ProductionSourceExporterVerified: false,
			QuotaEvidenceStatus:              "candidate_slice_metadata_not_independently_reviewed_gold",
			SplitAlgorithmID:                 plan.Selection.AlgorithmID,
			CandidateCorpusSHA256:            candidates.SHA256,
			CandidateCases:                   len(candidates.Records),
			DevelopmentCorpusSHA256:          development.SHA256,
			DevelopmentCases:                 len(development.Records),
			HoldoutCorpusSHA256:              holdout.SHA256,
			HoldoutCases:                     len(holdout.Records),
			Tasks:                            summaries,
		},
	}, nil
}

// FreezeProductionCandidateCorpus is the only path that may set
// production_source_exporter_verified. It binds the exact exporter manifest
// bytes to the plan and candidate corpus before replaying the deterministic
// split.
func FreezeProductionCandidateCorpus(
	plan CorpusPlan,
	planSHA256 string,
	candidates Corpus,
	exportManifest ProductionCandidateExportManifest,
	exportManifestSHA256 string,
	privacySidecar ProductionPrivacySidecar,
	privacySidecarSHA256 string,
) (CorpusFreeze, error) {
	if !plan.Readiness.ProductionSourceExporter {
		return CorpusFreeze{}, fmt.Errorf(
			"plan does not claim an implemented production source exporter",
		)
	}
	if err := validateSHA256(
		"production_export_manifest_sha256",
		exportManifestSHA256,
	); err != nil {
		return CorpusFreeze{}, err
	}
	if err := ValidateProductionCandidateExportManifest(
		plan,
		planSHA256,
		candidates,
		exportManifest,
	); err != nil {
		return CorpusFreeze{}, err
	}
	if exportManifest.ExporterID !=
		CaptureLedgerProductionCandidateExporterID ||
		exportManifest.SamplingOrigin != ProductionCaptureSamplingOrigin {
		return CorpusFreeze{}, fmt.Errorf(
			"only the audited natural-capture ledger exporter may establish production-source verification",
		)
	}
	for i, record := range candidates.Records {
		phase, err := recordReviewPhase(record)
		if err != nil {
			return CorpusFreeze{}, err
		}
		if phase == "" {
			return CorpusFreeze{}, fmt.Errorf(
				"production candidate %d (%q) is missing its development/holdout review phase binding",
				i+1,
				record.CaseID,
			)
		}
	}
	if err := ValidateProductionPrivacySidecar(
		plan,
		planSHA256,
		candidates,
		exportManifest,
		privacySidecar,
		privacySidecarSHA256,
	); err != nil {
		return CorpusFreeze{}, err
	}
	frozen, err := FreezeCandidateCorpus(plan, planSHA256, candidates)
	if err != nil {
		return CorpusFreeze{}, err
	}
	frozen.Manifest.Status = ProductionCorpusFreezeStatus
	frozen.Manifest.Scope = ProductionCorpusFreezeScope
	frozen.Manifest.ProductionSourceExporterVerified = true
	frozen.Manifest.ProductionExportManifestSHA256 = exportManifestSHA256
	frozen.Manifest.PrivacySidecarSHA256 = privacySidecarSHA256
	return frozen, nil
}

// ValidateCorpusFreezeManifest replays every deterministic split field and
// validates the production-proof extension. The generic replay deliberately
// never derives trust from a plan readiness boolean.
func ValidateCorpusFreezeManifest(
	plan CorpusPlan,
	planSHA256 string,
	candidates Corpus,
	manifest CorpusFreezeManifest,
) error {
	expected, err := FreezeCandidateCorpus(plan, planSHA256, candidates)
	if err != nil {
		return err
	}
	normalized := manifest
	normalized.Status = expected.Manifest.Status
	normalized.Scope = expected.Manifest.Scope
	normalized.ProductionSourceExporterVerified = false
	normalized.ProductionExportManifestSHA256 = ""
	normalized.PrivacySidecarSHA256 = ""
	if !reflect.DeepEqual(expected.Manifest, normalized) {
		return fmt.Errorf(
			"candidate freeze manifest does not match the exact plan and candidate corpus",
		)
	}

	if manifest.ProductionSourceExporterVerified {
		if !plan.Readiness.ProductionSourceExporter {
			return fmt.Errorf(
				"candidate freeze claims a production exporter that the plan does not claim",
			)
		}
		if manifest.Status != ProductionCorpusFreezeStatus {
			return fmt.Errorf(
				"verified production freeze status got %q, want %q",
				manifest.Status,
				ProductionCorpusFreezeStatus,
			)
		}
		if manifest.Scope != ProductionCorpusFreezeScope {
			return fmt.Errorf("verified production freeze scope is not recognized")
		}
		if err := validateSHA256(
			"production_export_manifest_sha256",
			manifest.ProductionExportManifestSHA256,
		); err != nil {
			return err
		}
		if err := validateSHA256(
			"privacy_sidecar_sha256",
			manifest.PrivacySidecarSHA256,
		); err != nil {
			return err
		}
		return nil
	}

	if plan.Executable || plan.Readiness.ProductionSourceExporter {
		return fmt.Errorf(
			"candidate freeze must bind a verified production export manifest for this plan",
		)
	}
	if manifest.Status != CorpusFreezeStatus ||
		manifest.Scope != CorpusFreezeScope ||
		manifest.ProductionExportManifestSHA256 != "" ||
		manifest.PrivacySidecarSHA256 != "" {
		return fmt.Errorf(
			"unverified candidate freeze contains production proof fields",
		)
	}
	return nil
}

func recordPrimarySuite(
	record CorpusRecord,
	prefix string,
	allocations map[string]int,
) (string, error) {
	var suite string
	for _, sliceID := range record.SliceIDs {
		if !strings.HasPrefix(sliceID, prefix) {
			continue
		}
		candidate := strings.TrimPrefix(sliceID, prefix)
		if _, exists := allocations[candidate]; !exists {
			return "", fmt.Errorf(
				"primary suite slice %q is not a plan allocation",
				sliceID,
			)
		}
		if suite != "" {
			return "", fmt.Errorf(
				"exactly one primary suite slice is required",
			)
		}
		suite = candidate
	}
	if suite == "" {
		return "", fmt.Errorf(
			"exactly one %q primary suite slice is required",
			prefix+"<allocation>",
		)
	}
	return suite, nil
}

func recordReviewPhase(record CorpusRecord) (string, error) {
	phase := ""
	for _, sliceID := range record.SliceIDs {
		if !strings.HasPrefix(sliceID, CorpusReviewPhaseSlicePrefix) {
			continue
		}
		candidate := strings.TrimPrefix(
			sliceID,
			CorpusReviewPhaseSlicePrefix,
		)
		if candidate != CorpusReviewPhaseDevelopment &&
			candidate != CorpusReviewPhaseHoldout {
			return "", fmt.Errorf(
				"review phase slice %q is unsupported",
				sliceID,
			)
		}
		if phase != "" {
			return "", fmt.Errorf(
				"exactly one review phase slice is permitted",
			)
		}
		phase = candidate
	}
	return phase, nil
}

func validateMutuallyExclusiveSliceMembership(
	record CorpusRecord,
	required map[string]int,
	fieldName string,
) error {
	matches := 0
	for key := range required {
		if containsString(record.SliceIDs, key) {
			matches++
		}
	}
	if matches > 1 {
		return fmt.Errorf(
			"%s slices must be mutually exclusive",
			fieldName,
		)
	}
	return nil
}

func splitGroupHash(planID string, task Task, groupID string) [sha256.Size]byte {
	return sha256.Sum256([]byte(
		planID + "\x00" + string(task) + "\x00" + groupID,
	))
}

func selectDevelopmentRepresentatives(
	groups []*corpusFreezeGroup,
	targets map[string]int,
) (
	selected []*corpusFreezeGroup,
	remaining []*corpusFreezeGroup,
	excludedAliases int,
	err error,
) {
	targetGroups := 0
	for _, target := range targets {
		targetGroups += target
	}
	if targetGroups <= 0 {
		return nil, nil, 0, fmt.Errorf(
			"development independent-group target must be positive",
		)
	}
	if len(groups) < targetGroups {
		return nil, nil, 0, fmt.Errorf(
			"candidate corpus has %d independent groups, below development target %d",
			len(groups),
			targetGroups,
		)
	}
	selected = make([]*corpusFreezeGroup, 0, targetGroups)
	selectedSet := make(map[*corpusFreezeGroup]struct{}, targetGroups)
	selectedBySuite := make(map[string]int, len(targets))
	for _, group := range groups {
		target, exists := targets[group.suite]
		if !exists || selectedBySuite[group.suite] >= target {
			continue
		}
		if len(group.records) == 0 {
			return nil, nil, 0, fmt.Errorf(
				"development group %q has no records",
				group.groupID,
			)
		}
		representative := group.records[0]
		for _, record := range group.records[1:] {
			if record.CaseID < representative.CaseID {
				representative = record
			}
		}
		copyGroup := *group
		copyGroup.records = []CorpusRecord{representative}
		selected = append(selected, &copyGroup)
		selectedSet[group] = struct{}{}
		selectedBySuite[group.suite]++
		excludedAliases += len(group.records) - 1
	}
	for suite, target := range targets {
		if selectedBySuite[suite] != target {
			return nil, nil, 0, fmt.Errorf(
				"candidate corpus has %d development groups in suite %q, below target %d",
				selectedBySuite[suite],
				suite,
				target,
			)
		}
	}
	for _, group := range groups {
		if _, selected := selectedSet[group]; !selected {
			remaining = append(remaining, group)
		}
	}
	return selected, remaining, excludedAliases, nil
}

func selectHoldoutGroups(
	taskPlan CorpusPlanTask,
	remaining []*corpusFreezeGroup,
) (
	[]*corpusFreezeGroup,
	map[string][]*corpusFreezeGroup,
	error,
) {
	available := make(map[string][]*corpusFreezeGroup)
	for _, group := range remaining {
		available[group.suite] = append(available[group.suite], group)
	}
	selectedBySuite := make(map[string][]*corpusFreezeGroup)
	nextBySuite := make(map[string]int)
	for _, suite := range sortedCountMapKeys(taskPlan.HoldoutAllocation) {
		minimum := taskPlan.HoldoutAllocation[suite]
		for groupCaseCount(selectedBySuite[suite]) < minimum {
			index := nextBySuite[suite]
			if index >= len(available[suite]) {
				return nil, nil, fmt.Errorf(
					"holdout suite %q has %d cases, below minimum %d",
					suite,
					groupCaseCount(selectedBySuite[suite]),
					minimum,
				)
			}
			selectedBySuite[suite] = append(
				selectedBySuite[suite],
				available[suite][index],
			)
			nextBySuite[suite] = index + 1
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
		selected := flattenSelectedGroups(selectedBySuite)
		if missing := missingRequiredSlices(taskPlan, selected); len(missing) == 0 {
			return selected, selectedBySuite, nil
		}
		index := nextBySuite[topUpSuite]
		if index >= len(available[topUpSuite]) {
			return nil, nil, fmt.Errorf(
				"holdout cannot satisfy required slices %s after exhausting suite %q",
				strings.Join(missingRequiredSlices(taskPlan, selected), ", "),
				topUpSuite,
			)
		}
		selectedBySuite[topUpSuite] = append(
			selectedBySuite[topUpSuite],
			available[topUpSuite][index],
		)
		nextBySuite[topUpSuite] = index + 1
	}
}

func missingRequiredSlices(
	taskPlan CorpusPlanTask,
	groups []*corpusFreezeGroup,
) []string {
	counts := requiredSliceCounts(taskPlan, groups)
	var missing []string
	switch taskPlan.Task {
	case TaskMatcherExtract, TaskMatcherRerank:
		for _, sliceID := range taskPlan.RequiredSafetySlices {
			if counts[sliceID] == 0 {
				missing = append(missing, sliceID+"<1")
			}
		}
	case TaskContentFilter:
		for _, sliceID := range sortedCountMapKeys(taskPlan.RequiredSafetyCounts) {
			if counts[sliceID] < taskPlan.RequiredSafetyCounts[sliceID] {
				missing = append(
					missing,
					fmt.Sprintf(
						"%s<%d",
						sliceID,
						taskPlan.RequiredSafetyCounts[sliceID],
					),
				)
			}
		}
	case TaskJunkPurge:
		for _, sliceID := range sortedCountMapKeys(taskPlan.RequiredUnionCounts) {
			if counts[sliceID] < taskPlan.RequiredUnionCounts[sliceID] {
				missing = append(
					missing,
					fmt.Sprintf(
						"%s<%d",
						sliceID,
						taskPlan.RequiredUnionCounts[sliceID],
					),
				)
			}
		}
	}
	return missing
}

func requiredSliceCounts(
	taskPlan CorpusPlanTask,
	groups []*corpusFreezeGroup,
) map[string]int {
	required := make(map[string]struct{})
	switch taskPlan.Task {
	case TaskMatcherExtract, TaskMatcherRerank:
		for _, sliceID := range taskPlan.RequiredSafetySlices {
			required[sliceID] = struct{}{}
		}
	case TaskContentFilter:
		for sliceID := range taskPlan.RequiredSafetyCounts {
			required[sliceID] = struct{}{}
		}
	case TaskJunkPurge:
		for sliceID := range taskPlan.RequiredUnionCounts {
			required[sliceID] = struct{}{}
		}
	}
	counts := make(map[string]int, len(required))
	for sliceID := range required {
		counts[sliceID] = 0
	}
	for _, group := range groups {
		for _, record := range group.records {
			if (taskPlan.Task == TaskMatcherExtract ||
				taskPlan.Task == TaskMatcherRerank ||
				taskPlan.Task == TaskContentFilter) &&
				group.suite != "safety" {
				continue
			}
			for sliceID := range required {
				if containsString(record.SliceIDs, sliceID) {
					counts[sliceID]++
				}
			}
		}
	}
	return counts
}

func flattenSelectedGroups(
	selectedBySuite map[string][]*corpusFreezeGroup,
) []*corpusFreezeGroup {
	var groups []*corpusFreezeGroup
	for _, selected := range selectedBySuite {
		groups = append(groups, selected...)
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

func appendGroupRecords(
	target []CorpusRecord,
	groups []*corpusFreezeGroup,
) []CorpusRecord {
	for _, group := range groups {
		target = append(target, group.records...)
	}
	return target
}

func groupCaseCount(groups []*corpusFreezeGroup) int {
	total := 0
	for _, group := range groups {
		total += len(group.records)
	}
	return total
}

func sortedCountMapKeys(values map[string]int) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func validateSplitDisjoint(development, holdout Corpus) error {
	developmentGroups := make(map[string]struct{}, len(development.Records))
	for _, record := range development.Records {
		key := string(record.Task) + "\x00" + record.GroupID
		developmentGroups[key] = struct{}{}
	}
	for _, record := range holdout.Records {
		key := string(record.Task) + "\x00" + record.GroupID
		if _, exists := developmentGroups[key]; exists {
			return fmt.Errorf(
				"split invariant: task %q group %q appears in development and holdout",
				record.Task,
				record.GroupID,
			)
		}
	}
	return nil
}

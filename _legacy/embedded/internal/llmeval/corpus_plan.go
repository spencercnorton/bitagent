package llmeval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const (
	// CorpusSplitAlgorithmGroupSHA256V2 is the only split algorithm supported
	// by corpus-plan schema version 1. The identifier binds executable
	// behavior; the adjacent prose is explanatory and is not parsed.
	CorpusSplitAlgorithmGroupSHA256V2 = "sha256-plan-task-group-representative-v2"

	// CorpusPrimarySuiteSlicePrefix reserves slice IDs used to assign each
	// group to exactly one plan-defined holdout allocation.
	CorpusPrimarySuiteSlicePrefix = "suite:"
	CorpusReviewPhaseSlicePrefix  = "review_phase:"
	CorpusReviewPhaseDevelopment  = "development"
	CorpusReviewPhaseHoldout      = "holdout"

	GoldQuotaNaturalPrimaryHarmGroups = "natural_primary_harm_eligible_groups"
	GoldQuotaNaturalTaskSuccessGroups = "natural_task_success_eligible_groups"
	GoldQuotaSafetyPrimaryHarmGroups  = "safety_primary_harm_eligible_groups"
	GoldQuotaSafetyTaskSuccessGroups  = "safety_task_success_eligible_groups"

	maxCorpusPlanBytes = 1 << 20
)

// CorpusPlan is the complete, strict schema for the checked-in sampling
// pre-registration. It contains no production source text.
type CorpusPlan struct {
	SchemaVersion       int                     `json:"schema_version"`
	PlanID              string                  `json:"plan_id"`
	Status              string                  `json:"status"`
	Executable          bool                    `json:"executable"`
	NonExecutableReason string                  `json:"non_executable_reason,omitempty"`
	AsOfUTC             string                  `json:"as_of_utc"`
	Purpose             string                  `json:"purpose"`
	NonGoals            []string                `json:"non_goals"`
	PrivacyGate         CorpusPlanPrivacyGate   `json:"privacy_gate"`
	Selection           CorpusPlanSelection     `json:"selection"`
	SourceObservations  CorpusSourceObservation `json:"source_observations"`
	Tasks               []CorpusPlanTask        `json:"tasks"`
	Readiness           CorpusPlanReadiness     `json:"readiness"`
	// A plan is never edited in place: the deterministic selection split hashes
	// plan_id, so an edit silently re-shuffles every sample. A revision is a new
	// file with a new plan_id that names its predecessor here.
	SupersedesPlanID string `json:"supersedes_plan_id,omitempty"`
	SupersedesReason string `json:"supersedes_reason,omitempty"`
}

type CorpusPlanPrivacyGate struct {
	Stage        string   `json:"stage"`
	Requirements []string `json:"requirements"`
}

type CorpusPlanSelection struct {
	Unit                       string `json:"unit"`
	AlgorithmID                string `json:"algorithm_id"`
	PrimarySuiteSlicePrefix    string `json:"primary_suite_slice_prefix"`
	Deduplication              string `json:"deduplication"`
	CanonicalOrder             string `json:"canonical_order"`
	SplitAlgorithm             string `json:"split_algorithm"`
	DevelopmentTargetSemantics string `json:"development_target_semantics"`
	HoldoutTargetSemantics     string `json:"holdout_target_semantics"`
	DevelopmentGroups          string `json:"development_groups"`
	HoldoutGroups              string `json:"holdout_groups"`
	CrossSuiteRule             string `json:"cross_suite_rule"`
	ReplacementRule            string `json:"replacement_rule"`
	StageZero                  string `json:"stage_zero"`
}

type CorpusSourceObservation struct {
	Classification   string `json:"classification"`
	ObservedAtUTC    string `json:"observed_at_utc"`
	ProductionTotals struct {
		Torrents                    int64 `json:"torrents"`
		TorrentContents             int64 `json:"torrent_contents"`
		CanonicalLabels             int64 `json:"canonical_labels"`
		TMDBAttachedTorrentContents int64 `json:"tmdb_attached_torrent_contents"`
	} `json:"production_totals"`
	FrozenSegments []struct {
		Segment             string `json:"segment"`
		Records             int64  `json:"records"`
		RecordsWithExpected int64  `json:"records_with_expected"`
	} `json:"frozen_segments"`
	MatcherEvidence struct {
		ResolvedTMDBCasesObserved int64  `json:"resolved_tmdb_cases_observed"`
		Caveat                    string `json:"caveat"`
	} `json:"matcher_evidence"`
	JunkTeacherStrata struct {
		RealMangled int64  `json:"real_mangled"`
		Junk        int64  `json:"junk"`
		Unsure      int64  `json:"unsure"`
		RealAbsent  int64  `json:"real_absent"`
		Caveat      string `json:"caveat"`
	} `json:"junk_teacher_strata"`
}

type CorpusPlanTask struct {
	Task                          Task           `json:"task"`
	SourceCohort                  string         `json:"source_cohort"`
	ModelVisibleFields            []string       `json:"model_visible_fields"`
	DevelopmentTarget             int            `json:"development_target"`
	DevelopmentAllocation         map[string]int `json:"development_allocation"`
	DevelopmentReplacementReserve map[string]int `json:"development_replacement_reserve_cases"`
	HoldoutTarget                 int            `json:"holdout_target"`
	HoldoutAllocation             map[string]int `json:"holdout_allocation"`
	ReplacementReserve            map[string]int `json:"holdout_replacement_reserve_cases"`
	ReserveMaximum                map[string]int `json:"holdout_candidate_reserve_max_cases_by_suite"`
	RequiredGoldEligibleGroups    map[string]int `json:"required_gold_eligible_groups"`
	RequiredSafetySlices          []string       `json:"required_safety_slices,omitempty"`
	RequiredSafetyCounts          map[string]int `json:"required_safety_counts,omitempty"`
	RequiredUnionCounts           map[string]int `json:"required_union_counts,omitempty"`
	TopUpRule                     string         `json:"top_up_rule,omitempty"`
	GoldContract                  string         `json:"gold_contract"`
	ExportRequirement             string         `json:"export_requirement"`
	// GoldTiers declares populations that are scored separately and never
	// blended. Declared in the plan rather than discovered at scoring time,
	// because a tier tag added after adjudication starts is a second migration.
	GoldTiers         map[string]CorpusPlanGoldTier `json:"gold_tiers,omitempty"`
	TierReportingRule string                        `json:"tier_reporting_rule,omitempty"`
	HarmDenominator   string                        `json:"harm_denominator,omitempty"`
}

// CorpusPlanGoldTier is one non-blendable gold population.
type CorpusPlanGoldTier struct {
	N          int    `json:"n"`
	Source     string `json:"source"`
	HumanHours string `json:"human_hours"`
	Bounds     string `json:"bounds"`
	Note       string `json:"note,omitempty"`
}

type CorpusPlanReadiness struct {
	SamplingContract                 bool     `json:"sampling_contract"`
	MachinePlanValidator             bool     `json:"machine_plan_validator"`
	DeterministicSplitFreeze         bool     `json:"deterministic_split_freeze"`
	StrictCorpusSchema               bool     `json:"strict_corpus_schema"`
	BlindedTwoReviewerWorkflow       bool     `json:"blinded_two_reviewer_workflow"`
	IndependentAdjudicationWorkflow  bool     `json:"independent_adjudication_workflow"`
	GoldPromotionWorkflow            bool     `json:"gold_promotion_workflow"`
	PostReviewReplacementWorkflow    bool     `json:"post_review_replacement_workflow"`
	ProductionSourceExporter         bool     `json:"production_source_exporter"`
	ProductionCorpusFrozen           bool     `json:"production_corpus_frozen"`
	IndependentReviewComplete        bool     `json:"independent_review_complete"`
	PaidOpenRouterComparisonComplete bool     `json:"paid_openrouter_comparison_complete"`
	PromotionReady                   bool     `json:"promotion_ready"`
	NextBlockingSteps                []string `json:"next_blocking_steps"`
}

// ReadCorpusPlan reads, identifies, and semantically validates one plan. The
// SHA-256 covers the exact bytes so a formatting or wording change invalidates
// every freeze manifest bound to the previous file.
func ReadCorpusPlan(r io.Reader) (CorpusPlan, string, error) {
	if r == nil {
		return CorpusPlan{}, "", fmt.Errorf("corpus plan reader is nil")
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxCorpusPlanBytes+1))
	if err != nil {
		return CorpusPlan{}, "", fmt.Errorf("read corpus plan: %w", err)
	}
	if len(raw) == 0 {
		return CorpusPlan{}, "", fmt.Errorf("corpus plan is empty")
	}
	if len(raw) > maxCorpusPlanBytes {
		return CorpusPlan{}, "", fmt.Errorf(
			"corpus plan exceeds the %d-byte limit",
			maxCorpusPlanBytes,
		)
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return CorpusPlan{}, "", fmt.Errorf("decode corpus plan: %w", err)
	}

	var plan CorpusPlan
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return CorpusPlan{}, "", fmt.Errorf("decode corpus plan: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return CorpusPlan{}, "", fmt.Errorf(
				"decode corpus plan: multiple JSON values",
			)
		}
		return CorpusPlan{}, "", fmt.Errorf(
			"decode corpus plan: trailing data: %w",
			err,
		)
	}
	if err := plan.Validate(); err != nil {
		return CorpusPlan{}, "", err
	}
	return plan, sha256Hex(raw), nil
}

func (p CorpusPlan) Validate() error {
	if p.SchemaVersion != SchemaVersion {
		return fmt.Errorf(
			"corpus plan schema_version: got %d, want %d",
			p.SchemaVersion,
			SchemaVersion,
		)
	}
	if err := validateIdentifier("plan_id", p.PlanID); err != nil {
		return err
	}
	if err := validateIdentifier("status", p.Status); err != nil {
		return err
	}
	if p.Executable {
		if strings.TrimSpace(p.NonExecutableReason) != "" {
			return fmt.Errorf(
				"non_executable_reason: must be empty when executable is true",
			)
		}
	} else if err := validateText(
		"non_executable_reason",
		p.NonExecutableReason,
		maxOverviewLength,
	); err != nil {
		return err
	}
	if err := validateUTC("as_of_utc", p.AsOfUTC); err != nil {
		return err
	}
	if err := validateText("purpose", p.Purpose, maxOverviewLength); err != nil {
		return err
	}
	if err := validateTextList("non_goals", p.NonGoals, 1); err != nil {
		return err
	}
	if err := p.PrivacyGate.Validate(); err != nil {
		return fmt.Errorf("privacy_gate: %w", err)
	}
	if err := p.Selection.Validate(); err != nil {
		return fmt.Errorf("selection: %w", err)
	}
	if err := p.SourceObservations.Validate(); err != nil {
		return fmt.Errorf("source_observations: %w", err)
	}

	if len(p.Tasks) != len(orderedTasks) {
		return fmt.Errorf(
			"tasks: got %d tasks, want exactly %d",
			len(p.Tasks),
			len(orderedTasks),
		)
	}
	seen := make(map[Task]struct{}, len(p.Tasks))
	for i, task := range p.Tasks {
		if err := task.Validate(p.Selection.PrimarySuiteSlicePrefix); err != nil {
			return fmt.Errorf("tasks[%d]: %w", i, err)
		}
		if _, duplicate := seen[task.Task]; duplicate {
			return fmt.Errorf("tasks[%d]: duplicate task %q", i, task.Task)
		}
		seen[task.Task] = struct{}{}
	}
	for _, task := range orderedTasks {
		if _, exists := seen[task]; !exists {
			return fmt.Errorf("tasks: missing task %q", task)
		}
	}
	if err := p.Readiness.Validate(p.Executable); err != nil {
		return fmt.Errorf("readiness: %w", err)
	}
	return nil
}

func (g CorpusPlanPrivacyGate) Validate() error {
	if err := validateText("stage", g.Stage, maxOverviewLength); err != nil {
		return err
	}
	return validateTextList("requirements", g.Requirements, 1)
}

func (s CorpusPlanSelection) Validate() error {
	if s.Unit != "group_id" {
		return fmt.Errorf("unit: got %q, want %q", s.Unit, "group_id")
	}
	if s.AlgorithmID != CorpusSplitAlgorithmGroupSHA256V2 {
		return fmt.Errorf(
			"algorithm_id: got %q, want %q",
			s.AlgorithmID,
			CorpusSplitAlgorithmGroupSHA256V2,
		)
	}
	if s.PrimarySuiteSlicePrefix != CorpusPrimarySuiteSlicePrefix {
		return fmt.Errorf(
			"primary_suite_slice_prefix: got %q, want %q",
			s.PrimarySuiteSlicePrefix,
			CorpusPrimarySuiteSlicePrefix,
		)
	}
	for name, value := range map[string]string{
		"deduplication":                s.Deduplication,
		"canonical_order":              s.CanonicalOrder,
		"split_algorithm":              s.SplitAlgorithm,
		"development_target_semantics": s.DevelopmentTargetSemantics,
		"holdout_target_semantics":     s.HoldoutTargetSemantics,
		"development_groups":           s.DevelopmentGroups,
		"holdout_groups":               s.HoldoutGroups,
		"cross_suite_rule":             s.CrossSuiteRule,
		"replacement_rule":             s.ReplacementRule,
		"stage_zero":                   s.StageZero,
	} {
		if err := validateText(name, value, maxOverviewLength); err != nil {
			return err
		}
	}
	return nil
}

func (o CorpusSourceObservation) Validate() error {
	if err := validateIdentifier("classification", o.Classification); err != nil {
		return err
	}
	if err := validateUTC("observed_at_utc", o.ObservedAtUTC); err != nil {
		return err
	}
	for name, count := range map[string]int64{
		"production_totals.torrents":                       o.ProductionTotals.Torrents,
		"production_totals.torrent_contents":               o.ProductionTotals.TorrentContents,
		"production_totals.canonical_labels":               o.ProductionTotals.CanonicalLabels,
		"production_totals.tmdb_attached_torrent_contents": o.ProductionTotals.TMDBAttachedTorrentContents,
		"matcher_evidence.resolved_tmdb_cases_observed":    o.MatcherEvidence.ResolvedTMDBCasesObserved,
		"junk_teacher_strata.real_mangled":                 o.JunkTeacherStrata.RealMangled,
		"junk_teacher_strata.junk":                         o.JunkTeacherStrata.Junk,
		"junk_teacher_strata.unsure":                       o.JunkTeacherStrata.Unsure,
		"junk_teacher_strata.real_absent":                  o.JunkTeacherStrata.RealAbsent,
	} {
		if count < 0 {
			return fmt.Errorf("%s: must be non-negative", name)
		}
	}
	if len(o.FrozenSegments) == 0 {
		return fmt.Errorf("frozen_segments: at least one observation is required")
	}
	segments := make(map[string]struct{}, len(o.FrozenSegments))
	for i, segment := range o.FrozenSegments {
		if err := validateIdentifier(
			fmt.Sprintf("frozen_segments[%d].segment", i),
			segment.Segment,
		); err != nil {
			return err
		}
		if _, duplicate := segments[segment.Segment]; duplicate {
			return fmt.Errorf(
				"frozen_segments[%d].segment: duplicate %q",
				i,
				segment.Segment,
			)
		}
		segments[segment.Segment] = struct{}{}
		if segment.Records < 0 || segment.RecordsWithExpected < 0 {
			return fmt.Errorf(
				"frozen_segments[%d]: counts must be non-negative",
				i,
			)
		}
		if segment.RecordsWithExpected > segment.Records {
			return fmt.Errorf(
				"frozen_segments[%d]: records_with_expected exceeds records",
				i,
			)
		}
	}
	if err := validateText(
		"matcher_evidence.caveat",
		o.MatcherEvidence.Caveat,
		maxOverviewLength,
	); err != nil {
		return err
	}
	if err := validateText(
		"junk_teacher_strata.caveat",
		o.JunkTeacherStrata.Caveat,
		maxOverviewLength,
	); err != nil {
		return err
	}
	return nil
}

func (t CorpusPlanTask) Validate(primarySuitePrefix string) error {
	if !t.Task.valid() {
		return fmt.Errorf("task: unsupported value %q", t.Task)
	}
	if err := validateText("source_cohort", t.SourceCohort, maxOverviewLength); err != nil {
		return err
	}
	if err := validateIdentifierList(
		"model_visible_fields",
		t.ModelVisibleFields,
		1,
	); err != nil {
		return err
	}
	if t.DevelopmentTarget <= 0 {
		return fmt.Errorf("development_target: must be positive")
	}
	if t.DevelopmentTarget > maxJSONLRecords {
		return fmt.Errorf(
			"development_target: %d exceeds corpus record limit %d",
			t.DevelopmentTarget,
			maxJSONLRecords,
		)
	}
	if err := validatePositiveCountMap(
		"development_allocation",
		t.DevelopmentAllocation,
		1,
	); err != nil {
		return err
	}
	var developmentTotal int
	for suite, count := range t.DevelopmentAllocation {
		if _, exists := t.HoldoutAllocation[suite]; !exists {
			return fmt.Errorf(
				"development_allocation has suite %q absent from holdout_allocation",
				suite,
			)
		}
		developmentTotal += count
	}
	if developmentTotal != t.DevelopmentTarget {
		return fmt.Errorf(
			"development_allocation: totals %d, want development_target %d",
			developmentTotal,
			t.DevelopmentTarget,
		)
	}
	if len(t.DevelopmentReplacementReserve) !=
		len(t.DevelopmentAllocation) {
		return fmt.Errorf(
			"development_replacement_reserve_cases must contain exactly one entry per development suite",
		)
	}
	for suite := range t.DevelopmentAllocation {
		count, exists := t.DevelopmentReplacementReserve[suite]
		if !exists {
			return fmt.Errorf(
				"development_replacement_reserve_cases is missing suite %q",
				suite,
			)
		}
		if count < 0 {
			return fmt.Errorf(
				"development_replacement_reserve_cases[%q] must be non-negative",
				suite,
			)
		}
	}
	if t.HoldoutTarget <= 0 {
		return fmt.Errorf("holdout_target: must be positive")
	}
	if t.HoldoutTarget > maxJSONLRecords {
		return fmt.Errorf(
			"holdout_target: %d exceeds corpus record limit %d",
			t.HoldoutTarget,
			maxJSONLRecords,
		)
	}
	if len(t.HoldoutAllocation) == 0 {
		return fmt.Errorf("holdout_allocation: at least one suite is required")
	}
	var allocationTotal int64
	for suite, count := range t.HoldoutAllocation {
		if err := validateIdentifier("holdout_allocation suite", suite); err != nil {
			return err
		}
		if err := validateIdentifier(
			"holdout_allocation suite slice",
			primarySuitePrefix+suite,
		); err != nil {
			return err
		}
		if count <= 0 {
			return fmt.Errorf(
				"holdout_allocation[%q]: must be positive",
				suite,
			)
		}
		if count > maxJSONLRecords {
			return fmt.Errorf(
				"holdout_allocation[%q]: %d exceeds corpus record limit %d",
				suite,
				count,
				maxJSONLRecords,
			)
		}
		allocationTotal += int64(count)
	}
	if allocationTotal != int64(t.HoldoutTarget) {
		return fmt.Errorf(
			"holdout_allocation: totals %d, want holdout_target %d",
			allocationTotal,
			t.HoldoutTarget,
		)
	}
	if err := validatePositiveCountMap(
		"replacement_reserve_cases",
		t.ReplacementReserve,
		len(t.HoldoutAllocation),
	); err != nil {
		return err
	}
	if len(t.ReplacementReserve) != len(t.HoldoutAllocation) {
		return fmt.Errorf(
			"replacement_reserve_cases must contain exactly one entry per holdout suite",
		)
	}
	for suite := range t.HoldoutAllocation {
		if _, exists := t.ReplacementReserve[suite]; !exists {
			return fmt.Errorf(
				"replacement_reserve_cases is missing suite %q",
				suite,
			)
		}
	}
	if err := validatePositiveCountMap(
		"candidate_reserve_max_cases_by_suite",
		t.ReserveMaximum,
		len(t.HoldoutAllocation),
	); err != nil {
		return err
	}
	if len(t.ReserveMaximum) != len(t.HoldoutAllocation) {
		return fmt.Errorf(
			"candidate_reserve_max_cases_by_suite must contain exactly one entry per holdout suite",
		)
	}
	for suite, holdoutMinimum := range t.HoldoutAllocation {
		maximum, exists := t.ReserveMaximum[suite]
		if !exists {
			return fmt.Errorf(
				"candidate_reserve_max_cases_by_suite is missing suite %q",
				suite,
			)
		}
		minimum := holdoutMinimum + t.ReplacementReserve[suite]
		if maximum < minimum {
			return fmt.Errorf(
				"candidate_reserve_max_cases_by_suite[%q] is %d, below holdout plus replacement minimum %d",
				suite,
				maximum,
				minimum,
			)
		}
	}
	if err := validateRequiredGoldEligibleGroups(t); err != nil {
		return err
	}

	switch t.Task {
	case TaskMatcherExtract, TaskMatcherRerank:
		if err := validateIdentifierList(
			"required_safety_slices",
			t.RequiredSafetySlices,
			1,
		); err != nil {
			return err
		}
		if _, exists := t.HoldoutAllocation["safety"]; !exists {
			return fmt.Errorf("holdout_allocation: matcher task requires safety")
		}
		if len(t.RequiredSafetyCounts) != 0 ||
			len(t.RequiredUnionCounts) != 0 ||
			strings.TrimSpace(t.TopUpRule) != "" {
			return fmt.Errorf("matcher task has fields reserved for another task")
		}
	case TaskContentFilter:
		if len(t.RequiredSafetySlices) != 0 ||
			len(t.RequiredUnionCounts) != 0 ||
			strings.TrimSpace(t.TopUpRule) != "" {
			return fmt.Errorf("contentfilter has fields reserved for another task")
		}
		if _, exists := t.HoldoutAllocation["safety"]; !exists {
			return fmt.Errorf("holdout_allocation: contentfilter requires safety")
		}
		if err := validatePositiveCountMap(
			"required_safety_counts",
			t.RequiredSafetyCounts,
			1,
		); err != nil {
			return err
		}
	case TaskJunkPurge:
		if len(t.RequiredSafetySlices) != 0 ||
			len(t.RequiredSafetyCounts) != 0 {
			return fmt.Errorf("junkpurge has fields reserved for another task")
		}
		if _, exists := t.HoldoutAllocation["safety_top_up"]; !exists {
			return fmt.Errorf(
				"holdout_allocation: junkpurge requires safety_top_up",
			)
		}
		if err := validatePositiveCountMap(
			"required_union_counts",
			t.RequiredUnionCounts,
			1,
		); err != nil {
			return err
		}
		if err := validateText("top_up_rule", t.TopUpRule, maxOverviewLength); err != nil {
			return err
		}
	}

	if err := validateText("gold_contract", t.GoldContract, maxOverviewLength); err != nil {
		return err
	}
	return validateText(
		"export_requirement",
		t.ExportRequirement,
		maxOverviewLength,
	)
}

func validateRequiredGoldEligibleGroups(t CorpusPlanTask) error {
	required := []string{
		GoldQuotaNaturalPrimaryHarmGroups,
		GoldQuotaNaturalTaskSuccessGroups,
		GoldQuotaSafetyPrimaryHarmGroups,
		GoldQuotaSafetyTaskSuccessGroups,
	}
	if err := validatePositiveCountMap(
		"required_gold_eligible_groups",
		t.RequiredGoldEligibleGroups,
		len(required),
	); err != nil {
		return err
	}
	if len(t.RequiredGoldEligibleGroups) != len(required) {
		return fmt.Errorf(
			"required_gold_eligible_groups must contain exactly the four promotion gate denominators",
		)
	}
	safetySuite := "safety"
	if t.Task == TaskJunkPurge {
		safetySuite = "safety_top_up"
	}
	for _, quota := range required {
		count, exists := t.RequiredGoldEligibleGroups[quota]
		if !exists {
			return fmt.Errorf(
				"required_gold_eligible_groups is missing %q",
				quota,
			)
		}
		maximum := t.ReserveMaximum["natural"]
		if quota == GoldQuotaSafetyPrimaryHarmGroups ||
			quota == GoldQuotaSafetyTaskSuccessGroups {
			maximum = t.ReserveMaximum[safetySuite]
		}
		if count > maximum {
			return fmt.Errorf(
				"required_gold_eligible_groups[%q]=%d exceeds its suite candidate maximum %d",
				quota,
				count,
				maximum,
			)
		}
	}
	return nil
}

func (r CorpusPlanReadiness) Validate(executable bool) error {
	requiredImplemented := map[string]bool{
		"sampling_contract":                 r.SamplingContract,
		"machine_plan_validator":            r.MachinePlanValidator,
		"deterministic_split_freeze":        r.DeterministicSplitFreeze,
		"strict_corpus_schema":              r.StrictCorpusSchema,
		"blinded_two_reviewer_workflow":     r.BlindedTwoReviewerWorkflow,
		"independent_adjudication_workflow": r.IndependentAdjudicationWorkflow,
		"gold_promotion_workflow":           r.GoldPromotionWorkflow,
	}
	for name, implemented := range requiredImplemented {
		if !implemented {
			return fmt.Errorf("%s: must be true for this schema", name)
		}
	}
	if executable && !r.ProductionSourceExporter {
		return fmt.Errorf(
			"production_source_exporter: must be true when executable is true",
		)
	}
	if executable && !r.PostReviewReplacementWorkflow {
		return fmt.Errorf(
			"post_review_replacement_workflow: must be true when executable is true",
		)
	}
	if r.ProductionCorpusFrozen && !r.ProductionSourceExporter {
		return fmt.Errorf(
			"production_corpus_frozen: requires production_source_exporter",
		)
	}
	if r.IndependentReviewComplete && !r.ProductionCorpusFrozen {
		return fmt.Errorf(
			"independent_review_complete: requires production_corpus_frozen",
		)
	}
	if r.PromotionReady &&
		(!r.ProductionSourceExporter ||
			!r.ProductionCorpusFrozen ||
			!r.IndependentReviewComplete ||
			!r.PostReviewReplacementWorkflow ||
			!r.PaidOpenRouterComparisonComplete) {
		return fmt.Errorf(
			"promotion_ready: all production and comparison prerequisites must be true",
		)
	}
	if !r.PromotionReady {
		if err := validateTextList(
			"next_blocking_steps",
			r.NextBlockingSteps,
			1,
		); err != nil {
			return err
		}
	}
	return nil
}

func validateUTC(name, value string) error {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return fmt.Errorf("%s: must be RFC3339: %w", name, err)
	}
	if parsed.UTC().Format(time.RFC3339) != value {
		return fmt.Errorf("%s: must use canonical UTC Z form", name)
	}
	return nil
}

func validateTextList(name string, values []string, minimum int) error {
	if len(values) < minimum {
		return fmt.Errorf("%s: got %d values, want at least %d", name, len(values), minimum)
	}
	seen := make(map[string]struct{}, len(values))
	for i, value := range values {
		if err := validateText(
			fmt.Sprintf("%s[%d]", name, i),
			value,
			maxOverviewLength,
		); err != nil {
			return err
		}
		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("%s[%d]: duplicate value", name, i)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateIdentifierList(name string, values []string, minimum int) error {
	if len(values) < minimum {
		return fmt.Errorf("%s: got %d values, want at least %d", name, len(values), minimum)
	}
	seen := make(map[string]struct{}, len(values))
	for i, value := range values {
		if err := validateIdentifier(
			fmt.Sprintf("%s[%d]", name, i),
			value,
		); err != nil {
			return err
		}
		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("%s[%d]: duplicate %q", name, i, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validatePositiveCountMap(
	name string,
	values map[string]int,
	minimum int,
) error {
	if len(values) < minimum {
		return fmt.Errorf("%s: got %d values, want at least %d", name, len(values), minimum)
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := validateIdentifier(name+" key", key); err != nil {
			return err
		}
		if values[key] <= 0 {
			return fmt.Errorf("%s[%q]: must be positive", name, key)
		}
		if values[key] > maxJSONLRecords {
			return fmt.Errorf(
				"%s[%q]: %d exceeds corpus record limit %d",
				name,
				key,
				values[key],
				maxJSONLRecords,
			)
		}
	}
	return nil
}

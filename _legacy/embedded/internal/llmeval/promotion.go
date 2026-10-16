package llmeval

// PromotionEligibility separates a useful diagnostic comparison from evidence
// that is strong enough to authorize a production model change.
// PromotionEligibility.Decision can be pass only when promotion mode was
// explicitly requested and every requirement below cleared.
type PromotionEligibility struct {
	Requested                      bool                       `json:"requested"`
	CorpusEligible                 bool                       `json:"corpus_eligible"`
	ProtocolComplete               bool                       `json:"protocol_complete"`
	Eligible                       bool                       `json:"eligible"`
	Decision                       ComparisonDecision         `json:"decision"`
	ReasonCodes                    []string                   `json:"reason_codes"`
	Cases                          int                        `json:"cases"`
	GoldCases                      int                        `json:"gold_cases"`
	HumanReviewedGoldCases         int                        `json:"human_reviewed_gold_cases"`
	IndependentlyReviewedGoldCases int                        `json:"independently_reviewed_gold_cases"`
	HoldoutCases                   int                        `json:"holdout_cases"`
	HoldoutSliceID                 string                     `json:"holdout_slice_id"`
	MinimumIndependentGroups       int                        `json:"minimum_independent_groups"`
	Tasks                          []PromotionTaskEligibility `json:"tasks"`
}

// PromotionTaskEligibility exposes the exact sample-size evidence behind a
// task's eligibility. Safety groups count only primary-harm-eligible cases in
// the configured safety slice, not every row carrying the slice.
type PromotionTaskEligibility struct {
	Task                Task `json:"task"`
	Cases               int  `json:"cases"`
	UniqueGroups        int  `json:"unique_groups"`
	SafetyEligibleCases int  `json:"safety_eligible_cases"`
	SafetyUniqueGroups  int  `json:"safety_unique_groups"`
	MeetsMinimum        bool `json:"meets_minimum"`
}

func assessPromotionEligibility(
	corpus Corpus,
	tasks []TaskComparison,
	options ComparisonOptions,
) PromotionEligibility {
	minimumGroups := minimumZeroDiscordanceGroups(ComparisonHarmMargin)
	assessment := PromotionEligibility{
		Requested:                options.PromotionMode,
		CorpusEligible:           true,
		Cases:                    len(corpus.Records),
		HoldoutSliceID:           options.HoldoutSliceID,
		MinimumIndependentGroups: minimumGroups,
	}
	if options.ClosureBoundHoldout {
		assessment.HoldoutSliceID = "gold_closure_manifest:holdout"
	}

	for _, record := range corpus.Records {
		if record.Label.Strength == LabelStrengthGold {
			assessment.GoldCases++
		}
		if record.Label.Strength == LabelStrengthGold &&
			record.Label.Provenance == LabelProvenanceHumanReview {
			assessment.HumanReviewedGoldCases++
		}
		if record.Label.Strength == LabelStrengthGold &&
			record.Label.Provenance == LabelProvenanceHumanReview &&
			record.Label.ReviewerCount >= 2 {
			assessment.IndependentlyReviewedGoldCases++
		}
		if options.ClosureBoundHoldout ||
			hasSlice(record.SliceIDs, options.HoldoutSliceID) {
			assessment.HoldoutCases++
		}
	}

	if assessment.GoldCases != assessment.Cases {
		assessment.CorpusEligible = false
		assessment.ReasonCodes = append(assessment.ReasonCodes, "non_gold_labels")
	}
	if assessment.HumanReviewedGoldCases != assessment.Cases {
		assessment.CorpusEligible = false
		assessment.ReasonCodes = append(
			assessment.ReasonCodes,
			"non_human_review_gold_labels",
		)
	}
	if assessment.IndependentlyReviewedGoldCases != assessment.Cases {
		assessment.CorpusEligible = false
		assessment.ReasonCodes = append(
			assessment.ReasonCodes,
			"insufficient_independent_reviewers",
		)
	}
	if assessment.HoldoutCases != assessment.Cases {
		assessment.CorpusEligible = false
		assessment.ReasonCodes = append(
			assessment.ReasonCodes,
			"missing_holdout_membership",
		)
	}
	if options.PromotionMode && !options.ClosureBoundHoldout {
		assessment.CorpusEligible = false
		assessment.ReasonCodes = append(
			assessment.ReasonCodes,
			"holdout_not_bound_by_gold_closure_manifest",
		)
	}

	insufficientTaskGroups := false
	insufficientSafetyGroups := false
	for _, task := range tasks {
		primaryGroups := 0
		for _, suite := range task.Suites {
			if suite.Suite == task.PrimaryQualitySuite {
				primaryGroups = suite.UniqueGroups
				break
			}
		}
		taskEligibility := PromotionTaskEligibility{
			Task:                task.Task,
			Cases:               task.Cases,
			UniqueGroups:        primaryGroups,
			SafetyEligibleCases: task.SafetyHarm.Candidate.Denominator,
			SafetyUniqueGroups:  task.SafetyHarm.UniqueGroups,
		}
		taskEligibility.MeetsMinimum =
			taskEligibility.UniqueGroups >= minimumGroups &&
				taskEligibility.SafetyUniqueGroups >= minimumGroups
		if taskEligibility.UniqueGroups < minimumGroups {
			insufficientTaskGroups = true
		}
		if taskEligibility.SafetyUniqueGroups < minimumGroups {
			insufficientSafetyGroups = true
		}
		assessment.Tasks = append(assessment.Tasks, taskEligibility)
	}
	if insufficientTaskGroups {
		assessment.CorpusEligible = false
		assessment.ReasonCodes = append(
			assessment.ReasonCodes,
			"insufficient_task_groups",
		)
	}
	if insufficientSafetyGroups {
		assessment.CorpusEligible = false
		assessment.ReasonCodes = append(
			assessment.ReasonCodes,
			"insufficient_safety_groups",
		)
	}
	if !assessment.Requested {
		assessment.ReasonCodes = append(
			assessment.ReasonCodes,
			"promotion_mode_not_requested",
		)
	}
	// The current runner produces one paired result set. It does not yet bind
	// the required packing/permutation/instability runs, schema-repair
	// accounting, route-return reconciliation, multiplicity correction, and
	// calibration artifacts into one verified promotion bundle. Fail closed
	// until that bundle has a validated machine-readable contract.
	assessment.ProtocolComplete = false
	assessment.ReasonCodes = append(
		assessment.ReasonCodes,
		"required_protocol_artifacts_unavailable",
	)

	assessment.Eligible = assessment.Requested &&
		assessment.CorpusEligible &&
		assessment.ProtocolComplete
	return assessment
}

func promotionSafeDecision(
	diagnostic ComparisonDecision,
	promotionEligible bool,
) ComparisonDecision {
	if promotionEligible || diagnostic == ComparisonFail {
		return diagnostic
	}
	return ComparisonInconclusive
}

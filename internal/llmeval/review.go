package llmeval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
)

// ReviewAssignmentKind distinguishes independent primary review from the
// smaller follow-up assignment containing only cases that need adjudication.
type ReviewAssignmentKind string

const (
	ReviewAssignmentPrimary      ReviewAssignmentKind = "primary"
	ReviewAssignmentAdjudication ReviewAssignmentKind = "adjudication"
)

// ReviewAssignmentSpec identifies one reviewer's blinded work packet. The
// policy version is frozen into the assignment ID and later copied into gold
// label metadata.
type ReviewAssignmentSpec struct {
	ReviewSetID   string
	ReviewerID    string
	PolicyVersion string
	ReviewProof   HumanReviewProof
	Tasks         []Task
}

// ReviewAssignment is a deterministic input-only view of a frozen corpus.
// Cases never contain expected labels, label metadata, group/slice membership,
// model identities, model outputs, or provider responses.
type ReviewAssignment struct {
	SchemaVersion int
	AssignmentID  string
	Kind          ReviewAssignmentKind
	ReviewSetID   string
	ReviewerID    string
	PolicyVersion string
	ReviewProof   HumanReviewProof
	CorpusSHA256  string
	CaseSetSHA256 string
	Tasks         []Task
	Cases         []ReviewCaseRecord
}

// ReviewCaseRecord is the standalone JSONL record shown to a reviewer.
// ReviewCaseID is reviewer-specific and cannot reveal a potentially
// label-bearing source case ID.
type ReviewCaseRecord struct {
	SchemaVersion int                  `json:"schema_version"`
	AssignmentID  string               `json:"assignment_id"`
	Kind          ReviewAssignmentKind `json:"kind"`
	ReviewSetID   string               `json:"review_set_id"`
	ReviewerID    string               `json:"reviewer_id"`
	PolicyVersion string               `json:"policy_version"`
	ReviewProof   HumanReviewProof     `json:"review_proof"`
	CorpusSHA256  string               `json:"corpus_sha256"`
	CaseSetSHA256 string               `json:"case_set_sha256"`
	Tasks         []Task               `json:"tasks"`
	Ordinal       int                  `json:"ordinal"`
	ReviewCaseID  string               `json:"review_case_id"`
	Task          Task                 `json:"task"`
	InputSHA256   string               `json:"input_sha256"`
	ContentFilter *ContentFilterInput  `json:"contentfilter,omitempty"`
	JunkPurge     *JunkPurgeInput      `json:"junkpurge,omitempty"`
}

// ReviewLabel is a human review decision. Ambiguous is intentionally valid at
// review time but cannot become a gold expected label.
type ReviewLabel string

const (
	ReviewLabelEnglish    ReviewLabel = "english"
	ReviewLabelNonEnglish ReviewLabel = "non_english"
	ReviewLabelUncertain  ReviewLabel = "uncertain"
	ReviewLabelAmbiguous  ReviewLabel = "ambiguous"
)

// Junk-purge review labels are the JunkContentClass values verbatim.
//
// This is where the cataloguedness defect actually lived. The old vocabulary
// was junk | real_mangled | real_absent | unsure, so the question put to a
// human reviewer was literally "is this a real movie or TV show?" — provenance.
// Every hour of adjudication under it would have produced labels answering the
// wrong question, and the review budget is spent once.
//
// A reviewer now picks a CONTENT CLASS. The disposition is derived by
// DeriveJunkDisposition, so a later policy change is a re-derivation over
// existing labels rather than another adjudication pass.
func junkReviewLabel(class JunkContentClass) ReviewLabel {
	return ReviewLabel(class)
}

// junkContentClassForReviewLabel is the inverse, and the only place a review
// label becomes a gold class.
func junkContentClassForReviewLabel(
	label ReviewLabel,
) (JunkContentClass, bool) {
	for _, class := range JunkContentClasses() {
		if ReviewLabel(class) == label {
			return class, true
		}
	}
	return "", false
}

// JunkReviewLabels is the reviewer's permitted decision set for the junk-purge
// task, in derivation order. It must equal the `decisions` list of the bound
// review policy artifact; TestJunkReviewLabelsMatchPolicyArtifact pins that.
func JunkReviewLabels() []ReviewLabel {
	classes := JunkContentClasses()
	labels := make([]ReviewLabel, 0, len(classes))
	for _, class := range classes {
		labels = append(labels, junkReviewLabel(class))
	}
	return labels
}

// ReviewResponseRecord is one strict response row. It carries no notes or
// arbitrary metadata, which prevents credentials, model output, or hidden
// reviewer hints from entering the label artifact.
type ReviewResponseRecord struct {
	SchemaVersion int              `json:"schema_version"`
	AssignmentID  string           `json:"assignment_id"`
	ReviewerID    string           `json:"reviewer_id"`
	ReviewProof   HumanReviewProof `json:"review_proof"`
	ReviewCaseID  string           `json:"review_case_id"`
	Task          Task             `json:"task"`
	Label         ReviewLabel      `json:"label"`
}

// ReviewSubmission is a validated, complete response to one assignment.
type ReviewSubmission struct {
	Assignment ReviewAssignment
	Responses  []ReviewResponseRecord
}

// AgreementStats reports observed agreement and Cohen's kappa. CohensKappa is
// nil when the expected agreement is one and kappa is mathematically
// undefined.
type AgreementStats struct {
	Cases             int      `json:"cases"`
	Agreements        int      `json:"agreements"`
	Disagreements     int      `json:"disagreements"`
	NeedsAdjudication int      `json:"needs_adjudication"`
	ObservedAgreement float64  `json:"observed_agreement"`
	ExpectedAgreement float64  `json:"expected_agreement"`
	CohensKappa       *float64 `json:"cohens_kappa,omitempty"`
}

// TaskAgreementStats carries agreement statistics for one review task.
type TaskAgreementStats struct {
	Task Task `json:"task"`
	AgreementStats
}

// ReviewConfusionCell is one deterministic cell of the primary-review
// confusion matrix.
type ReviewConfusionCell struct {
	Task           Task        `json:"task"`
	ReviewerALabel ReviewLabel `json:"reviewer_a_label"`
	ReviewerBLabel ReviewLabel `json:"reviewer_b_label"`
	Count          int         `json:"count"`
}

// ReviewAgreementReport contains no source text or labels from a model. Source
// case IDs appear only in NeedsAdjudicationCaseIDs for operator-side routing;
// adjudicators receive fresh opaque IDs through NewAdjudicationAssignment.
type ReviewAgreementReport struct {
	CorpusSHA256             string                `json:"corpus_sha256"`
	ReviewSetID              string                `json:"review_set_id"`
	PolicyVersion            string                `json:"policy_version"`
	ReviewProof              HumanReviewProof      `json:"review_proof"`
	ReviewerA                string                `json:"reviewer_a"`
	ReviewerB                string                `json:"reviewer_b"`
	Overall                  AgreementStats        `json:"overall"`
	ByTask                   []TaskAgreementStats  `json:"by_task"`
	Confusion                []ReviewConfusionCell `json:"confusion"`
	NeedsAdjudicationCaseIDs []string              `json:"needs_adjudication_case_ids"`
}

// GoldPromotion is the independently reviewed subset that can safely enter a
// scored corpus. GoldCorpus is nil when every reviewed case remains ambiguous.
type GoldPromotion struct {
	GoldCorpus        *Corpus
	Agreement         ReviewAgreementReport
	AdjudicatedCases  int
	UnresolvedCaseIDs []string
}

// NewReviewAssignment builds a primary, reviewer-specific blinded assignment.
// Only contentfilter and junkpurge are currently human-reviewable because
// their label policies are represented by ReviewLabel.
func NewReviewAssignment(source Corpus, spec ReviewAssignmentSpec) (ReviewAssignment, error) {
	canonical, err := validateFrozenReviewSource(source)
	if err != nil {
		return ReviewAssignment{}, err
	}
	spec, err = canonicalReviewSpec(spec)
	if err != nil {
		return ReviewAssignment{}, err
	}

	caseIDs := make([]string, 0, len(canonical.Records))
	for _, record := range canonical.Records {
		if containsTask(spec.Tasks, record.Task) {
			caseIDs = append(caseIDs, record.CaseID)
		}
	}
	if len(caseIDs) == 0 {
		return ReviewAssignment{}, fmt.Errorf("review assignment: no cases match requested tasks")
	}

	return buildReviewAssignment(
		canonical,
		spec,
		ReviewAssignmentPrimary,
		caseIDs,
	)
}

// NewReviewSubmission validates that responses cover an assignment exactly
// once. Input order is ignored on import and restored to assignment order.
func NewReviewSubmission(
	source Corpus,
	assignment ReviewAssignment,
	responses []ReviewResponseRecord,
) (ReviewSubmission, error) {
	if err := ValidateReviewAssignment(source, assignment); err != nil {
		return ReviewSubmission{}, err
	}
	if len(responses) != len(assignment.Cases) {
		return ReviewSubmission{}, fmt.Errorf(
			"review responses: got %d rows, want %d",
			len(responses),
			len(assignment.Cases),
		)
	}

	caseByID := make(map[string]ReviewCaseRecord, len(assignment.Cases))
	for _, reviewCase := range assignment.Cases {
		caseByID[reviewCase.ReviewCaseID] = reviewCase
	}
	responseByID := make(map[string]ReviewResponseRecord, len(responses))
	for i, response := range responses {
		if err := response.validateForAssignment(assignment); err != nil {
			return ReviewSubmission{}, fmt.Errorf("review response %d: %w", i+1, err)
		}
		reviewCase, exists := caseByID[response.ReviewCaseID]
		if !exists {
			return ReviewSubmission{}, fmt.Errorf(
				"review response %d: review_case_id %q is not in assignment",
				i+1,
				response.ReviewCaseID,
			)
		}
		if response.Task != reviewCase.Task {
			return ReviewSubmission{}, fmt.Errorf(
				"review response %d: task %q does not match assigned task %q",
				i+1,
				response.Task,
				reviewCase.Task,
			)
		}
		if _, exists := responseByID[response.ReviewCaseID]; exists {
			return ReviewSubmission{}, fmt.Errorf(
				"review response %d: duplicate review_case_id %q",
				i+1,
				response.ReviewCaseID,
			)
		}
		responseByID[response.ReviewCaseID] = response
	}

	canonicalResponses := make([]ReviewResponseRecord, 0, len(assignment.Cases))
	for _, reviewCase := range assignment.Cases {
		response, exists := responseByID[reviewCase.ReviewCaseID]
		if !exists {
			return ReviewSubmission{}, fmt.Errorf(
				"review responses: missing review_case_id %q",
				reviewCase.ReviewCaseID,
			)
		}
		canonicalResponses = append(canonicalResponses, response)
	}

	return ReviewSubmission{
		Assignment: cloneReviewAssignment(assignment),
		Responses:  append([]ReviewResponseRecord(nil), canonicalResponses...),
	}, nil
}

// ComparePrimaryReviews validates two independent, complete assignments and
// reports exact-label agreement. A disagreement or an agreed ambiguous answer
// requires third-party adjudication before gold promotion.
func ComparePrimaryReviews(
	source Corpus,
	reviewerA ReviewSubmission,
	reviewerB ReviewSubmission,
) (ReviewAgreementReport, error) {
	canonical, pairs, err := primaryReviewPairs(source, reviewerA, reviewerB)
	if err != nil {
		return ReviewAgreementReport{}, err
	}

	report := ReviewAgreementReport{
		CorpusSHA256:  canonical.SHA256,
		ReviewSetID:   reviewerA.Assignment.ReviewSetID,
		PolicyVersion: reviewerA.Assignment.PolicyVersion,
		ReviewProof:   reviewerA.Assignment.ReviewProof,
		ReviewerA:     reviewerA.Assignment.ReviewerID,
		ReviewerB:     reviewerB.Assignment.ReviewerID,
	}

	report.Overall = calculateAgreementStats(pairs, true)

	byTask := make(map[Task][]reviewPair)
	confusion := make(map[confusionKey]int)
	for _, pair := range pairs {
		byTask[pair.task] = append(byTask[pair.task], pair)
		confusion[confusionKey{
			task: pair.task,
			a:    pair.a,
			b:    pair.b,
		}]++
		if pair.needsAdjudication() {
			report.NeedsAdjudicationCaseIDs = append(
				report.NeedsAdjudicationCaseIDs,
				pair.caseID,
			)
		}
	}
	sort.Strings(report.NeedsAdjudicationCaseIDs)

	for _, task := range orderedTasks {
		taskPairs := byTask[task]
		if len(taskPairs) == 0 {
			continue
		}
		report.ByTask = append(report.ByTask, TaskAgreementStats{
			Task:           task,
			AgreementStats: calculateAgreementStats(taskPairs, false),
		})
	}

	keys := make([]confusionKey, 0, len(confusion))
	for key := range confusion {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].task != keys[j].task {
			return keys[i].task < keys[j].task
		}
		if keys[i].a != keys[j].a {
			return keys[i].a < keys[j].a
		}
		return keys[i].b < keys[j].b
	})
	for _, key := range keys {
		report.Confusion = append(report.Confusion, ReviewConfusionCell{
			Task:           key.task,
			ReviewerALabel: key.a,
			ReviewerBLabel: key.b,
			Count:          confusion[key],
		})
	}

	return report, nil
}

// NewAdjudicationAssignment builds a fresh blinded assignment containing
// exactly the disagreements and ambiguous primary agreements.
func NewAdjudicationAssignment(
	source Corpus,
	adjudicatorID string,
	reviewerA ReviewSubmission,
	reviewerB ReviewSubmission,
) (ReviewAssignment, ReviewAgreementReport, error) {
	report, err := ComparePrimaryReviews(source, reviewerA, reviewerB)
	if err != nil {
		return ReviewAssignment{}, ReviewAgreementReport{}, err
	}
	if len(report.NeedsAdjudicationCaseIDs) == 0 {
		return ReviewAssignment{}, report, fmt.Errorf(
			"adjudication assignment: no cases require adjudication",
		)
	}
	if adjudicatorID == reviewerA.Assignment.ReviewerID ||
		adjudicatorID == reviewerB.Assignment.ReviewerID {
		return ReviewAssignment{}, report, fmt.Errorf(
			"adjudication assignment: adjudicator must be independent of primary reviewers",
		)
	}

	canonical, err := validateFrozenReviewSource(source)
	if err != nil {
		return ReviewAssignment{}, report, err
	}
	spec, err := canonicalReviewSpec(ReviewAssignmentSpec{
		ReviewSetID:   report.ReviewSetID,
		ReviewerID:    adjudicatorID,
		PolicyVersion: report.PolicyVersion,
		ReviewProof:   report.ReviewProof,
		Tasks:         reviewerA.Assignment.Tasks,
	})
	if err != nil {
		return ReviewAssignment{}, report, err
	}
	assignment, err := buildReviewAssignment(
		canonical,
		spec,
		ReviewAssignmentAdjudication,
		report.NeedsAdjudicationCaseIDs,
	)
	if err != nil {
		return ReviewAssignment{}, report, err
	}

	return assignment, report, nil
}

// PromoteGoldLabels resolves two primary reviews and the required third-party
// adjudication. Concrete decisions become human_review/gold labels; cases that
// remain ambiguous are returned as unresolved and excluded from GoldCorpus.
func PromoteGoldLabels(
	source Corpus,
	reviewerA ReviewSubmission,
	reviewerB ReviewSubmission,
	adjudication *ReviewSubmission,
) (GoldPromotion, error) {
	canonical, pairs, err := primaryReviewPairs(source, reviewerA, reviewerB)
	if err != nil {
		return GoldPromotion{}, err
	}
	report, err := ComparePrimaryReviews(canonical, reviewerA, reviewerB)
	if err != nil {
		return GoldPromotion{}, err
	}

	adjudicatedLabels := make(map[string]ReviewLabel)
	if len(report.NeedsAdjudicationCaseIDs) > 0 {
		if adjudication == nil {
			return GoldPromotion{}, fmt.Errorf(
				"gold promotion: %d cases require independent adjudication",
				len(report.NeedsAdjudicationCaseIDs),
			)
		}
		if err := validateAdjudicationSubmission(
			canonical,
			reviewerA,
			reviewerB,
			*adjudication,
			report.NeedsAdjudicationCaseIDs,
		); err != nil {
			return GoldPromotion{}, err
		}
		adjudicatedLabels, err = submissionLabelsBySourceCase(canonical, *adjudication)
		if err != nil {
			return GoldPromotion{}, err
		}
	} else if adjudication != nil {
		return GoldPromotion{}, fmt.Errorf(
			"gold promotion: adjudication supplied but no case requires it",
		)
	}

	sourceByID := make(map[string]CorpusRecord, len(canonical.Records))
	for _, record := range canonical.Records {
		sourceByID[record.CaseID] = record
	}

	promoted := make([]CorpusRecord, 0, len(pairs))
	unresolved := make([]string, 0)
	adjudicatedCount := 0
	for _, pair := range pairs {
		label := pair.a
		reviewerCount := 2
		if pair.needsAdjudication() {
			label = adjudicatedLabels[pair.caseID]
			reviewerCount = 3
			adjudicatedCount++
		}
		if label == ReviewLabelAmbiguous {
			unresolved = append(unresolved, pair.caseID)
			continue
		}

		record := sourceByID[pair.caseID]
		if err := applyGoldReviewLabel(&record, label); err != nil {
			return GoldPromotion{}, fmt.Errorf(
				"gold promotion case %q: %w",
				pair.caseID,
				err,
			)
		}
		record.Label = LabelMetadata{
			Provenance:       LabelProvenanceHumanReview,
			Strength:         LabelStrengthGold,
			SourceRef:        report.ReviewSetID,
			PolicyVersion:    report.PolicyVersion,
			ReviewerCount:    reviewerCount,
			HumanReviewProof: &report.ReviewProof,
		}
		promoted = append(promoted, record)
	}
	sort.Strings(unresolved)

	result := GoldPromotion{
		Agreement:         report,
		AdjudicatedCases:  adjudicatedCount,
		UnresolvedCaseIDs: unresolved,
	}
	if len(promoted) == 0 {
		return result, nil
	}
	gold, err := NewCorpus(promoted)
	if err != nil {
		return GoldPromotion{}, fmt.Errorf("gold promotion: %w", err)
	}
	result.GoldCorpus = &gold

	return result, nil
}

// ValidateReviewAssignment proves that every exported input and opaque alias
// still corresponds to the exact frozen source corpus.
func ValidateReviewAssignment(source Corpus, assignment ReviewAssignment) error {
	canonical, err := validateFrozenReviewSource(source)
	if err != nil {
		return err
	}
	if err := assignment.validateStructure(); err != nil {
		return err
	}
	if assignment.CorpusSHA256 != canonical.SHA256 {
		return fmt.Errorf(
			"review assignment: corpus_sha256 %q does not match source %q",
			assignment.CorpusSHA256,
			canonical.SHA256,
		)
	}

	reviewCaseIDs := make(map[string]struct{}, len(assignment.Cases))
	for _, reviewCase := range assignment.Cases {
		reviewCaseIDs[reviewCase.ReviewCaseID] = struct{}{}
	}

	selectedSourceIDs := make([]string, 0, len(assignment.Cases))
	for _, record := range canonical.Records {
		if !containsTask(assignment.Tasks, record.Task) {
			continue
		}
		reviewCaseID := opaqueReviewCaseID(assignment.AssignmentID, record.CaseID)
		if _, exists := reviewCaseIDs[reviewCaseID]; exists {
			selectedSourceIDs = append(selectedSourceIDs, record.CaseID)
			continue
		}
		if assignment.Kind == ReviewAssignmentPrimary {
			return fmt.Errorf(
				"review assignment: primary assignment is missing source case %q",
				record.CaseID,
			)
		}
	}
	if len(selectedSourceIDs) != len(assignment.Cases) {
		return fmt.Errorf("review assignment: contains a case not found in frozen source")
	}

	spec := ReviewAssignmentSpec{
		ReviewSetID:   assignment.ReviewSetID,
		ReviewerID:    assignment.ReviewerID,
		PolicyVersion: assignment.PolicyVersion,
		ReviewProof:   assignment.ReviewProof,
		Tasks:         assignment.Tasks,
	}
	expected, err := buildReviewAssignment(
		canonical,
		spec,
		assignment.Kind,
		selectedSourceIDs,
	)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(expected, assignment) {
		return fmt.Errorf("review assignment: frozen metadata, order, alias, or input mismatch")
	}

	return nil
}

func buildReviewAssignment(
	source Corpus,
	spec ReviewAssignmentSpec,
	kind ReviewAssignmentKind,
	caseIDs []string,
) (ReviewAssignment, error) {
	if kind != ReviewAssignmentPrimary && kind != ReviewAssignmentAdjudication {
		return ReviewAssignment{}, fmt.Errorf("review assignment: unsupported kind %q", kind)
	}
	if len(caseIDs) == 0 {
		return ReviewAssignment{}, fmt.Errorf("review assignment: case set is empty")
	}

	sourceByID := make(map[string]CorpusRecord, len(source.Records))
	for _, record := range source.Records {
		sourceByID[record.CaseID] = record
	}
	uniqueIDs := make(map[string]struct{}, len(caseIDs))
	canonicalIDs := make([]string, 0, len(caseIDs))
	for _, caseID := range caseIDs {
		if _, exists := uniqueIDs[caseID]; exists {
			return ReviewAssignment{}, fmt.Errorf(
				"review assignment: duplicate source case %q",
				caseID,
			)
		}
		record, exists := sourceByID[caseID]
		if !exists {
			return ReviewAssignment{}, fmt.Errorf(
				"review assignment: source case %q does not exist",
				caseID,
			)
		}
		if !containsTask(spec.Tasks, record.Task) {
			return ReviewAssignment{}, fmt.Errorf(
				"review assignment: source case %q task %q is not selected",
				caseID,
				record.Task,
			)
		}
		uniqueIDs[caseID] = struct{}{}
		canonicalIDs = append(canonicalIDs, caseID)
	}
	sort.Strings(canonicalIDs)

	caseSetSHA := hashReviewFields("llmeval-review-case-set-v1", canonicalIDs...)
	assignmentID := hashReviewFields(
		"llmeval-review-assignment-v1",
		fmt.Sprint(SchemaVersion),
		string(kind),
		source.SHA256,
		caseSetSHA,
		spec.ReviewSetID,
		spec.ReviewerID,
		spec.PolicyVersion,
		reviewProofIdentity(spec.ReviewProof),
		tasksIdentity(spec.Tasks),
	)

	sort.Slice(canonicalIDs, func(i, j int) bool {
		left := reviewOrderKey(assignmentID, canonicalIDs[i])
		right := reviewOrderKey(assignmentID, canonicalIDs[j])
		if cmp := bytes.Compare(left[:], right[:]); cmp != 0 {
			return cmp < 0
		}
		return canonicalIDs[i] < canonicalIDs[j]
	})

	assignment := ReviewAssignment{
		SchemaVersion: SchemaVersion,
		AssignmentID:  assignmentID,
		Kind:          kind,
		ReviewSetID:   spec.ReviewSetID,
		ReviewerID:    spec.ReviewerID,
		PolicyVersion: spec.PolicyVersion,
		ReviewProof:   spec.ReviewProof,
		CorpusSHA256:  source.SHA256,
		CaseSetSHA256: caseSetSHA,
		Tasks:         append([]Task(nil), spec.Tasks...),
		Cases:         make([]ReviewCaseRecord, 0, len(canonicalIDs)),
	}
	for i, caseID := range canonicalIDs {
		record := sourceByID[caseID]
		reviewCase, err := makeReviewCase(assignment, i+1, caseID, record)
		if err != nil {
			return ReviewAssignment{}, err
		}
		assignment.Cases = append(assignment.Cases, reviewCase)
	}
	if err := assignment.validateStructure(); err != nil {
		return ReviewAssignment{}, err
	}

	return assignment, nil
}

func makeReviewCase(
	assignment ReviewAssignment,
	ordinal int,
	sourceCaseID string,
	record CorpusRecord,
) (ReviewCaseRecord, error) {
	reviewCase := ReviewCaseRecord{
		SchemaVersion: assignment.SchemaVersion,
		AssignmentID:  assignment.AssignmentID,
		Kind:          assignment.Kind,
		ReviewSetID:   assignment.ReviewSetID,
		ReviewerID:    assignment.ReviewerID,
		PolicyVersion: assignment.PolicyVersion,
		ReviewProof:   assignment.ReviewProof,
		CorpusSHA256:  assignment.CorpusSHA256,
		CaseSetSHA256: assignment.CaseSetSHA256,
		Tasks:         append([]Task(nil), assignment.Tasks...),
		Ordinal:       ordinal,
		ReviewCaseID:  opaqueReviewCaseID(assignment.AssignmentID, sourceCaseID),
		Task:          record.Task,
	}

	var input any
	switch record.Task {
	case TaskContentFilter:
		payload := record.ContentFilter.Input
		reviewCase.ContentFilter = &payload
		input = struct {
			Task  Task               `json:"task"`
			Input ContentFilterInput `json:"input"`
		}{Task: record.Task, Input: payload}
	case TaskJunkPurge:
		payload := record.JunkPurge.Input
		reviewCase.JunkPurge = &payload
		input = struct {
			Task  Task           `json:"task"`
			Input JunkPurgeInput `json:"input"`
		}{Task: record.Task, Input: payload}
	default:
		return ReviewCaseRecord{}, fmt.Errorf(
			"review case %q: task %q is not human-reviewable",
			sourceCaseID,
			record.Task,
		)
	}

	raw, err := json.Marshal(input)
	if err != nil {
		return ReviewCaseRecord{}, fmt.Errorf(
			"review case %q: marshal input: %w",
			sourceCaseID,
			err,
		)
	}
	sum := sha256.Sum256(raw)
	reviewCase.InputSHA256 = hex.EncodeToString(sum[:])

	return reviewCase, nil
}

func (a ReviewAssignment) validateStructure() error {
	if a.SchemaVersion != SchemaVersion {
		return fmt.Errorf(
			"review assignment: schema_version got %d, want %d",
			a.SchemaVersion,
			SchemaVersion,
		)
	}
	if err := validateIdentifier("review assignment.assignment_id", a.AssignmentID); err != nil {
		return err
	}
	if a.Kind != ReviewAssignmentPrimary && a.Kind != ReviewAssignmentAdjudication {
		return fmt.Errorf("review assignment: unsupported kind %q", a.Kind)
	}
	if err := validateIdentifier("review assignment.review_set_id", a.ReviewSetID); err != nil {
		return err
	}
	if err := validateIdentifier("review assignment.reviewer_id", a.ReviewerID); err != nil {
		return err
	}
	if err := validateTag("review assignment.policy_version", a.PolicyVersion); err != nil {
		return err
	}
	if err := a.ReviewProof.Validate(); err != nil {
		return fmt.Errorf("review assignment.review_proof: %w", err)
	}
	if a.ReviewProof.Workflow != ReviewWorkflowContentJunk {
		return fmt.Errorf(
			"review assignment.review_proof: workflow must be %q",
			ReviewWorkflowContentJunk,
		)
	}
	if a.PolicyVersion != a.ReviewProof.PolicyVersion {
		return fmt.Errorf(
			"review assignment.policy_version does not match review_proof",
		)
	}
	if err := validateSHA256("review assignment.corpus_sha256", a.CorpusSHA256); err != nil {
		return err
	}
	if a.ReviewProof.EvidenceCorpusSHA256 != a.CorpusSHA256 {
		return fmt.Errorf(
			"review assignment.review_proof evidence corpus does not match assignment",
		)
	}
	if err := validateSHA256("review assignment.case_set_sha256", a.CaseSetSHA256); err != nil {
		return err
	}
	spec, err := canonicalReviewSpec(ReviewAssignmentSpec{
		ReviewSetID:   a.ReviewSetID,
		ReviewerID:    a.ReviewerID,
		PolicyVersion: a.PolicyVersion,
		ReviewProof:   a.ReviewProof,
		Tasks:         a.Tasks,
	})
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(spec.Tasks, a.Tasks) {
		return fmt.Errorf("review assignment.tasks: must be sorted and unique")
	}
	if len(a.Cases) == 0 {
		return fmt.Errorf("review assignment: cases are empty")
	}

	expectedAssignmentID := hashReviewFields(
		"llmeval-review-assignment-v1",
		fmt.Sprint(SchemaVersion),
		string(a.Kind),
		a.CorpusSHA256,
		a.CaseSetSHA256,
		a.ReviewSetID,
		a.ReviewerID,
		a.PolicyVersion,
		reviewProofIdentity(a.ReviewProof),
		tasksIdentity(a.Tasks),
	)
	if a.AssignmentID != expectedAssignmentID {
		return fmt.Errorf("review assignment: assignment_id does not match frozen metadata")
	}

	seen := make(map[string]struct{}, len(a.Cases))
	for i, reviewCase := range a.Cases {
		if err := reviewCase.validate(); err != nil {
			return fmt.Errorf("review assignment case %d: %w", i+1, err)
		}
		if reviewCase.Ordinal != i+1 {
			return fmt.Errorf(
				"review assignment case %d: ordinal got %d, want %d",
				i+1,
				reviewCase.Ordinal,
				i+1,
			)
		}
		if reviewCase.AssignmentID != a.AssignmentID ||
			reviewCase.Kind != a.Kind ||
			reviewCase.ReviewSetID != a.ReviewSetID ||
			reviewCase.ReviewerID != a.ReviewerID ||
			reviewCase.PolicyVersion != a.PolicyVersion ||
			!reflect.DeepEqual(reviewCase.ReviewProof, a.ReviewProof) ||
			reviewCase.CorpusSHA256 != a.CorpusSHA256 ||
			reviewCase.CaseSetSHA256 != a.CaseSetSHA256 ||
			!reflect.DeepEqual(reviewCase.Tasks, a.Tasks) {
			return fmt.Errorf("review assignment case %d: assignment metadata mismatch", i+1)
		}
		if _, exists := seen[reviewCase.ReviewCaseID]; exists {
			return fmt.Errorf(
				"review assignment case %d: duplicate review_case_id %q",
				i+1,
				reviewCase.ReviewCaseID,
			)
		}
		seen[reviewCase.ReviewCaseID] = struct{}{}
	}

	return nil
}

func (r ReviewCaseRecord) validate() error {
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version got %d, want %d", r.SchemaVersion, SchemaVersion)
	}
	if err := validateIdentifier("assignment_id", r.AssignmentID); err != nil {
		return err
	}
	if r.Kind != ReviewAssignmentPrimary && r.Kind != ReviewAssignmentAdjudication {
		return fmt.Errorf("kind: unsupported value %q", r.Kind)
	}
	if err := validateIdentifier("review_set_id", r.ReviewSetID); err != nil {
		return err
	}
	if err := validateIdentifier("reviewer_id", r.ReviewerID); err != nil {
		return err
	}
	if err := validateTag("policy_version", r.PolicyVersion); err != nil {
		return err
	}
	if err := r.ReviewProof.Validate(); err != nil {
		return fmt.Errorf("review_proof: %w", err)
	}
	if r.PolicyVersion != r.ReviewProof.PolicyVersion {
		return fmt.Errorf("policy_version does not match review_proof")
	}
	if err := validateSHA256("corpus_sha256", r.CorpusSHA256); err != nil {
		return err
	}
	if err := validateSHA256("case_set_sha256", r.CaseSetSHA256); err != nil {
		return err
	}
	if r.Ordinal <= 0 {
		return fmt.Errorf("ordinal: must be positive")
	}
	if err := validateIdentifier("review_case_id", r.ReviewCaseID); err != nil {
		return err
	}
	if !containsTask(r.Tasks, r.Task) {
		return fmt.Errorf("task %q is not present in assignment tasks", r.Task)
	}
	if err := validateSHA256("input_sha256", r.InputSHA256); err != nil {
		return err
	}

	payloads := boolInt(r.ContentFilter != nil) + boolInt(r.JunkPurge != nil)
	if payloads != 1 {
		return fmt.Errorf("input payload: exactly one payload is required, got %d", payloads)
	}
	switch r.Task {
	case TaskContentFilter:
		if r.ContentFilter == nil {
			return fmt.Errorf("contentfilter input is required")
		}
		return validateText("contentfilter.title", r.ContentFilter.Title, maxTextLength)
	case TaskJunkPurge:
		if r.JunkPurge == nil {
			return fmt.Errorf("junkpurge input is required")
		}
		return validateText("junkpurge.torrent_name", r.JunkPurge.TorrentName, maxTextLength)
	default:
		return fmt.Errorf("task %q is not human-reviewable", r.Task)
	}
}

func (r ReviewResponseRecord) validateForAssignment(assignment ReviewAssignment) error {
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version got %d, want %d", r.SchemaVersion, SchemaVersion)
	}
	if r.AssignmentID != assignment.AssignmentID {
		return fmt.Errorf("assignment_id does not match assignment")
	}
	if r.ReviewerID != assignment.ReviewerID {
		return fmt.Errorf("reviewer_id does not match assignment")
	}
	if !reflect.DeepEqual(r.ReviewProof, assignment.ReviewProof) {
		return fmt.Errorf("review_proof does not match assignment")
	}
	if err := validateIdentifier("review_case_id", r.ReviewCaseID); err != nil {
		return err
	}

	switch r.Task {
	case TaskContentFilter:
		switch r.Label {
		case ReviewLabelEnglish,
			ReviewLabelNonEnglish,
			ReviewLabelUncertain,
			ReviewLabelAmbiguous:
			return nil
		default:
			return fmt.Errorf("label %q is invalid for contentfilter", r.Label)
		}
	case TaskJunkPurge:
		// Every content class is a valid REVIEW response, including ambiguous.
		// Only promotion to gold rejects ambiguous — a reviewer must be able to
		// decline, or the decline rate cannot be measured.
		if _, ok := junkContentClassForReviewLabel(r.Label); !ok {
			return fmt.Errorf("label %q is invalid for junkpurge", r.Label)
		}
		return nil
	default:
		return fmt.Errorf("task %q is not human-reviewable", r.Task)
	}
}

func primaryReviewPairs(
	source Corpus,
	reviewerA ReviewSubmission,
	reviewerB ReviewSubmission,
) (Corpus, []reviewPair, error) {
	canonical, err := validateFrozenReviewSource(source)
	if err != nil {
		return Corpus{}, nil, err
	}
	if reviewerA.Assignment.Kind != ReviewAssignmentPrimary ||
		reviewerB.Assignment.Kind != ReviewAssignmentPrimary {
		return Corpus{}, nil, fmt.Errorf("primary comparison requires two primary assignments")
	}
	if reviewerA.Assignment.ReviewerID == reviewerB.Assignment.ReviewerID {
		return Corpus{}, nil, fmt.Errorf("primary reviewers must be distinct")
	}
	if reviewerA.Assignment.ReviewSetID != reviewerB.Assignment.ReviewSetID ||
		reviewerA.Assignment.PolicyVersion != reviewerB.Assignment.PolicyVersion ||
		!reflect.DeepEqual(
			reviewerA.Assignment.ReviewProof,
			reviewerB.Assignment.ReviewProof,
		) ||
		reviewerA.Assignment.CorpusSHA256 != reviewerB.Assignment.CorpusSHA256 ||
		!reflect.DeepEqual(reviewerA.Assignment.Tasks, reviewerB.Assignment.Tasks) {
		return Corpus{}, nil, fmt.Errorf(
			"primary assignments must share review set, policy, corpus, and tasks",
		)
	}
	if err := validateSubmission(canonical, reviewerA); err != nil {
		return Corpus{}, nil, fmt.Errorf("reviewer A: %w", err)
	}
	if err := validateSubmission(canonical, reviewerB); err != nil {
		return Corpus{}, nil, fmt.Errorf("reviewer B: %w", err)
	}

	labelsA, err := submissionLabelsBySourceCase(canonical, reviewerA)
	if err != nil {
		return Corpus{}, nil, err
	}
	labelsB, err := submissionLabelsBySourceCase(canonical, reviewerB)
	if err != nil {
		return Corpus{}, nil, err
	}
	if len(labelsA) != len(labelsB) {
		return Corpus{}, nil, fmt.Errorf("primary assignments cover different case counts")
	}

	taskByID := make(map[string]Task, len(canonical.Records))
	for _, record := range canonical.Records {
		taskByID[record.CaseID] = record.Task
	}
	caseIDs := make([]string, 0, len(labelsA))
	for caseID := range labelsA {
		if _, exists := labelsB[caseID]; !exists {
			return Corpus{}, nil, fmt.Errorf(
				"primary assignments cover different source cases",
			)
		}
		caseIDs = append(caseIDs, caseID)
	}
	sort.Strings(caseIDs)

	pairs := make([]reviewPair, 0, len(caseIDs))
	for _, caseID := range caseIDs {
		pairs = append(pairs, reviewPair{
			caseID: caseID,
			task:   taskByID[caseID],
			a:      labelsA[caseID],
			b:      labelsB[caseID],
		})
	}

	return canonical, pairs, nil
}

func validateAdjudicationSubmission(
	source Corpus,
	reviewerA ReviewSubmission,
	reviewerB ReviewSubmission,
	adjudication ReviewSubmission,
	requiredCaseIDs []string,
) error {
	if adjudication.Assignment.Kind != ReviewAssignmentAdjudication {
		return fmt.Errorf("gold promotion: third submission is not an adjudication assignment")
	}
	if adjudication.Assignment.ReviewerID == reviewerA.Assignment.ReviewerID ||
		adjudication.Assignment.ReviewerID == reviewerB.Assignment.ReviewerID {
		return fmt.Errorf("gold promotion: adjudicator must be independent")
	}
	if adjudication.Assignment.ReviewSetID != reviewerA.Assignment.ReviewSetID ||
		adjudication.Assignment.PolicyVersion != reviewerA.Assignment.PolicyVersion ||
		!reflect.DeepEqual(
			adjudication.Assignment.ReviewProof,
			reviewerA.Assignment.ReviewProof,
		) ||
		adjudication.Assignment.CorpusSHA256 != reviewerA.Assignment.CorpusSHA256 ||
		!reflect.DeepEqual(adjudication.Assignment.Tasks, reviewerA.Assignment.Tasks) {
		return fmt.Errorf(
			"gold promotion: adjudication must share review set, policy, corpus, and tasks",
		)
	}
	if err := validateSubmission(source, adjudication); err != nil {
		return fmt.Errorf("adjudication: %w", err)
	}
	labels, err := submissionLabelsBySourceCase(source, adjudication)
	if err != nil {
		return err
	}
	if len(labels) != len(requiredCaseIDs) {
		return fmt.Errorf(
			"gold promotion: adjudication covers %d cases, want %d",
			len(labels),
			len(requiredCaseIDs),
		)
	}
	for _, caseID := range requiredCaseIDs {
		if _, exists := labels[caseID]; !exists {
			return fmt.Errorf(
				"gold promotion: adjudication is missing source case %q",
				caseID,
			)
		}
	}

	return nil
}

func validateSubmission(source Corpus, submission ReviewSubmission) error {
	expected, err := NewReviewSubmission(
		source,
		submission.Assignment,
		submission.Responses,
	)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(expected, submission) {
		return fmt.Errorf("review submission: non-canonical or mutated submission")
	}

	return nil
}

func submissionLabelsBySourceCase(
	source Corpus,
	submission ReviewSubmission,
) (map[string]ReviewLabel, error) {
	responseByReviewID := make(map[string]ReviewLabel, len(submission.Responses))
	for _, response := range submission.Responses {
		responseByReviewID[response.ReviewCaseID] = response.Label
	}

	labels := make(map[string]ReviewLabel, len(submission.Responses))
	for _, record := range source.Records {
		reviewCaseID := opaqueReviewCaseID(submission.Assignment.AssignmentID, record.CaseID)
		label, exists := responseByReviewID[reviewCaseID]
		if !exists {
			continue
		}
		labels[record.CaseID] = label
	}
	if len(labels) != len(submission.Responses) {
		return nil, fmt.Errorf("review submission: response alias is not in frozen source")
	}

	return labels, nil
}

func calculateAgreementStats(pairs []reviewPair, includeTask bool) AgreementStats {
	stats := AgreementStats{Cases: len(pairs)}
	countA := make(map[string]int)
	countB := make(map[string]int)
	for _, pair := range pairs {
		if pair.a == pair.b {
			stats.Agreements++
		} else {
			stats.Disagreements++
		}
		if pair.needsAdjudication() {
			stats.NeedsAdjudication++
		}
		categoryA := string(pair.a)
		categoryB := string(pair.b)
		if includeTask {
			categoryA = string(pair.task) + "\x00" + categoryA
			categoryB = string(pair.task) + "\x00" + categoryB
		}
		countA[categoryA]++
		countB[categoryB]++
	}
	if len(pairs) == 0 {
		return stats
	}

	stats.ObservedAgreement = float64(stats.Agreements) / float64(stats.Cases)
	categories := make([]string, 0, len(countA))
	for category := range countA {
		categories = append(categories, category)
	}
	sort.Strings(categories)
	for _, category := range categories {
		count := countA[category]
		stats.ExpectedAgreement +=
			(float64(count) / float64(stats.Cases)) *
				(float64(countB[category]) / float64(stats.Cases))
	}
	if math.Abs(1-stats.ExpectedAgreement) > 1e-12 {
		kappa := (stats.ObservedAgreement - stats.ExpectedAgreement) /
			(1 - stats.ExpectedAgreement)
		stats.CohensKappa = &kappa
	}

	return stats
}

func applyGoldReviewLabel(record *CorpusRecord, label ReviewLabel) error {
	switch record.Task {
	case TaskContentFilter:
		switch label {
		case ReviewLabelEnglish:
			record.ContentFilter.Expected = ContentFilterExpected{
				Language: LanguageEnglish,
			}
		case ReviewLabelNonEnglish:
			record.ContentFilter.Expected = ContentFilterExpected{
				Language: LanguageNonEnglish,
			}
		case ReviewLabelUncertain:
			record.ContentFilter.Expected = ContentFilterExpected{
				AllowAbstain: true,
			}
		default:
			return fmt.Errorf("label %q cannot become contentfilter gold", label)
		}
	case TaskJunkPurge:
		class, ok := junkContentClassForReviewLabel(label)
		if !ok {
			return fmt.Errorf("label %q cannot become junkpurge gold", label)
		}
		if class == JunkClassAmbiguous {
			return fmt.Errorf(
				"label %q cannot become junkpurge gold: a declined "+
					"adjudication is counted, not scored",
				label,
			)
		}
		disposition, err := DeriveJunkDisposition(JunkDispositionPolicyV1, class)
		if err != nil {
			return err
		}
		// Disposition is DERIVED here and nowhere else. A reviewer supplies the
		// content class only; that is what makes a policy revision free.
		record.JunkPurge.Expected = JunkPurgeExpected{
			Disposition:       disposition,
			ContentClass:      class,
			DispositionPolicy: JunkDispositionPolicyV1,
		}
	default:
		return fmt.Errorf("task %q is not human-reviewable", record.Task)
	}

	return nil
}

func validateFrozenReviewSource(source Corpus) (Corpus, error) {
	canonical, err := NewCorpus(source.Records)
	if err != nil {
		return Corpus{}, fmt.Errorf("review source: %w", err)
	}
	if source.SHA256 != canonical.SHA256 {
		return Corpus{}, fmt.Errorf(
			"review source: corpus_sha256 %q does not match canonical source %q",
			source.SHA256,
			canonical.SHA256,
		)
	}

	return canonical, nil
}

func canonicalReviewSpec(spec ReviewAssignmentSpec) (ReviewAssignmentSpec, error) {
	if err := validateIdentifier("review_set_id", spec.ReviewSetID); err != nil {
		return ReviewAssignmentSpec{}, err
	}
	if err := validateIdentifier("reviewer_id", spec.ReviewerID); err != nil {
		return ReviewAssignmentSpec{}, err
	}
	if err := validateTag("policy_version", spec.PolicyVersion); err != nil {
		return ReviewAssignmentSpec{}, err
	}
	if err := spec.ReviewProof.Validate(); err != nil {
		return ReviewAssignmentSpec{}, fmt.Errorf("review_proof: %w", err)
	}
	if spec.ReviewProof.Workflow != ReviewWorkflowContentJunk {
		return ReviewAssignmentSpec{}, fmt.Errorf(
			"review_proof.workflow: got %q, want %q",
			spec.ReviewProof.Workflow,
			ReviewWorkflowContentJunk,
		)
	}
	if spec.PolicyVersion != spec.ReviewProof.PolicyVersion {
		return ReviewAssignmentSpec{}, fmt.Errorf(
			"policy_version does not match review_proof",
		)
	}
	if len(spec.Tasks) == 0 {
		return ReviewAssignmentSpec{}, fmt.Errorf("review tasks: at least one task is required")
	}

	tasks := append([]Task(nil), spec.Tasks...)
	sort.Slice(tasks, func(i, j int) bool { return tasks[i] < tasks[j] })
	for i, task := range tasks {
		if task != TaskContentFilter && task != TaskJunkPurge {
			return ReviewAssignmentSpec{}, fmt.Errorf(
				"review tasks[%d]: task %q is not human-reviewable",
				i,
				task,
			)
		}
		if i > 0 && task == tasks[i-1] {
			return ReviewAssignmentSpec{}, fmt.Errorf(
				"review tasks[%d]: duplicate task %q",
				i,
				task,
			)
		}
	}
	spec.Tasks = tasks

	return spec, nil
}

func cloneReviewAssignment(in ReviewAssignment) ReviewAssignment {
	out := in
	out.Tasks = append([]Task(nil), in.Tasks...)
	out.Cases = make([]ReviewCaseRecord, len(in.Cases))
	for i, reviewCase := range in.Cases {
		out.Cases[i] = reviewCase
		out.Cases[i].Tasks = append([]Task(nil), reviewCase.Tasks...)
		if reviewCase.ContentFilter != nil {
			payload := *reviewCase.ContentFilter
			out.Cases[i].ContentFilter = &payload
		}
		if reviewCase.JunkPurge != nil {
			payload := *reviewCase.JunkPurge
			out.Cases[i].JunkPurge = &payload
		}
	}

	return out
}

func containsTask(tasks []Task, target Task) bool {
	for _, task := range tasks {
		if task == target {
			return true
		}
	}

	return false
}

func tasksIdentity(tasks []Task) string {
	values := make([]string, len(tasks))
	for i, task := range tasks {
		values[i] = string(task)
	}

	return hashReviewFields("llmeval-review-tasks-v1", values...)
}

func hashReviewFields(domain string, fields ...string) string {
	hash := sha256.New()
	for _, field := range append([]string{domain}, fields...) {
		var length [8]byte
		value := uint64(len(field))
		for i := 7; i >= 0; i-- {
			length[i] = byte(value)
			value >>= 8
		}
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(field))
	}

	return hex.EncodeToString(hash.Sum(nil))
}

func opaqueReviewCaseID(assignmentID, sourceCaseID string) string {
	return "review:" + hashReviewFields(
		"llmeval-review-case-alias-v1",
		assignmentID,
		sourceCaseID,
	)
}

func reviewOrderKey(assignmentID, sourceCaseID string) [sha256.Size]byte {
	return sha256.Sum256([]byte(
		"llmeval-review-order-v1\x00" + assignmentID + "\x00" + sourceCaseID,
	))
}

type reviewPair struct {
	caseID string
	task   Task
	a      ReviewLabel
	b      ReviewLabel
}

func (p reviewPair) needsAdjudication() bool {
	return p.a != p.b || p.a == ReviewLabelAmbiguous
}

type confusionKey struct {
	task Task
	a    ReviewLabel
	b    ReviewLabel
}

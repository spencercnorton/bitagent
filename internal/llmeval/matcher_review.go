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

// MatcherReviewAssignmentSpec identifies one reviewer's blinded matcher work
// packet. Matcher reviews deliberately use a separate schema from the
// categorical content/junk ReviewLabel workflow.
type MatcherReviewAssignmentSpec struct {
	ReviewSetID   string
	ReviewerID    string
	PolicyVersion string
	ReviewProof   HumanReviewProof
	Tasks         []Task
}

// MatcherReviewAssignment is a deterministic input-only view of frozen
// matcher cases. Expected values and all source/model provenance are omitted.
type MatcherReviewAssignment struct {
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
	Cases         []MatcherReviewCaseRecord
}

// MatcherReviewCaseRecord contains only the production-visible frozen input.
// ReviewCaseID is reviewer-specific and does not reveal the source case ID.
type MatcherReviewCaseRecord struct {
	SchemaVersion  int                  `json:"schema_version"`
	AssignmentID   string               `json:"assignment_id"`
	Kind           ReviewAssignmentKind `json:"kind"`
	ReviewSetID    string               `json:"review_set_id"`
	ReviewerID     string               `json:"reviewer_id"`
	PolicyVersion  string               `json:"policy_version"`
	ReviewProof    HumanReviewProof     `json:"review_proof"`
	CorpusSHA256   string               `json:"corpus_sha256"`
	CaseSetSHA256  string               `json:"case_set_sha256"`
	Tasks          []Task               `json:"tasks"`
	Ordinal        int                  `json:"ordinal"`
	ReviewCaseID   string               `json:"review_case_id"`
	Task           Task                 `json:"task"`
	InputSHA256    string               `json:"input_sha256"`
	MatcherExtract *MatcherExtractInput `json:"matcher_extract,omitempty"`
	MatcherRerank  *MatcherRerankInput  `json:"matcher_rerank,omitempty"`
}

// MatcherExtractReviewDecision is expected-shaped human truth. Reviewers may
// enumerate equivalent tuples and may explicitly allow an abstention.
// Ambiguous decisions must contain no concrete answer and cannot become gold.
type MatcherExtractReviewDecision struct {
	Acceptable   []MatcherExtraction `json:"acceptable,omitempty"`
	AllowAbstain bool                `json:"allow_abstain,omitempty"`
	Ambiguous    bool                `json:"ambiguous,omitempty"`
}

// MatcherRerankReviewDecision is expected-shaped human truth. Every acceptable
// ID must be present in the frozen candidate list. Ambiguous decisions cannot
// become gold.
type MatcherRerankReviewDecision struct {
	AcceptableTMDBIDs []int64 `json:"acceptable_tmdb_ids,omitempty"`
	AllowAbstain      bool    `json:"allow_abstain,omitempty"`
	Ambiguous         bool    `json:"ambiguous,omitempty"`
}

// MatcherReviewResponseRecord contains exactly one task-appropriate structured
// decision and no notes or arbitrary metadata.
type MatcherReviewResponseRecord struct {
	SchemaVersion  int                           `json:"schema_version"`
	AssignmentID   string                        `json:"assignment_id"`
	ReviewerID     string                        `json:"reviewer_id"`
	ReviewProof    HumanReviewProof              `json:"review_proof"`
	ReviewCaseID   string                        `json:"review_case_id"`
	Task           Task                          `json:"task"`
	MatcherExtract *MatcherExtractReviewDecision `json:"matcher_extract,omitempty"`
	MatcherRerank  *MatcherRerankReviewDecision  `json:"matcher_rerank,omitempty"`
}

// MatcherReviewSubmission is a validated complete response to one assignment.
type MatcherReviewSubmission struct {
	Assignment MatcherReviewAssignment
	Responses  []MatcherReviewResponseRecord
}

// MatcherReviewDecision is the task-tagged decision representation used by
// agreement reports. Exactly one payload is present.
type MatcherReviewDecision struct {
	MatcherExtract *MatcherExtractReviewDecision `json:"matcher_extract,omitempty"`
	MatcherRerank  *MatcherRerankReviewDecision  `json:"matcher_rerank,omitempty"`
}

// MatcherReviewConfusionCell records an exact structured decision pairing.
type MatcherReviewConfusionCell struct {
	Task              Task                  `json:"task"`
	ReviewerADecision MatcherReviewDecision `json:"reviewer_a_decision"`
	ReviewerBDecision MatcherReviewDecision `json:"reviewer_b_decision"`
	Count             int                   `json:"count"`
}

// MatcherReviewAgreementReport summarizes exact structured agreement. Source
// IDs occur only in NeedsAdjudicationCaseIDs for operator-side routing.
type MatcherReviewAgreementReport struct {
	CorpusSHA256             string                       `json:"corpus_sha256"`
	ReviewSetID              string                       `json:"review_set_id"`
	PolicyVersion            string                       `json:"policy_version"`
	ReviewProof              HumanReviewProof             `json:"review_proof"`
	ReviewerA                string                       `json:"reviewer_a"`
	ReviewerB                string                       `json:"reviewer_b"`
	Overall                  AgreementStats               `json:"overall"`
	ByTask                   []TaskAgreementStats         `json:"by_task"`
	Confusion                []MatcherReviewConfusionCell `json:"confusion"`
	NeedsAdjudicationCaseIDs []string                     `json:"needs_adjudication_case_ids"`
}

// MatcherGoldPromotion is the reviewed subset safe to score. GoldCorpus is nil
// when every matcher case remains ambiguous.
type MatcherGoldPromotion struct {
	GoldCorpus        *Corpus
	Agreement         MatcherReviewAgreementReport
	AdjudicatedCases  int
	UnresolvedCaseIDs []string
}

// NewMatcherReviewAssignment creates a complete primary assignment for the
// selected matcher tasks.
func NewMatcherReviewAssignment(
	source Corpus,
	spec MatcherReviewAssignmentSpec,
) (MatcherReviewAssignment, error) {
	canonical, err := validateFrozenReviewSource(source)
	if err != nil {
		return MatcherReviewAssignment{}, err
	}
	spec, err = canonicalMatcherReviewSpec(spec)
	if err != nil {
		return MatcherReviewAssignment{}, err
	}

	caseIDs := make([]string, 0, len(canonical.Records))
	for _, record := range canonical.Records {
		if containsTask(spec.Tasks, record.Task) {
			caseIDs = append(caseIDs, record.CaseID)
		}
	}
	if len(caseIDs) == 0 {
		return MatcherReviewAssignment{}, fmt.Errorf(
			"matcher review assignment: no cases match requested tasks",
		)
	}
	return buildMatcherReviewAssignment(
		canonical,
		spec,
		ReviewAssignmentPrimary,
		caseIDs,
	)
}

// NewMatcherReviewSubmission validates exact coverage and canonicalizes
// equivalent-answer sets before agreement is measured.
func NewMatcherReviewSubmission(
	source Corpus,
	assignment MatcherReviewAssignment,
	responses []MatcherReviewResponseRecord,
) (MatcherReviewSubmission, error) {
	if err := ValidateMatcherReviewAssignment(source, assignment); err != nil {
		return MatcherReviewSubmission{}, err
	}
	if len(responses) != len(assignment.Cases) {
		return MatcherReviewSubmission{}, fmt.Errorf(
			"matcher review responses: got %d rows, want %d",
			len(responses),
			len(assignment.Cases),
		)
	}

	caseByID := make(map[string]MatcherReviewCaseRecord, len(assignment.Cases))
	for _, reviewCase := range assignment.Cases {
		caseByID[reviewCase.ReviewCaseID] = reviewCase
	}
	responseByID := make(map[string]MatcherReviewResponseRecord, len(responses))
	for i, input := range responses {
		response := canonicalMatcherReviewResponse(input)
		reviewCase, exists := caseByID[response.ReviewCaseID]
		if !exists {
			return MatcherReviewSubmission{}, fmt.Errorf(
				"matcher review response %d: review_case_id %q is not in assignment",
				i+1,
				response.ReviewCaseID,
			)
		}
		if err := response.validateForAssignment(assignment, reviewCase); err != nil {
			return MatcherReviewSubmission{}, fmt.Errorf(
				"matcher review response %d: %w",
				i+1,
				err,
			)
		}
		if _, exists := responseByID[response.ReviewCaseID]; exists {
			return MatcherReviewSubmission{}, fmt.Errorf(
				"matcher review response %d: duplicate review_case_id %q",
				i+1,
				response.ReviewCaseID,
			)
		}
		responseByID[response.ReviewCaseID] = response
	}

	canonicalResponses := make([]MatcherReviewResponseRecord, 0, len(assignment.Cases))
	for _, reviewCase := range assignment.Cases {
		response, exists := responseByID[reviewCase.ReviewCaseID]
		if !exists {
			return MatcherReviewSubmission{}, fmt.Errorf(
				"matcher review responses: missing review_case_id %q",
				reviewCase.ReviewCaseID,
			)
		}
		canonicalResponses = append(canonicalResponses, response)
	}
	return MatcherReviewSubmission{
		Assignment: cloneMatcherReviewAssignment(assignment),
		Responses:  canonicalResponses,
	}, nil
}

// ComparePrimaryMatcherReviews validates two independent submissions and
// reports exact set/tuple agreement. Any disagreement or ambiguous consensus
// requires independent adjudication.
func ComparePrimaryMatcherReviews(
	source Corpus,
	reviewerA MatcherReviewSubmission,
	reviewerB MatcherReviewSubmission,
) (MatcherReviewAgreementReport, error) {
	canonical, pairs, err := primaryMatcherReviewPairs(source, reviewerA, reviewerB)
	if err != nil {
		return MatcherReviewAgreementReport{}, err
	}
	report := MatcherReviewAgreementReport{
		CorpusSHA256:  canonical.SHA256,
		ReviewSetID:   reviewerA.Assignment.ReviewSetID,
		PolicyVersion: reviewerA.Assignment.PolicyVersion,
		ReviewProof:   reviewerA.Assignment.ReviewProof,
		ReviewerA:     reviewerA.Assignment.ReviewerID,
		ReviewerB:     reviewerB.Assignment.ReviewerID,
		Overall:       calculateMatcherAgreementStats(pairs, true),
	}

	byTask := make(map[Task][]matcherReviewPair)
	type confusionEntry struct {
		a     MatcherReviewDecision
		b     MatcherReviewDecision
		count int
	}
	confusion := make(map[string]confusionEntry)
	for _, pair := range pairs {
		byTask[pair.task] = append(byTask[pair.task], pair)
		key := string(pair.task) + "\x00" + matcherDecisionIdentity(pair.a) +
			"\x00" + matcherDecisionIdentity(pair.b)
		entry := confusion[key]
		entry.a = cloneMatcherReviewDecision(pair.a)
		entry.b = cloneMatcherReviewDecision(pair.b)
		entry.count++
		confusion[key] = entry
		if pair.needsAdjudication() {
			report.NeedsAdjudicationCaseIDs = append(
				report.NeedsAdjudicationCaseIDs,
				pair.caseID,
			)
		}
	}
	sort.Strings(report.NeedsAdjudicationCaseIDs)
	for _, task := range orderedTasks {
		if len(byTask[task]) == 0 {
			continue
		}
		report.ByTask = append(report.ByTask, TaskAgreementStats{
			Task:           task,
			AgreementStats: calculateMatcherAgreementStats(byTask[task], false),
		})
	}
	keys := make([]string, 0, len(confusion))
	for key := range confusion {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		entry := confusion[key]
		task := entry.a.task()
		report.Confusion = append(report.Confusion, MatcherReviewConfusionCell{
			Task:              task,
			ReviewerADecision: entry.a,
			ReviewerBDecision: entry.b,
			Count:             entry.count,
		})
	}
	return report, nil
}

// NewMatcherAdjudicationAssignment creates a fresh opaque packet containing
// exactly structured disagreements and ambiguous agreements.
func NewMatcherAdjudicationAssignment(
	source Corpus,
	adjudicatorID string,
	reviewerA MatcherReviewSubmission,
	reviewerB MatcherReviewSubmission,
) (MatcherReviewAssignment, MatcherReviewAgreementReport, error) {
	report, err := ComparePrimaryMatcherReviews(source, reviewerA, reviewerB)
	if err != nil {
		return MatcherReviewAssignment{}, MatcherReviewAgreementReport{}, err
	}
	if len(report.NeedsAdjudicationCaseIDs) == 0 {
		return MatcherReviewAssignment{}, report, fmt.Errorf(
			"matcher adjudication assignment: no cases require adjudication",
		)
	}
	if adjudicatorID == reviewerA.Assignment.ReviewerID ||
		adjudicatorID == reviewerB.Assignment.ReviewerID {
		return MatcherReviewAssignment{}, report, fmt.Errorf(
			"matcher adjudication assignment: adjudicator must be independent of primary reviewers",
		)
	}
	canonical, err := validateFrozenReviewSource(source)
	if err != nil {
		return MatcherReviewAssignment{}, report, err
	}
	spec, err := canonicalMatcherReviewSpec(MatcherReviewAssignmentSpec{
		ReviewSetID:   report.ReviewSetID,
		ReviewerID:    adjudicatorID,
		PolicyVersion: report.PolicyVersion,
		ReviewProof:   report.ReviewProof,
		Tasks:         reviewerA.Assignment.Tasks,
	})
	if err != nil {
		return MatcherReviewAssignment{}, report, err
	}
	assignment, err := buildMatcherReviewAssignment(
		canonical,
		spec,
		ReviewAssignmentAdjudication,
		report.NeedsAdjudicationCaseIDs,
	)
	if err != nil {
		return MatcherReviewAssignment{}, report, err
	}
	return assignment, report, nil
}

// PromoteMatcherGold resolves primary reviews and required adjudication.
// Concrete expected-shaped answers become human_review/gold; ambiguity is
// reported and excluded.
func PromoteMatcherGold(
	source Corpus,
	reviewerA MatcherReviewSubmission,
	reviewerB MatcherReviewSubmission,
	adjudication *MatcherReviewSubmission,
) (MatcherGoldPromotion, error) {
	canonical, pairs, err := primaryMatcherReviewPairs(source, reviewerA, reviewerB)
	if err != nil {
		return MatcherGoldPromotion{}, err
	}
	report, err := ComparePrimaryMatcherReviews(canonical, reviewerA, reviewerB)
	if err != nil {
		return MatcherGoldPromotion{}, err
	}

	adjudicated := make(map[string]MatcherReviewDecision)
	if len(report.NeedsAdjudicationCaseIDs) > 0 {
		if adjudication == nil {
			return MatcherGoldPromotion{}, fmt.Errorf(
				"matcher gold promotion: %d cases require independent adjudication",
				len(report.NeedsAdjudicationCaseIDs),
			)
		}
		if err := validateMatcherAdjudicationSubmission(
			canonical,
			reviewerA,
			reviewerB,
			*adjudication,
			report.NeedsAdjudicationCaseIDs,
		); err != nil {
			return MatcherGoldPromotion{}, err
		}
		adjudicated, err = matcherSubmissionDecisionsBySourceCase(
			canonical,
			*adjudication,
		)
		if err != nil {
			return MatcherGoldPromotion{}, err
		}
	} else if adjudication != nil {
		return MatcherGoldPromotion{}, fmt.Errorf(
			"matcher gold promotion: adjudication supplied but no case requires it",
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
		decision := pair.a
		reviewerCount := 2
		if pair.needsAdjudication() {
			decision = adjudicated[pair.caseID]
			reviewerCount = 3
			adjudicatedCount++
		}
		if decision.ambiguous() {
			unresolved = append(unresolved, pair.caseID)
			continue
		}
		record := sourceByID[pair.caseID]
		if err := applyMatcherGoldDecision(&record, decision); err != nil {
			return MatcherGoldPromotion{}, fmt.Errorf(
				"matcher gold promotion case %q: %w",
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
	result := MatcherGoldPromotion{
		Agreement:         report,
		AdjudicatedCases:  adjudicatedCount,
		UnresolvedCaseIDs: unresolved,
	}
	if len(promoted) == 0 {
		return result, nil
	}
	gold, err := NewCorpus(promoted)
	if err != nil {
		return MatcherGoldPromotion{}, fmt.Errorf("matcher gold promotion: %w", err)
	}
	result.GoldCorpus = &gold
	return result, nil
}

// ValidateMatcherReviewAssignment proves the packet is a deterministic
// input-only projection of the exact frozen source corpus.
func ValidateMatcherReviewAssignment(
	source Corpus,
	assignment MatcherReviewAssignment,
) error {
	canonical, err := validateFrozenReviewSource(source)
	if err != nil {
		return err
	}
	if err := assignment.validateStructure(); err != nil {
		return err
	}
	if assignment.CorpusSHA256 != canonical.SHA256 {
		return fmt.Errorf(
			"matcher review assignment: corpus_sha256 %q does not match source %q",
			assignment.CorpusSHA256,
			canonical.SHA256,
		)
	}

	reviewIDs := make(map[string]struct{}, len(assignment.Cases))
	for _, reviewCase := range assignment.Cases {
		reviewIDs[reviewCase.ReviewCaseID] = struct{}{}
	}
	selectedSourceIDs := make([]string, 0, len(assignment.Cases))
	for _, record := range canonical.Records {
		if !containsTask(assignment.Tasks, record.Task) {
			continue
		}
		reviewID := opaqueMatcherReviewCaseID(assignment.AssignmentID, record.CaseID)
		if _, exists := reviewIDs[reviewID]; exists {
			selectedSourceIDs = append(selectedSourceIDs, record.CaseID)
			continue
		}
		if assignment.Kind == ReviewAssignmentPrimary {
			return fmt.Errorf(
				"matcher review assignment: primary assignment is missing source case %q",
				record.CaseID,
			)
		}
	}
	if len(selectedSourceIDs) != len(assignment.Cases) {
		return fmt.Errorf(
			"matcher review assignment: contains a case not found in frozen source",
		)
	}
	expected, err := buildMatcherReviewAssignment(
		canonical,
		MatcherReviewAssignmentSpec{
			ReviewSetID:   assignment.ReviewSetID,
			ReviewerID:    assignment.ReviewerID,
			PolicyVersion: assignment.PolicyVersion,
			ReviewProof:   assignment.ReviewProof,
			Tasks:         assignment.Tasks,
		},
		assignment.Kind,
		selectedSourceIDs,
	)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(expected, assignment) {
		return fmt.Errorf(
			"matcher review assignment: frozen metadata, order, alias, or input mismatch",
		)
	}
	return nil
}

func buildMatcherReviewAssignment(
	source Corpus,
	spec MatcherReviewAssignmentSpec,
	kind ReviewAssignmentKind,
	caseIDs []string,
) (MatcherReviewAssignment, error) {
	if kind != ReviewAssignmentPrimary && kind != ReviewAssignmentAdjudication {
		return MatcherReviewAssignment{}, fmt.Errorf(
			"matcher review assignment: unsupported kind %q",
			kind,
		)
	}
	if len(caseIDs) == 0 {
		return MatcherReviewAssignment{}, fmt.Errorf(
			"matcher review assignment: case set is empty",
		)
	}
	sourceByID := make(map[string]CorpusRecord, len(source.Records))
	for _, record := range source.Records {
		sourceByID[record.CaseID] = record
	}
	unique := make(map[string]struct{}, len(caseIDs))
	canonicalIDs := make([]string, 0, len(caseIDs))
	for _, caseID := range caseIDs {
		if _, exists := unique[caseID]; exists {
			return MatcherReviewAssignment{}, fmt.Errorf(
				"matcher review assignment: duplicate source case %q",
				caseID,
			)
		}
		record, exists := sourceByID[caseID]
		if !exists {
			return MatcherReviewAssignment{}, fmt.Errorf(
				"matcher review assignment: source case %q does not exist",
				caseID,
			)
		}
		if !containsTask(spec.Tasks, record.Task) {
			return MatcherReviewAssignment{}, fmt.Errorf(
				"matcher review assignment: source case %q task %q is not selected",
				caseID,
				record.Task,
			)
		}
		unique[caseID] = struct{}{}
		canonicalIDs = append(canonicalIDs, caseID)
	}
	sort.Strings(canonicalIDs)
	caseSetSHA := hashReviewFields("llmeval-matcher-review-case-set-v1", canonicalIDs...)
	assignmentID := hashReviewFields(
		"llmeval-matcher-review-assignment-v1",
		fmt.Sprint(SchemaVersion),
		string(kind),
		source.SHA256,
		caseSetSHA,
		spec.ReviewSetID,
		spec.ReviewerID,
		spec.PolicyVersion,
		reviewProofIdentity(spec.ReviewProof),
		matcherReviewTasksIdentity(spec.Tasks),
	)
	sort.Slice(canonicalIDs, func(i, j int) bool {
		left := matcherReviewOrderKey(assignmentID, canonicalIDs[i])
		right := matcherReviewOrderKey(assignmentID, canonicalIDs[j])
		if cmp := bytes.Compare(left[:], right[:]); cmp != 0 {
			return cmp < 0
		}
		return canonicalIDs[i] < canonicalIDs[j]
	})

	assignment := MatcherReviewAssignment{
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
		Cases:         make([]MatcherReviewCaseRecord, 0, len(canonicalIDs)),
	}
	for i, caseID := range canonicalIDs {
		reviewCase, err := makeMatcherReviewCase(
			assignment,
			i+1,
			caseID,
			sourceByID[caseID],
		)
		if err != nil {
			return MatcherReviewAssignment{}, err
		}
		assignment.Cases = append(assignment.Cases, reviewCase)
	}
	if err := assignment.validateStructure(); err != nil {
		return MatcherReviewAssignment{}, err
	}
	return assignment, nil
}

func makeMatcherReviewCase(
	assignment MatcherReviewAssignment,
	ordinal int,
	sourceCaseID string,
	record CorpusRecord,
) (MatcherReviewCaseRecord, error) {
	reviewCase := MatcherReviewCaseRecord{
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
		ReviewCaseID:  opaqueMatcherReviewCaseID(assignment.AssignmentID, sourceCaseID),
		Task:          record.Task,
	}
	var input any
	switch record.Task {
	case TaskMatcherExtract:
		payload := cloneMatcherExtractInput(record.MatcherExtract.Input)
		reviewCase.MatcherExtract = &payload
		input = struct {
			Task  Task                `json:"task"`
			Input MatcherExtractInput `json:"input"`
		}{Task: record.Task, Input: payload}
	case TaskMatcherRerank:
		payload := cloneMatcherRerankInput(record.MatcherRerank.Input)
		reviewCase.MatcherRerank = &payload
		input = struct {
			Task  Task               `json:"task"`
			Input MatcherRerankInput `json:"input"`
		}{Task: record.Task, Input: payload}
	default:
		return MatcherReviewCaseRecord{}, fmt.Errorf(
			"matcher review case %q: task %q is not matcher-reviewable",
			sourceCaseID,
			record.Task,
		)
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return MatcherReviewCaseRecord{}, fmt.Errorf(
			"matcher review case %q: marshal input: %w",
			sourceCaseID,
			err,
		)
	}
	sum := sha256.Sum256(raw)
	reviewCase.InputSHA256 = hex.EncodeToString(sum[:])
	return reviewCase, nil
}

func (a MatcherReviewAssignment) validateStructure() error {
	if a.SchemaVersion != SchemaVersion {
		return fmt.Errorf(
			"matcher review assignment: schema_version got %d, want %d",
			a.SchemaVersion,
			SchemaVersion,
		)
	}
	if err := validateIdentifier("matcher review assignment.assignment_id", a.AssignmentID); err != nil {
		return err
	}
	if a.Kind != ReviewAssignmentPrimary && a.Kind != ReviewAssignmentAdjudication {
		return fmt.Errorf("matcher review assignment: unsupported kind %q", a.Kind)
	}
	if err := validateIdentifier("matcher review assignment.review_set_id", a.ReviewSetID); err != nil {
		return err
	}
	if err := validateIdentifier("matcher review assignment.reviewer_id", a.ReviewerID); err != nil {
		return err
	}
	if err := validateTag("matcher review assignment.policy_version", a.PolicyVersion); err != nil {
		return err
	}
	if err := a.ReviewProof.Validate(); err != nil {
		return fmt.Errorf("matcher review assignment.review_proof: %w", err)
	}
	if a.ReviewProof.Workflow != ReviewWorkflowMatcher {
		return fmt.Errorf(
			"matcher review assignment.review_proof: workflow must be %q",
			ReviewWorkflowMatcher,
		)
	}
	if a.PolicyVersion != a.ReviewProof.PolicyVersion {
		return fmt.Errorf(
			"matcher review assignment.policy_version does not match review_proof",
		)
	}
	if err := validateSHA256("matcher review assignment.corpus_sha256", a.CorpusSHA256); err != nil {
		return err
	}
	if a.ReviewProof.EvidenceCorpusSHA256 != a.CorpusSHA256 {
		return fmt.Errorf(
			"matcher review assignment.review_proof evidence corpus does not match assignment",
		)
	}
	if err := validateSHA256("matcher review assignment.case_set_sha256", a.CaseSetSHA256); err != nil {
		return err
	}
	spec, err := canonicalMatcherReviewSpec(MatcherReviewAssignmentSpec{
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
		return fmt.Errorf("matcher review assignment.tasks: must be sorted and unique")
	}
	if len(a.Cases) == 0 {
		return fmt.Errorf("matcher review assignment: cases are empty")
	}
	expectedID := hashReviewFields(
		"llmeval-matcher-review-assignment-v1",
		fmt.Sprint(SchemaVersion),
		string(a.Kind),
		a.CorpusSHA256,
		a.CaseSetSHA256,
		a.ReviewSetID,
		a.ReviewerID,
		a.PolicyVersion,
		reviewProofIdentity(a.ReviewProof),
		matcherReviewTasksIdentity(a.Tasks),
	)
	if a.AssignmentID != expectedID {
		return fmt.Errorf(
			"matcher review assignment: assignment_id does not match frozen metadata",
		)
	}
	seen := make(map[string]struct{}, len(a.Cases))
	for i, reviewCase := range a.Cases {
		if err := reviewCase.validate(); err != nil {
			return fmt.Errorf("matcher review assignment case %d: %w", i+1, err)
		}
		if reviewCase.Ordinal != i+1 {
			return fmt.Errorf(
				"matcher review assignment case %d: ordinal got %d, want %d",
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
			return fmt.Errorf(
				"matcher review assignment case %d: assignment metadata mismatch",
				i+1,
			)
		}
		if _, exists := seen[reviewCase.ReviewCaseID]; exists {
			return fmt.Errorf(
				"matcher review assignment case %d: duplicate review_case_id %q",
				i+1,
				reviewCase.ReviewCaseID,
			)
		}
		seen[reviewCase.ReviewCaseID] = struct{}{}
	}
	return nil
}

func (r MatcherReviewCaseRecord) validate() error {
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
	payloads := boolInt(r.MatcherExtract != nil) + boolInt(r.MatcherRerank != nil)
	if payloads != 1 {
		return fmt.Errorf("input payload: exactly one payload is required, got %d", payloads)
	}
	switch r.Task {
	case TaskMatcherExtract:
		if r.MatcherExtract == nil {
			return fmt.Errorf("matcher_extract input is required")
		}
		return validateMatcherExtractInput(*r.MatcherExtract)
	case TaskMatcherRerank:
		if r.MatcherRerank == nil {
			return fmt.Errorf("matcher_rerank input is required")
		}
		return validateMatcherRerankInput(*r.MatcherRerank)
	default:
		return fmt.Errorf("task %q is not matcher-reviewable", r.Task)
	}
}

func (r MatcherReviewResponseRecord) validateForAssignment(
	assignment MatcherReviewAssignment,
	reviewCase MatcherReviewCaseRecord,
) error {
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
	if r.Task != reviewCase.Task {
		return fmt.Errorf("task %q does not match assigned task %q", r.Task, reviewCase.Task)
	}
	payloads := boolInt(r.MatcherExtract != nil) + boolInt(r.MatcherRerank != nil)
	if payloads != 1 {
		return fmt.Errorf("decision: exactly one payload is required, got %d", payloads)
	}
	switch r.Task {
	case TaskMatcherExtract:
		if r.MatcherExtract == nil {
			return fmt.Errorf("matcher_extract decision is required")
		}
		return r.MatcherExtract.validate()
	case TaskMatcherRerank:
		if r.MatcherRerank == nil {
			return fmt.Errorf("matcher_rerank decision is required")
		}
		return r.MatcherRerank.validate(reviewCase.MatcherRerank.Candidates)
	default:
		return fmt.Errorf("task %q is not matcher-reviewable", r.Task)
	}
}

func (d MatcherExtractReviewDecision) validate() error {
	if d.Ambiguous {
		if len(d.Acceptable) != 0 || d.AllowAbstain {
			return fmt.Errorf(
				"matcher_extract decision: ambiguous cannot include a concrete answer",
			)
		}
		return nil
	}
	if len(d.Acceptable) == 0 && !d.AllowAbstain {
		return fmt.Errorf(
			"matcher_extract decision: acceptable output, allow_abstain, or ambiguous is required",
		)
	}
	seen := make(map[MatcherExtraction]struct{}, len(d.Acceptable))
	for i, extraction := range d.Acceptable {
		if err := extraction.Validate(); err != nil {
			return fmt.Errorf("matcher_extract decision.acceptable[%d]: %w", i, err)
		}
		if _, exists := seen[extraction]; exists {
			return fmt.Errorf(
				"matcher_extract decision.acceptable[%d]: duplicate output",
				i,
			)
		}
		seen[extraction] = struct{}{}
	}
	return nil
}

func (d MatcherRerankReviewDecision) validate(candidates []MatcherCandidate) error {
	if d.Ambiguous {
		if len(d.AcceptableTMDBIDs) != 0 || d.AllowAbstain {
			return fmt.Errorf(
				"matcher_rerank decision: ambiguous cannot include a concrete answer",
			)
		}
		return nil
	}
	if len(d.AcceptableTMDBIDs) == 0 && !d.AllowAbstain {
		return fmt.Errorf(
			"matcher_rerank decision: acceptable id, allow_abstain, or ambiguous is required",
		)
	}
	candidateIDs := make(map[int64]struct{}, len(candidates))
	for _, candidate := range candidates {
		candidateIDs[candidate.TMDBID] = struct{}{}
	}
	seen := make(map[int64]struct{}, len(d.AcceptableTMDBIDs))
	for i, id := range d.AcceptableTMDBIDs {
		if id <= 0 {
			return fmt.Errorf(
				"matcher_rerank decision.acceptable_tmdb_ids[%d]: must be positive",
				i,
			)
		}
		if _, exists := candidateIDs[id]; !exists {
			return fmt.Errorf(
				"matcher_rerank decision.acceptable_tmdb_ids[%d]: %d is not in candidates",
				i,
				id,
			)
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf(
				"matcher_rerank decision.acceptable_tmdb_ids[%d]: duplicate %d",
				i,
				id,
			)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func primaryMatcherReviewPairs(
	source Corpus,
	reviewerA MatcherReviewSubmission,
	reviewerB MatcherReviewSubmission,
) (Corpus, []matcherReviewPair, error) {
	canonical, err := validateFrozenReviewSource(source)
	if err != nil {
		return Corpus{}, nil, err
	}
	if reviewerA.Assignment.Kind != ReviewAssignmentPrimary ||
		reviewerB.Assignment.Kind != ReviewAssignmentPrimary {
		return Corpus{}, nil, fmt.Errorf("matcher primary reviews require primary assignments")
	}
	if reviewerA.Assignment.ReviewerID == reviewerB.Assignment.ReviewerID {
		return Corpus{}, nil, fmt.Errorf("matcher primary reviewers must be distinct")
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
			"matcher primary assignments must share review set, policy, corpus, and tasks",
		)
	}
	if err := validateMatcherSubmission(canonical, reviewerA); err != nil {
		return Corpus{}, nil, fmt.Errorf("matcher reviewer A: %w", err)
	}
	if err := validateMatcherSubmission(canonical, reviewerB); err != nil {
		return Corpus{}, nil, fmt.Errorf("matcher reviewer B: %w", err)
	}
	decisionsA, err := matcherSubmissionDecisionsBySourceCase(canonical, reviewerA)
	if err != nil {
		return Corpus{}, nil, err
	}
	decisionsB, err := matcherSubmissionDecisionsBySourceCase(canonical, reviewerB)
	if err != nil {
		return Corpus{}, nil, err
	}
	caseIDs := make([]string, 0, len(decisionsA))
	taskByID := make(map[string]Task, len(decisionsA))
	for _, record := range canonical.Records {
		if _, exists := decisionsA[record.CaseID]; !exists {
			continue
		}
		if _, exists := decisionsB[record.CaseID]; !exists {
			return Corpus{}, nil, fmt.Errorf(
				"matcher primary assignments cover different case sets",
			)
		}
		caseIDs = append(caseIDs, record.CaseID)
		taskByID[record.CaseID] = record.Task
	}
	if len(caseIDs) != len(decisionsA) || len(caseIDs) != len(decisionsB) {
		return Corpus{}, nil, fmt.Errorf("matcher primary assignments cover different case sets")
	}
	sort.Strings(caseIDs)
	pairs := make([]matcherReviewPair, 0, len(caseIDs))
	for _, caseID := range caseIDs {
		pairs = append(pairs, matcherReviewPair{
			caseID: caseID,
			task:   taskByID[caseID],
			a:      decisionsA[caseID],
			b:      decisionsB[caseID],
		})
	}
	return canonical, pairs, nil
}

func validateMatcherAdjudicationSubmission(
	source Corpus,
	reviewerA MatcherReviewSubmission,
	reviewerB MatcherReviewSubmission,
	adjudication MatcherReviewSubmission,
	requiredCaseIDs []string,
) error {
	if adjudication.Assignment.Kind != ReviewAssignmentAdjudication {
		return fmt.Errorf("matcher gold promotion: adjudication assignment has wrong kind")
	}
	if adjudication.Assignment.ReviewerID == reviewerA.Assignment.ReviewerID ||
		adjudication.Assignment.ReviewerID == reviewerB.Assignment.ReviewerID {
		return fmt.Errorf(
			"matcher gold promotion: adjudicator must be independent of primary reviewers",
		)
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
			"matcher gold promotion: adjudication must share review set, policy, corpus, and tasks",
		)
	}
	if err := validateMatcherSubmission(source, adjudication); err != nil {
		return fmt.Errorf("matcher adjudication: %w", err)
	}
	decisions, err := matcherSubmissionDecisionsBySourceCase(source, adjudication)
	if err != nil {
		return err
	}
	if len(decisions) != len(requiredCaseIDs) {
		return fmt.Errorf(
			"matcher gold promotion: adjudication covers %d cases, want %d",
			len(decisions),
			len(requiredCaseIDs),
		)
	}
	for _, caseID := range requiredCaseIDs {
		if _, exists := decisions[caseID]; !exists {
			return fmt.Errorf(
				"matcher gold promotion: adjudication is missing source case %q",
				caseID,
			)
		}
	}
	return nil
}

func validateMatcherSubmission(source Corpus, submission MatcherReviewSubmission) error {
	expected, err := NewMatcherReviewSubmission(
		source,
		submission.Assignment,
		submission.Responses,
	)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(expected, submission) {
		return fmt.Errorf("matcher review submission: non-canonical or mutated submission")
	}
	return nil
}

func matcherSubmissionDecisionsBySourceCase(
	source Corpus,
	submission MatcherReviewSubmission,
) (map[string]MatcherReviewDecision, error) {
	responseByReviewID := make(
		map[string]MatcherReviewResponseRecord,
		len(submission.Responses),
	)
	for _, response := range submission.Responses {
		responseByReviewID[response.ReviewCaseID] = response
	}
	decisions := make(map[string]MatcherReviewDecision, len(submission.Responses))
	for _, record := range source.Records {
		reviewID := opaqueMatcherReviewCaseID(
			submission.Assignment.AssignmentID,
			record.CaseID,
		)
		response, exists := responseByReviewID[reviewID]
		if !exists {
			continue
		}
		decisions[record.CaseID] = response.decision()
	}
	if len(decisions) != len(submission.Responses) {
		return nil, fmt.Errorf(
			"matcher review submission: response alias is not in frozen source",
		)
	}
	return decisions, nil
}

func calculateMatcherAgreementStats(
	pairs []matcherReviewPair,
	includeTask bool,
) AgreementStats {
	stats := AgreementStats{Cases: len(pairs)}
	countA := make(map[string]int)
	countB := make(map[string]int)
	for _, pair := range pairs {
		if pair.agrees() {
			stats.Agreements++
		} else {
			stats.Disagreements++
		}
		if pair.needsAdjudication() {
			stats.NeedsAdjudication++
		}
		categoryA := matcherDecisionIdentity(pair.a)
		categoryB := matcherDecisionIdentity(pair.b)
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
	categories := make(map[string]struct{}, len(countA)+len(countB))
	for category := range countA {
		categories[category] = struct{}{}
	}
	for category := range countB {
		categories[category] = struct{}{}
	}
	for category := range categories {
		stats.ExpectedAgreement +=
			(float64(countA[category]) / float64(stats.Cases)) *
				(float64(countB[category]) / float64(stats.Cases))
	}
	if math.Abs(1-stats.ExpectedAgreement) > 1e-12 {
		kappa := (stats.ObservedAgreement - stats.ExpectedAgreement) /
			(1 - stats.ExpectedAgreement)
		stats.CohensKappa = &kappa
	}
	return stats
}

func applyMatcherGoldDecision(record *CorpusRecord, decision MatcherReviewDecision) error {
	switch record.Task {
	case TaskMatcherExtract:
		if decision.MatcherExtract == nil || decision.MatcherExtract.Ambiguous {
			return fmt.Errorf("invalid matcher_extract gold decision")
		}
		record.MatcherExtract.Expected = MatcherExtractExpected{
			Acceptable: append(
				[]MatcherExtraction(nil),
				decision.MatcherExtract.Acceptable...,
			),
			AllowAbstain: decision.MatcherExtract.AllowAbstain,
		}
	case TaskMatcherRerank:
		if decision.MatcherRerank == nil || decision.MatcherRerank.Ambiguous {
			return fmt.Errorf("invalid matcher_rerank gold decision")
		}
		record.MatcherRerank.Expected = MatcherRerankExpected{
			AcceptableTMDBIDs: append(
				[]int64(nil),
				decision.MatcherRerank.AcceptableTMDBIDs...,
			),
			AllowAbstain: decision.MatcherRerank.AllowAbstain,
		}
	default:
		return fmt.Errorf("task %q is not matcher-reviewable", record.Task)
	}
	return nil
}

func canonicalMatcherReviewSpec(
	spec MatcherReviewAssignmentSpec,
) (MatcherReviewAssignmentSpec, error) {
	if err := validateIdentifier("review_set_id", spec.ReviewSetID); err != nil {
		return MatcherReviewAssignmentSpec{}, err
	}
	if err := validateIdentifier("reviewer_id", spec.ReviewerID); err != nil {
		return MatcherReviewAssignmentSpec{}, err
	}
	if err := validateTag("policy_version", spec.PolicyVersion); err != nil {
		return MatcherReviewAssignmentSpec{}, err
	}
	if err := spec.ReviewProof.Validate(); err != nil {
		return MatcherReviewAssignmentSpec{}, fmt.Errorf("review_proof: %w", err)
	}
	if spec.ReviewProof.Workflow != ReviewWorkflowMatcher {
		return MatcherReviewAssignmentSpec{}, fmt.Errorf(
			"review_proof.workflow: got %q, want %q",
			spec.ReviewProof.Workflow,
			ReviewWorkflowMatcher,
		)
	}
	if spec.PolicyVersion != spec.ReviewProof.PolicyVersion {
		return MatcherReviewAssignmentSpec{}, fmt.Errorf(
			"policy_version does not match review_proof",
		)
	}
	if len(spec.Tasks) == 0 {
		return MatcherReviewAssignmentSpec{}, fmt.Errorf(
			"matcher review tasks: at least one task is required",
		)
	}
	tasks := append([]Task(nil), spec.Tasks...)
	sort.Slice(tasks, func(i, j int) bool { return tasks[i] < tasks[j] })
	for i, task := range tasks {
		if task != TaskMatcherExtract && task != TaskMatcherRerank {
			return MatcherReviewAssignmentSpec{}, fmt.Errorf(
				"matcher review tasks[%d]: task %q is not matcher-reviewable",
				i,
				task,
			)
		}
		if i > 0 && task == tasks[i-1] {
			return MatcherReviewAssignmentSpec{}, fmt.Errorf(
				"matcher review tasks[%d]: duplicate task %q",
				i,
				task,
			)
		}
	}
	spec.Tasks = tasks
	return spec, nil
}

func canonicalMatcherReviewResponse(
	in MatcherReviewResponseRecord,
) MatcherReviewResponseRecord {
	out := in
	if in.MatcherExtract != nil {
		decision := *in.MatcherExtract
		decision.Acceptable = append([]MatcherExtraction(nil), decision.Acceptable...)
		sort.Slice(decision.Acceptable, func(i, j int) bool {
			left, _ := json.Marshal(decision.Acceptable[i])
			right, _ := json.Marshal(decision.Acceptable[j])
			return bytes.Compare(left, right) < 0
		})
		out.MatcherExtract = &decision
	}
	if in.MatcherRerank != nil {
		decision := *in.MatcherRerank
		decision.AcceptableTMDBIDs = append(
			[]int64(nil),
			decision.AcceptableTMDBIDs...,
		)
		sort.Slice(decision.AcceptableTMDBIDs, func(i, j int) bool {
			return decision.AcceptableTMDBIDs[i] < decision.AcceptableTMDBIDs[j]
		})
		out.MatcherRerank = &decision
	}
	return out
}

func cloneMatcherReviewAssignment(in MatcherReviewAssignment) MatcherReviewAssignment {
	out := in
	out.Tasks = append([]Task(nil), in.Tasks...)
	out.Cases = make([]MatcherReviewCaseRecord, len(in.Cases))
	for i, reviewCase := range in.Cases {
		out.Cases[i] = reviewCase
		out.Cases[i].Tasks = append([]Task(nil), reviewCase.Tasks...)
		if reviewCase.MatcherExtract != nil {
			payload := cloneMatcherExtractInput(*reviewCase.MatcherExtract)
			out.Cases[i].MatcherExtract = &payload
		}
		if reviewCase.MatcherRerank != nil {
			payload := cloneMatcherRerankInput(*reviewCase.MatcherRerank)
			out.Cases[i].MatcherRerank = &payload
		}
	}
	return out
}

func cloneMatcherExtractInput(in MatcherExtractInput) MatcherExtractInput {
	out := in
	out.FilePaths = append([]string(nil), in.FilePaths...)
	return out
}

func cloneMatcherRerankInput(in MatcherRerankInput) MatcherRerankInput {
	out := in
	out.Candidates = append([]MatcherCandidate(nil), in.Candidates...)
	for i := range out.Candidates {
		out.Candidates[i].AltTitles = append(
			[]string(nil),
			in.Candidates[i].AltTitles...,
		)
	}
	return out
}

func cloneMatcherReviewDecision(in MatcherReviewDecision) MatcherReviewDecision {
	response := canonicalMatcherReviewResponse(MatcherReviewResponseRecord{
		MatcherExtract: in.MatcherExtract,
		MatcherRerank:  in.MatcherRerank,
	})
	return MatcherReviewDecision{
		MatcherExtract: response.MatcherExtract,
		MatcherRerank:  response.MatcherRerank,
	}
}

func validateMatcherExtractInput(input MatcherExtractInput) error {
	return (MatcherExtractCase{
		Input: input,
		Expected: MatcherExtractExpected{
			AllowAbstain: true,
		},
	}).Validate()
}

func validateMatcherRerankInput(input MatcherRerankInput) error {
	return (MatcherRerankCase{
		Input: input,
		Expected: MatcherRerankExpected{
			AllowAbstain: true,
		},
	}).Validate()
}

func (r MatcherReviewResponseRecord) decision() MatcherReviewDecision {
	return cloneMatcherReviewDecision(MatcherReviewDecision{
		MatcherExtract: r.MatcherExtract,
		MatcherRerank:  r.MatcherRerank,
	})
}

func (d MatcherReviewDecision) task() Task {
	if d.MatcherExtract != nil {
		return TaskMatcherExtract
	}
	return TaskMatcherRerank
}

func (d MatcherReviewDecision) ambiguous() bool {
	if d.MatcherExtract != nil {
		return d.MatcherExtract.Ambiguous
	}
	return d.MatcherRerank != nil && d.MatcherRerank.Ambiguous
}

func matcherDecisionIdentity(decision MatcherReviewDecision) string {
	raw, _ := json.Marshal(decision)
	return string(raw)
}

func matcherReviewTasksIdentity(tasks []Task) string {
	values := make([]string, len(tasks))
	for i, task := range tasks {
		values[i] = string(task)
	}
	return hashReviewFields("llmeval-matcher-review-tasks-v1", values...)
}

func opaqueMatcherReviewCaseID(assignmentID, sourceCaseID string) string {
	return "matcher-review:" + hashReviewFields(
		"llmeval-matcher-review-case-alias-v1",
		assignmentID,
		sourceCaseID,
	)
}

func matcherReviewOrderKey(assignmentID, sourceCaseID string) [sha256.Size]byte {
	return sha256.Sum256([]byte(
		"llmeval-matcher-review-order-v1\x00" + assignmentID + "\x00" + sourceCaseID,
	))
}

type matcherReviewPair struct {
	caseID string
	task   Task
	a      MatcherReviewDecision
	b      MatcherReviewDecision
}

func (p matcherReviewPair) agrees() bool {
	return reflect.DeepEqual(p.a, p.b)
}

func (p matcherReviewPair) needsAdjudication() bool {
	return !p.agrees() || p.a.ambiguous()
}

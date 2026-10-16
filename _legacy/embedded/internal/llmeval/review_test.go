package llmeval

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestReviewAssignmentsAreDeterministicBlindedAndReviewerSpecific(t *testing.T) {
	corpus := reviewFixtureCorpus(t)
	spec := ReviewAssignmentSpec{
		ReviewSetID:   "review:2026-07",
		ReviewerID:    "reviewer:a",
		PolicyVersion: "human-policy-v1",
		ReviewProof: testReviewProof(
			corpus,
			ReviewWorkflowContentJunk,
			"human-policy-v1",
		),
		Tasks: []Task{TaskJunkPurge, TaskContentFilter},
	}

	first, err := NewReviewAssignment(corpus, spec)
	if err != nil {
		t.Fatalf("NewReviewAssignment first: %v", err)
	}
	second, err := NewReviewAssignment(corpus, spec)
	if err != nil {
		t.Fatalf("NewReviewAssignment second: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("identical reviewer assignment is not deterministic")
	}
	if got, want := first.Tasks, []Task{TaskContentFilter, TaskJunkPurge}; !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical tasks = %v, want %v", got, want)
	}

	otherSpec := spec
	otherSpec.ReviewerID = "reviewer:b"
	other, err := NewReviewAssignment(corpus, otherSpec)
	if err != nil {
		t.Fatalf("NewReviewAssignment other reviewer: %v", err)
	}
	if first.AssignmentID == other.AssignmentID {
		t.Fatal("different reviewers received the same assignment ID")
	}
	for i := range first.Cases {
		if first.Cases[i].ReviewCaseID == other.Cases[i].ReviewCaseID {
			t.Fatalf("case %d alias is not reviewer-specific", i)
		}
	}

	firstOrder := sourceOrderForAssignment(t, corpus, first)
	otherOrder := sourceOrderForAssignment(t, corpus, other)
	if reflect.DeepEqual(firstOrder, otherOrder) {
		t.Fatalf(
			"fixture reviewers unexpectedly received the same source order: %v",
			firstOrder,
		)
	}

	for _, reviewCase := range first.Cases {
		for _, source := range corpus.Records {
			if reviewCase.ReviewCaseID == source.CaseID {
				t.Fatalf("source case ID leaked as review case ID: %q", source.CaseID)
			}
		}
		if reviewCase.Task == TaskContentFilter && reviewCase.ContentFilter == nil {
			t.Fatal("contentfilter case omitted frozen input")
		}
		if reviewCase.Task == TaskJunkPurge && reviewCase.JunkPurge == nil {
			t.Fatal("junkpurge case omitted frozen input")
		}
	}
}

func TestReviewAssignmentRejectsSecretsAndSourceMutation(t *testing.T) {
	corpus := reviewFixtureCorpus(t)
	spec := reviewSpec(corpus, "reviewer:a")

	secretSpec := spec
	secretSpec.ReviewerID = "sk-" + "1234567890abcdefghijklmnop"
	if _, err := NewReviewAssignment(corpus, secretSpec); err == nil ||
		!strings.Contains(err.Error(), "secret-like material") {
		t.Fatalf("secret reviewer ID error = %v", err)
	}

	assignment, err := NewReviewAssignment(corpus, spec)
	if err != nil {
		t.Fatalf("NewReviewAssignment: %v", err)
	}

	mutatedSource := corpus
	mutatedSource.Records = append([]CorpusRecord(nil), corpus.Records...)
	for i := range mutatedSource.Records {
		if mutatedSource.Records[i].Task != TaskContentFilter {
			continue
		}
		payload := *mutatedSource.Records[i].ContentFilter
		payload.Input.Title += ".Changed"
		mutatedSource.Records[i].ContentFilter = &payload
		break
	}
	if err := ValidateReviewAssignment(mutatedSource, assignment); err == nil ||
		!strings.Contains(err.Error(), "corpus_sha256") {
		t.Fatalf("mutated source error = %v", err)
	}

	tampered := cloneReviewAssignment(assignment)
	tampered.Cases[0].InputSHA256 = strings.Repeat("0", 64)
	if err := ValidateReviewAssignment(corpus, tampered); err == nil ||
		!strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("tampered input hash error = %v", err)
	}
	tamperedProof := cloneReviewAssignment(assignment)
	tamperedProof.ReviewProof.PolicySHA256 = strings.Repeat("f", 64)
	if err := ValidateReviewAssignment(corpus, tamperedProof); err == nil ||
		!strings.Contains(err.Error(), "assignment_id") {
		t.Fatalf("tampered policy proof error = %v", err)
	}

	secretInput := cloneReviewAssignment(assignment)
	if secretInput.Cases[0].ContentFilter != nil {
		secretInput.Cases[0].ContentFilter.Title = "release sk-1234567890abcdefghijklmnop"
	} else {
		secretInput.Cases[0].JunkPurge.TorrentName =
			"release sk-1234567890abcdefghijklmnop"
	}
	if err := ValidateReviewAssignment(corpus, secretInput); err == nil ||
		!strings.Contains(err.Error(), "secret-like material") {
		t.Fatalf("secret input error = %v", err)
	}
}

func TestReviewSubmissionRequiresExactStrictCoverage(t *testing.T) {
	corpus := reviewFixtureCorpus(t)
	assignment := mustReviewAssignment(t, corpus, "reviewer:a")
	labels := concreteReviewLabels(corpus)
	responses := reviewResponses(t, corpus, assignment, labels)

	submission, err := NewReviewSubmission(corpus, assignment, responses)
	if err != nil {
		t.Fatalf("NewReviewSubmission: %v", err)
	}
	if len(submission.Responses) != len(assignment.Cases) {
		t.Fatalf(
			"submission response count = %d, want %d",
			len(submission.Responses),
			len(assignment.Cases),
		)
	}

	if _, err := NewReviewSubmission(corpus, assignment, responses[:len(responses)-1]); err == nil ||
		!strings.Contains(err.Error(), "rows") {
		t.Fatalf("missing response error = %v", err)
	}

	duplicate := append([]ReviewResponseRecord(nil), responses...)
	duplicate[len(duplicate)-1] = duplicate[0]
	if _, err := NewReviewSubmission(corpus, assignment, duplicate); err == nil ||
		!strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate response error = %v", err)
	}

	wrongReviewer := append([]ReviewResponseRecord(nil), responses...)
	wrongReviewer[0].ReviewerID = "reviewer:b"
	if _, err := NewReviewSubmission(corpus, assignment, wrongReviewer); err == nil ||
		!strings.Contains(err.Error(), "reviewer_id") {
		t.Fatalf("wrong reviewer error = %v", err)
	}

	wrongProof := append([]ReviewResponseRecord(nil), responses...)
	wrongProof[0].ReviewProof.PolicySHA256 = strings.Repeat("f", 64)
	if _, err := NewReviewSubmission(corpus, assignment, wrongProof); err == nil ||
		!strings.Contains(err.Error(), "review_proof") {
		t.Fatalf("wrong response proof error = %v", err)
	}

	wrongTask := append([]ReviewResponseRecord(nil), responses...)
	wrongTask[0].Task = TaskMatcherExtract
	if _, err := NewReviewSubmission(corpus, assignment, wrongTask); err == nil ||
		!strings.Contains(err.Error(), "not human-reviewable") {
		t.Fatalf("wrong task error = %v", err)
	}

	wrongLabel := append([]ReviewResponseRecord(nil), responses...)
	wrongLabel[0].Label = ReviewLabel(JunkClassDegenerate)
	if assignment.Cases[0].Task == TaskJunkPurge {
		wrongLabel[0].Label = ReviewLabelEnglish
	}
	if _, err := NewReviewSubmission(corpus, assignment, wrongLabel); err == nil ||
		!strings.Contains(err.Error(), "invalid") {
		t.Fatalf("wrong label error = %v", err)
	}
}

func TestAgreementAdjudicationAndGoldPromotion(t *testing.T) {
	corpus := reviewFixtureCorpus(t)
	assignmentA := mustReviewAssignment(t, corpus, "reviewer:a")
	assignmentB := mustReviewAssignment(t, corpus, "reviewer:b")

	labelsA := map[string]ReviewLabel{
		"source:content-a": ReviewLabelEnglish,
		"source:content-b": ReviewLabelEnglish,
		"source:content-c": ReviewLabelAmbiguous,
		"source:junk-a":    ReviewLabel(JunkClassDegenerate),
		"source:junk-b":    ReviewLabel(JunkClassMovie),
		"source:junk-c":    ReviewLabel(JunkClassTVShow),
	}
	labelsB := map[string]ReviewLabel{
		"source:content-a": ReviewLabelEnglish,
		"source:content-b": ReviewLabelNonEnglish,
		"source:content-c": ReviewLabelAmbiguous,
		"source:junk-a":    ReviewLabel(JunkClassDegenerate),
		"source:junk-b":    ReviewLabel(JunkClassTVShow),
		"source:junk-c":    ReviewLabel(JunkClassTVShow),
	}
	submissionA := mustReviewSubmission(t, corpus, assignmentA, labelsA)
	submissionB := mustReviewSubmission(t, corpus, assignmentB, labelsB)

	report, err := ComparePrimaryReviews(corpus, submissionA, submissionB)
	if err != nil {
		t.Fatalf("ComparePrimaryReviews: %v", err)
	}
	if report.Overall.Cases != 6 ||
		report.Overall.Agreements != 4 ||
		report.Overall.Disagreements != 2 ||
		report.Overall.NeedsAdjudication != 3 {
		t.Fatalf("overall agreement = %#v", report.Overall)
	}
	if math.Abs(report.Overall.ObservedAgreement-(4.0/6.0)) > 1e-12 {
		t.Fatalf(
			"observed agreement = %v, want %v",
			report.Overall.ObservedAgreement,
			4.0/6.0,
		)
	}
	if report.Overall.CohensKappa == nil {
		t.Fatal("overall Cohen's kappa is unexpectedly undefined")
	}
	if len(report.ByTask) != 2 || len(report.Confusion) != 6 {
		t.Fatalf(
			"by-task/confusion sizes = %d/%d, want 2/6",
			len(report.ByTask),
			len(report.Confusion),
		)
	}
	wantAdjudication := []string{
		"source:content-b",
		"source:content-c",
		"source:junk-b",
	}
	if !reflect.DeepEqual(report.NeedsAdjudicationCaseIDs, wantAdjudication) {
		t.Fatalf(
			"adjudication cases = %v, want %v",
			report.NeedsAdjudicationCaseIDs,
			wantAdjudication,
		)
	}

	if _, err := PromoteGoldLabels(corpus, submissionA, submissionB, nil); err == nil ||
		!strings.Contains(err.Error(), "require independent adjudication") {
		t.Fatalf("missing adjudication error = %v", err)
	}
	if _, _, err := NewAdjudicationAssignment(
		corpus,
		"reviewer:a",
		submissionA,
		submissionB,
	); err == nil || !strings.Contains(err.Error(), "independent") {
		t.Fatalf("non-independent adjudicator error = %v", err)
	}

	adjudicationAssignment, adjudicationReport, err := NewAdjudicationAssignment(
		corpus,
		"reviewer:c",
		submissionA,
		submissionB,
	)
	if err != nil {
		t.Fatalf("NewAdjudicationAssignment: %v", err)
	}
	if !reflect.DeepEqual(report, adjudicationReport) {
		t.Fatal("adjudication report differs from primary comparison")
	}
	if adjudicationAssignment.Kind != ReviewAssignmentAdjudication ||
		len(adjudicationAssignment.Cases) != 3 {
		t.Fatalf("adjudication assignment = %#v", adjudicationAssignment)
	}

	adjudicationLabels := map[string]ReviewLabel{
		"source:content-b": ReviewLabelNonEnglish,
		"source:content-c": ReviewLabelAmbiguous,
		"source:junk-b":    ReviewLabel(JunkClassMovie),
	}
	adjudication := mustReviewSubmission(
		t,
		corpus,
		adjudicationAssignment,
		adjudicationLabels,
	)
	promotion, err := PromoteGoldLabels(
		corpus,
		submissionA,
		submissionB,
		&adjudication,
	)
	if err != nil {
		t.Fatalf("PromoteGoldLabels: %v", err)
	}
	if promotion.GoldCorpus == nil {
		t.Fatal("gold corpus is nil")
	}
	if got := len(promotion.GoldCorpus.Records); got != 5 {
		t.Fatalf("gold record count = %d, want 5", got)
	}
	if promotion.AdjudicatedCases != 3 {
		t.Fatalf("adjudicated count = %d, want 3", promotion.AdjudicatedCases)
	}
	if want := []string{"source:content-c"}; !reflect.DeepEqual(
		promotion.UnresolvedCaseIDs,
		want,
	) {
		t.Fatalf(
			"unresolved cases = %v, want %v",
			promotion.UnresolvedCaseIDs,
			want,
		)
	}

	sourceByID := corpusRecordsByID(corpus)
	for _, record := range promotion.GoldCorpus.Records {
		if record.Label.Provenance != LabelProvenanceHumanReview ||
			record.Label.Strength != LabelStrengthGold ||
			record.Label.SourceRef != "review:2026-07" ||
			record.Label.PolicyVersion != "human-policy-v1" {
			t.Fatalf("case %q gold label metadata = %#v", record.CaseID, record.Label)
		}
		wantReviewers := 2
		if record.CaseID == "source:content-b" || record.CaseID == "source:junk-b" {
			wantReviewers = 3
		}
		if record.Label.ReviewerCount != wantReviewers {
			t.Fatalf(
				"case %q reviewer_count = %d, want %d",
				record.CaseID,
				record.Label.ReviewerCount,
				wantReviewers,
			)
		}

		original := sourceByID[record.CaseID]
		switch record.Task {
		case TaskContentFilter:
			if record.ContentFilter.Input != original.ContentFilter.Input {
				t.Fatalf("case %q contentfilter input changed", record.CaseID)
			}
		case TaskJunkPurge:
			if record.JunkPurge.Input != original.JunkPurge.Input {
				t.Fatalf("case %q junkpurge input changed", record.CaseID)
			}
		}
	}

	goldByID := corpusRecordsByID(*promotion.GoldCorpus)
	if got := goldByID["source:content-b"].ContentFilter.Expected.Language; got != LanguageNonEnglish {
		t.Fatalf("adjudicated content label = %q, want non_english", got)
	}
	junkB := goldByID["source:junk-b"].JunkPurge.Expected
	if junkB.ContentClass != JunkClassMovie {
		t.Fatalf("adjudicated junk class = %q, want movie", junkB.ContentClass)
	}
	// The adjudication must also have DERIVED the disposition rather than
	// carrying one over from the pre-review fixture, which was degenerate/delete.
	if junkB.Disposition != JunkDispositionKeep {
		t.Fatalf(
			"adjudicated junk disposition = %q, want keep derived from movie",
			junkB.Disposition,
		)
	}
}

func TestConcreteUncertainAndUnsurePromoteAsAbstentionGold(t *testing.T) {
	corpus := reviewFixtureCorpus(t)
	assignmentA := mustReviewAssignment(t, corpus, "reviewer:a")
	assignmentB := mustReviewAssignment(t, corpus, "reviewer:b")
	labels := concreteReviewLabels(corpus)
	labels["source:content-a"] = ReviewLabelUncertain
	labels["source:junk-a"] = ReviewLabel(JunkClassUnresolved)
	reviewerA := mustReviewSubmission(t, corpus, assignmentA, labels)
	reviewerB := mustReviewSubmission(t, corpus, assignmentB, labels)

	promotion, err := PromoteGoldLabels(
		corpus,
		reviewerA,
		reviewerB,
		nil,
	)
	if err != nil {
		t.Fatalf("PromoteGoldLabels: %v", err)
	}
	if promotion.GoldCorpus == nil {
		t.Fatal("concrete abstention labels unexpectedly produced no gold corpus")
	}
	records := corpusRecordsByID(*promotion.GoldCorpus)
	content := records["source:content-a"]
	if !content.ContentFilter.Expected.AllowAbstain ||
		content.ContentFilter.Expected.Language != "" {
		t.Fatalf("uncertain content gold = %+v", content.ContentFilter.Expected)
	}
	junk := records["source:junk-a"]
	if junk.JunkPurge.Expected.ContentClass != JunkClassUnresolved ||
		junk.JunkPurge.Expected.Disposition != JunkDispositionAbstain {
		t.Fatalf("unresolved junk gold = %+v", junk.JunkPurge.Expected)
	}
	sourceRecords := corpusRecordsByID(corpus)
	for _, record := range []CorpusRecord{content, junk} {
		if record.Label.HumanReviewProof == nil ||
			*record.Label.HumanReviewProof != assignmentA.ReviewProof {
			t.Fatalf("promoted review proof = %+v", record.Label.HumanReviewProof)
		}
		if !reflect.DeepEqual(
			record.PrivacyAttestation,
			sourceRecords[record.CaseID].PrivacyAttestation,
		) {
			t.Fatalf(
				"promoted record %q changed privacy attestation: got %+v want %+v",
				record.CaseID,
				record.PrivacyAttestation,
				sourceRecords[record.CaseID].PrivacyAttestation,
			)
		}
	}
}

func TestAgreementKappaKnownAndUndefinedCases(t *testing.T) {
	pairs := []reviewPair{
		{task: TaskContentFilter, a: ReviewLabelEnglish, b: ReviewLabelEnglish},
		{task: TaskContentFilter, a: ReviewLabelEnglish, b: ReviewLabelNonEnglish},
		{task: TaskContentFilter, a: ReviewLabelNonEnglish, b: ReviewLabelNonEnglish},
		{task: TaskContentFilter, a: ReviewLabelNonEnglish, b: ReviewLabelEnglish},
	}
	stats := calculateAgreementStats(pairs, false)
	if stats.CohensKappa == nil || math.Abs(*stats.CohensKappa) > 1e-12 {
		t.Fatalf("Cohen's kappa = %v, want 0", stats.CohensKappa)
	}

	undefined := calculateAgreementStats([]reviewPair{
		{task: TaskJunkPurge, a: ReviewLabel(JunkClassDegenerate), b: ReviewLabel(JunkClassDegenerate)},
		{task: TaskJunkPurge, a: ReviewLabel(JunkClassDegenerate), b: ReviewLabel(JunkClassDegenerate)},
	}, false)
	if undefined.CohensKappa != nil {
		t.Fatalf("undefined Cohen's kappa = %v, want nil", *undefined.CohensKappa)
	}
}

func reviewFixtureCorpus(t *testing.T) Corpus {
	t.Helper()
	records := []CorpusRecord{
		testContentRecord("source:content-a", LanguageNonEnglish),
		testContentRecord("source:content-b", LanguageEnglish),
		testContentRecord("source:content-c", LanguageNonEnglish),
		testJunkRecord("source:junk-a", JunkClassTVShow),
		testJunkRecord("source:junk-b", JunkClassDegenerate),
		testJunkRecord("source:junk-c", JunkClassMovie),
	}
	contentTitles := []string{
		"North Ridge S01E01 1080p",
		"La Casa del Lago 2024",
		"Roma International Cut",
	}
	junkTitles := []string{
		"Film Night 2023 1080p",
		"zz malformed upload bucket",
		"Missing Name S02 Complete",
	}
	contentIndex := 0
	junkIndex := 0
	for i := range records {
		records[i].Label = LabelMetadata{
			Provenance:    LabelProvenanceProductionTeacher,
			Strength:      LabelStrengthTeacher,
			SourceRef:     "teacher:legacy",
			PolicyVersion: "teacher-policy-v0",
		}
		if records[i].Task == TaskContentFilter {
			records[i].ContentFilter.Input.Title = contentTitles[contentIndex]
			contentIndex++
		} else {
			records[i].JunkPurge.Input.TorrentName = junkTitles[junkIndex]
			junkIndex++
		}
	}

	return mustCorpus(t, records)
}

func reviewSpec(corpus Corpus, reviewerID string) ReviewAssignmentSpec {
	proof := testReviewProof(
		corpus,
		ReviewWorkflowContentJunk,
		"human-policy-v1",
	)
	return ReviewAssignmentSpec{
		ReviewSetID:   "review:2026-07",
		ReviewerID:    reviewerID,
		PolicyVersion: "human-policy-v1",
		ReviewProof:   proof,
		Tasks:         []Task{TaskContentFilter, TaskJunkPurge},
	}
}

func mustReviewAssignment(t *testing.T, corpus Corpus, reviewerID string) ReviewAssignment {
	t.Helper()
	assignment, err := NewReviewAssignment(corpus, reviewSpec(corpus, reviewerID))
	if err != nil {
		t.Fatalf("NewReviewAssignment(%s): %v", reviewerID, err)
	}

	return assignment
}

func mustReviewSubmission(
	t *testing.T,
	corpus Corpus,
	assignment ReviewAssignment,
	labels map[string]ReviewLabel,
) ReviewSubmission {
	t.Helper()
	submission, err := NewReviewSubmission(
		corpus,
		assignment,
		reviewResponses(t, corpus, assignment, labels),
	)
	if err != nil {
		t.Fatalf("NewReviewSubmission(%s): %v", assignment.ReviewerID, err)
	}

	return submission
}

func reviewResponses(
	t *testing.T,
	corpus Corpus,
	assignment ReviewAssignment,
	labels map[string]ReviewLabel,
) []ReviewResponseRecord {
	t.Helper()
	caseIDByReviewID := make(map[string]string, len(corpus.Records))
	taskByCaseID := make(map[string]Task, len(corpus.Records))
	for _, record := range corpus.Records {
		caseIDByReviewID[opaqueReviewCaseID(assignment.AssignmentID, record.CaseID)] =
			record.CaseID
		taskByCaseID[record.CaseID] = record.Task
	}

	responses := make([]ReviewResponseRecord, 0, len(assignment.Cases))
	for _, reviewCase := range assignment.Cases {
		sourceCaseID, exists := caseIDByReviewID[reviewCase.ReviewCaseID]
		if !exists {
			t.Fatalf("review case alias %q not found in source", reviewCase.ReviewCaseID)
		}
		label, exists := labels[sourceCaseID]
		if !exists {
			t.Fatalf("label for source case %q is missing", sourceCaseID)
		}
		responses = append(responses, ReviewResponseRecord{
			SchemaVersion: SchemaVersion,
			AssignmentID:  assignment.AssignmentID,
			ReviewerID:    assignment.ReviewerID,
			ReviewProof:   assignment.ReviewProof,
			ReviewCaseID:  reviewCase.ReviewCaseID,
			Task:          taskByCaseID[sourceCaseID],
			Label:         label,
		})
	}

	return responses
}

func sourceOrderForAssignment(
	t *testing.T,
	corpus Corpus,
	assignment ReviewAssignment,
) []string {
	t.Helper()
	caseIDByReviewID := make(map[string]string, len(corpus.Records))
	for _, record := range corpus.Records {
		caseIDByReviewID[opaqueReviewCaseID(assignment.AssignmentID, record.CaseID)] =
			record.CaseID
	}

	order := make([]string, 0, len(assignment.Cases))
	for _, reviewCase := range assignment.Cases {
		caseID, exists := caseIDByReviewID[reviewCase.ReviewCaseID]
		if !exists {
			t.Fatalf("review case alias %q not found in source", reviewCase.ReviewCaseID)
		}
		order = append(order, caseID)
	}

	return order
}

func concreteReviewLabels(corpus Corpus) map[string]ReviewLabel {
	labels := make(map[string]ReviewLabel, len(corpus.Records))
	for _, record := range corpus.Records {
		if record.Task == TaskContentFilter {
			labels[record.CaseID] = ReviewLabelEnglish
		} else {
			labels[record.CaseID] = ReviewLabel(JunkClassTVShow)
		}
	}

	return labels
}

func corpusRecordsByID(corpus Corpus) map[string]CorpusRecord {
	records := make(map[string]CorpusRecord, len(corpus.Records))
	for _, record := range corpus.Records {
		records[record.CaseID] = record
	}

	return records
}

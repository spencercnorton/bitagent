package llmeval

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestMatcherReviewAssignmentsAreBlindedDeterministicAndReviewerSpecific(t *testing.T) {
	corpus := matcherReviewFixtureCorpus(t)
	first := mustMatcherReviewAssignment(t, corpus, "reviewer:a")
	repeated := mustMatcherReviewAssignment(t, corpus, "reviewer:a")
	other := mustMatcherReviewAssignment(t, corpus, "reviewer:b")

	if !reflect.DeepEqual(first, repeated) {
		t.Fatal("identical matcher assignment is not deterministic")
	}
	if first.AssignmentID == other.AssignmentID {
		t.Fatal("different matcher reviewers received the same assignment ID")
	}
	if got, want := first.Tasks, []Task{TaskMatcherExtract, TaskMatcherRerank}; !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical tasks = %v, want %v", got, want)
	}
	for _, reviewCase := range first.Cases {
		for _, source := range corpus.Records {
			if reviewCase.ReviewCaseID == source.CaseID {
				t.Fatalf("source case ID leaked as review case ID: %q", source.CaseID)
			}
		}
		switch reviewCase.Task {
		case TaskMatcherExtract:
			if reviewCase.MatcherExtract == nil || reviewCase.MatcherRerank != nil {
				t.Fatalf("extract review payload = %#v", reviewCase)
			}
		case TaskMatcherRerank:
			if reviewCase.MatcherRerank == nil || reviewCase.MatcherExtract != nil {
				t.Fatalf("rerank review payload = %#v", reviewCase)
			}
		}
	}

	var raw bytes.Buffer
	if err := WriteMatcherReviewAssignment(&raw, corpus, first); err != nil {
		t.Fatalf("WriteMatcherReviewAssignment: %v", err)
	}
	for _, forbidden := range []string{
		`"expected"`,
		`"label"`,
		`extract:a`,
		`extract:b`,
		`rerank:a`,
		`rerank:b`,
		`HIDDEN GOLD TITLE`,
	} {
		if strings.Contains(raw.String(), forbidden) {
			t.Fatalf("matcher assignment leaked %q:\n%s", forbidden, raw.String())
		}
	}
	roundTrip, err := ReadMatcherReviewAssignment(bytes.NewReader(raw.Bytes()), corpus)
	if err != nil {
		t.Fatalf("ReadMatcherReviewAssignment: %v", err)
	}
	if !reflect.DeepEqual(roundTrip, first) {
		t.Fatal("matcher assignment JSONL round trip changed the packet")
	}
}

func TestMatcherReviewSubmissionIsStrictAndCanonical(t *testing.T) {
	corpus := matcherReviewFixtureCorpus(t)
	assignment := mustMatcherReviewAssignment(t, corpus, "reviewer:a")
	decisions := concreteMatcherReviewDecisions()
	responses := matcherReviewResponses(t, corpus, assignment, decisions)

	for i := range responses {
		if responses[i].MatcherRerank != nil {
			responses[i].MatcherRerank.AcceptableTMDBIDs = []int64{202, 101}
		}
	}
	submission, err := NewMatcherReviewSubmission(corpus, assignment, responses)
	if err != nil {
		t.Fatalf("NewMatcherReviewSubmission: %v", err)
	}
	for _, response := range submission.Responses {
		if response.MatcherRerank != nil &&
			!reflect.DeepEqual(response.MatcherRerank.AcceptableTMDBIDs, []int64{101, 202}) {
			t.Fatalf("rerank IDs were not canonicalized: %#v", response.MatcherRerank)
		}
	}

	if _, err := NewMatcherReviewSubmission(
		corpus,
		assignment,
		responses[:len(responses)-1],
	); err == nil || !strings.Contains(err.Error(), "rows") {
		t.Fatalf("incomplete submission error = %v", err)
	}

	wrongProof := append([]MatcherReviewResponseRecord(nil), responses...)
	wrongProof[0].ReviewProof.PolicySHA256 = strings.Repeat("f", 64)
	if _, err := NewMatcherReviewSubmission(
		corpus,
		assignment,
		wrongProof,
	); err == nil || !strings.Contains(err.Error(), "review_proof") {
		t.Fatalf("wrong matcher response proof error = %v", err)
	}

	badCandidate := append([]MatcherReviewResponseRecord(nil), responses...)
	for i := range badCandidate {
		if badCandidate[i].MatcherRerank != nil {
			copy := *badCandidate[i].MatcherRerank
			copy.AcceptableTMDBIDs = []int64{999}
			badCandidate[i].MatcherRerank = &copy
			break
		}
	}
	if _, err := NewMatcherReviewSubmission(
		corpus,
		assignment,
		badCandidate,
	); err == nil || !strings.Contains(err.Error(), "not in candidates") {
		t.Fatalf("unknown candidate error = %v", err)
	}

	contradictory := append([]MatcherReviewResponseRecord(nil), responses...)
	for i := range contradictory {
		if contradictory[i].MatcherExtract != nil {
			copy := *contradictory[i].MatcherExtract
			copy.Ambiguous = true
			contradictory[i].MatcherExtract = &copy
			break
		}
	}
	if _, err := NewMatcherReviewSubmission(
		corpus,
		assignment,
		contradictory,
	); err == nil || !strings.Contains(err.Error(), "ambiguous cannot include") {
		t.Fatalf("contradictory decision error = %v", err)
	}

	var raw bytes.Buffer
	if err := WriteMatcherReviewSubmission(&raw, corpus, submission); err != nil {
		t.Fatalf("WriteMatcherReviewSubmission: %v", err)
	}
	unknown := strings.Replace(
		raw.String(),
		`"assignment_id"`,
		`"unexpected":true,"assignment_id"`,
		1,
	)
	if _, err := ReadMatcherReviewSubmission(
		strings.NewReader(unknown),
		corpus,
		assignment,
	); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown response field error = %v", err)
	}
}

func TestMatcherAgreementAdjudicationAndGoldPromotion(t *testing.T) {
	corpus := matcherReviewFixtureCorpus(t)
	assignmentA := mustMatcherReviewAssignment(t, corpus, "reviewer:a")
	assignmentB := mustMatcherReviewAssignment(t, corpus, "reviewer:b")

	extraction := testExtraction("Reviewed Movie")
	primaryA := map[string]MatcherReviewDecision{
		"extract:a": extractMatcherDecision(extraction),
		"extract:b": extractMatcherDecision(extraction),
		"rerank:a":  rerankMatcherDecision([]int64{202, 101}, false),
		"rerank:b":  rerankMatcherDecision([]int64{101}, false),
	}
	primaryB := map[string]MatcherReviewDecision{
		"extract:a": extractMatcherDecision(extraction),
		"extract:b": ambiguousExtractMatcherDecision(),
		"rerank:a":  rerankMatcherDecision([]int64{101, 202}, false),
		"rerank:b":  rerankMatcherDecision([]int64{202}, false),
	}
	submissionA := mustMatcherReviewSubmission(t, corpus, assignmentA, primaryA)
	submissionB := mustMatcherReviewSubmission(t, corpus, assignmentB, primaryB)

	report, err := ComparePrimaryMatcherReviews(corpus, submissionA, submissionB)
	if err != nil {
		t.Fatalf("ComparePrimaryMatcherReviews: %v", err)
	}
	if report.Overall.Cases != 4 ||
		report.Overall.Agreements != 2 ||
		report.Overall.Disagreements != 2 ||
		report.Overall.NeedsAdjudication != 2 {
		t.Fatalf("overall agreement = %#v", report.Overall)
	}
	if !reflect.DeepEqual(
		report.NeedsAdjudicationCaseIDs,
		[]string{"extract:b", "rerank:b"},
	) {
		t.Fatalf(
			"adjudication cases = %v",
			report.NeedsAdjudicationCaseIDs,
		)
	}
	if _, err := PromoteMatcherGold(
		corpus,
		submissionA,
		submissionB,
		nil,
	); err == nil || !strings.Contains(err.Error(), "require independent adjudication") {
		t.Fatalf("missing adjudication error = %v", err)
	}
	if _, _, err := NewMatcherAdjudicationAssignment(
		corpus,
		"reviewer:a",
		submissionA,
		submissionB,
	); err == nil || !strings.Contains(err.Error(), "independent") {
		t.Fatalf("non-independent adjudicator error = %v", err)
	}

	assignmentC, repeatedReport, err := NewMatcherAdjudicationAssignment(
		corpus,
		"reviewer:c",
		submissionA,
		submissionB,
	)
	if err != nil {
		t.Fatalf("NewMatcherAdjudicationAssignment: %v", err)
	}
	if !reflect.DeepEqual(report, repeatedReport) {
		t.Fatal("adjudication report changed")
	}
	if assignmentC.Kind != ReviewAssignmentAdjudication || len(assignmentC.Cases) != 2 {
		t.Fatalf("adjudication assignment = %#v", assignmentC)
	}
	adjudication := mustMatcherReviewSubmission(
		t,
		corpus,
		assignmentC,
		map[string]MatcherReviewDecision{
			"extract:b": rerouteExtractToAbstain(),
			"rerank:b":  ambiguousRerankMatcherDecision(),
		},
	)
	promotion, err := PromoteMatcherGold(
		corpus,
		submissionA,
		submissionB,
		&adjudication,
	)
	if err != nil {
		t.Fatalf("PromoteMatcherGold: %v", err)
	}
	if promotion.GoldCorpus == nil || len(promotion.GoldCorpus.Records) != 3 {
		t.Fatalf("gold corpus = %#v", promotion.GoldCorpus)
	}
	if promotion.AdjudicatedCases != 2 ||
		!reflect.DeepEqual(promotion.UnresolvedCaseIDs, []string{"rerank:b"}) {
		t.Fatalf("promotion = %#v", promotion)
	}
	records := corpusRecordsByID(*promotion.GoldCorpus)
	if !records["extract:b"].MatcherExtract.Expected.AllowAbstain ||
		len(records["extract:b"].MatcherExtract.Expected.Acceptable) != 0 {
		t.Fatalf(
			"adjudicated extraction expected = %#v",
			records["extract:b"].MatcherExtract.Expected,
		)
	}
	if !reflect.DeepEqual(
		records["rerank:a"].MatcherRerank.Expected.AcceptableTMDBIDs,
		[]int64{101, 202},
	) {
		t.Fatalf(
			"rerank acceptable set = %v",
			records["rerank:a"].MatcherRerank.Expected.AcceptableTMDBIDs,
		)
	}
	sourceRecords := corpusRecordsByID(corpus)
	for _, record := range promotion.GoldCorpus.Records {
		if !reflect.DeepEqual(
			record.PrivacyAttestation,
			sourceRecords[record.CaseID].PrivacyAttestation,
		) {
			t.Fatalf(
				"matcher promotion changed privacy attestation for %q: got %+v want %+v",
				record.CaseID,
				record.PrivacyAttestation,
				sourceRecords[record.CaseID].PrivacyAttestation,
			)
		}
		if record.Label.HumanReviewProof == nil ||
			*record.Label.HumanReviewProof != assignmentA.ReviewProof {
			t.Fatalf("matcher promotion proof = %+v", record.Label.HumanReviewProof)
		}
	}
	for id, record := range records {
		wantReviewers := 2
		if id == "extract:b" {
			wantReviewers = 3
		}
		if record.Label.Provenance != LabelProvenanceHumanReview ||
			record.Label.Strength != LabelStrengthGold ||
			record.Label.SourceRef != "review:matcher-fixture" ||
			record.Label.PolicyVersion != "matcher-policy-v1" ||
			record.Label.ReviewerCount != wantReviewers {
			t.Fatalf("gold metadata for %s = %#v", id, record.Label)
		}
	}
}

func matcherReviewFixtureCorpus(t *testing.T) Corpus {
	t.Helper()
	hidden := testExtraction("HIDDEN GOLD TITLE")
	records := []CorpusRecord{
		testExtractRecord("extract:a", MatcherExtractExpected{
			Acceptable: []MatcherExtraction{hidden},
		}),
		testExtractRecord("extract:b", MatcherExtractExpected{AllowAbstain: true}),
		testRerankRecord("rerank:a", MatcherRerankExpected{
			AcceptableTMDBIDs: []int64{101},
		}),
		testRerankRecord("rerank:b", MatcherRerankExpected{
			AcceptableTMDBIDs: []int64{202},
		}),
	}
	for i := range records {
		records[i].Label = LabelMetadata{
			Provenance:    LabelProvenanceProductionTeacher,
			Strength:      LabelStrengthTeacher,
			SourceRef:     "teacher:legacy",
			PolicyVersion: "teacher-policy-v0",
		}
		if records[i].MatcherExtract != nil {
			records[i].MatcherExtract.Input.ReleaseName += fmtIndex(i)
		}
	}
	return mustCorpus(t, records)
}

func matcherReviewSpec(
	corpus Corpus,
	reviewerID string,
) MatcherReviewAssignmentSpec {
	proof := testReviewProof(
		corpus,
		ReviewWorkflowMatcher,
		"matcher-policy-v1",
	)
	return MatcherReviewAssignmentSpec{
		ReviewSetID:   "review:matcher-fixture",
		ReviewerID:    reviewerID,
		PolicyVersion: "matcher-policy-v1",
		ReviewProof:   proof,
		Tasks:         []Task{TaskMatcherRerank, TaskMatcherExtract},
	}
}

func mustMatcherReviewAssignment(
	t *testing.T,
	corpus Corpus,
	reviewerID string,
) MatcherReviewAssignment {
	t.Helper()
	assignment, err := NewMatcherReviewAssignment(
		corpus,
		matcherReviewSpec(corpus, reviewerID),
	)
	if err != nil {
		t.Fatalf("NewMatcherReviewAssignment(%s): %v", reviewerID, err)
	}
	return assignment
}

func mustMatcherReviewSubmission(
	t *testing.T,
	corpus Corpus,
	assignment MatcherReviewAssignment,
	decisions map[string]MatcherReviewDecision,
) MatcherReviewSubmission {
	t.Helper()
	submission, err := NewMatcherReviewSubmission(
		corpus,
		assignment,
		matcherReviewResponses(t, corpus, assignment, decisions),
	)
	if err != nil {
		t.Fatalf("NewMatcherReviewSubmission(%s): %v", assignment.ReviewerID, err)
	}
	return submission
}

func matcherReviewResponses(
	t *testing.T,
	corpus Corpus,
	assignment MatcherReviewAssignment,
	decisions map[string]MatcherReviewDecision,
) []MatcherReviewResponseRecord {
	t.Helper()
	sourceByReviewID := make(map[string]CorpusRecord, len(corpus.Records))
	for _, record := range corpus.Records {
		sourceByReviewID[opaqueMatcherReviewCaseID(
			assignment.AssignmentID,
			record.CaseID,
		)] = record
	}
	responses := make([]MatcherReviewResponseRecord, 0, len(assignment.Cases))
	for _, reviewCase := range assignment.Cases {
		record, exists := sourceByReviewID[reviewCase.ReviewCaseID]
		if !exists {
			t.Fatalf("review case alias %q not found", reviewCase.ReviewCaseID)
		}
		decision, exists := decisions[record.CaseID]
		if !exists {
			t.Fatalf("decision for source case %q is missing", record.CaseID)
		}
		responses = append(responses, MatcherReviewResponseRecord{
			SchemaVersion:  SchemaVersion,
			AssignmentID:   assignment.AssignmentID,
			ReviewerID:     assignment.ReviewerID,
			ReviewProof:    assignment.ReviewProof,
			ReviewCaseID:   reviewCase.ReviewCaseID,
			Task:           record.Task,
			MatcherExtract: decision.MatcherExtract,
			MatcherRerank:  decision.MatcherRerank,
		})
	}
	return responses
}

func concreteMatcherReviewDecisions() map[string]MatcherReviewDecision {
	extractionA := testExtraction("Reviewed Movie")
	extractionB := testExtraction("Reviewed Film")
	return map[string]MatcherReviewDecision{
		"extract:a": {
			MatcherExtract: &MatcherExtractReviewDecision{
				Acceptable: []MatcherExtraction{extractionB, extractionA},
			},
		},
		"extract:b": rerouteExtractToAbstain(),
		"rerank:a":  rerankMatcherDecision([]int64{101, 202}, false),
		"rerank:b":  rerankMatcherDecision([]int64{101, 202}, false),
	}
}

func extractMatcherDecision(extraction MatcherExtraction) MatcherReviewDecision {
	return MatcherReviewDecision{
		MatcherExtract: &MatcherExtractReviewDecision{
			Acceptable: []MatcherExtraction{extraction},
		},
	}
}

func rerouteExtractToAbstain() MatcherReviewDecision {
	return MatcherReviewDecision{
		MatcherExtract: &MatcherExtractReviewDecision{AllowAbstain: true},
	}
}

func ambiguousExtractMatcherDecision() MatcherReviewDecision {
	return MatcherReviewDecision{
		MatcherExtract: &MatcherExtractReviewDecision{Ambiguous: true},
	}
}

func rerankMatcherDecision(ids []int64, allowAbstain bool) MatcherReviewDecision {
	return MatcherReviewDecision{
		MatcherRerank: &MatcherRerankReviewDecision{
			AcceptableTMDBIDs: append([]int64(nil), ids...),
			AllowAbstain:      allowAbstain,
		},
	}
}

func ambiguousRerankMatcherDecision() MatcherReviewDecision {
	return MatcherReviewDecision{
		MatcherRerank: &MatcherRerankReviewDecision{Ambiguous: true},
	}
}

func fmtIndex(index int) string {
	return string(rune('A' + index))
}

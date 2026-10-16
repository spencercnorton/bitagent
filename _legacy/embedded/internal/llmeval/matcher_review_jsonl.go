package llmeval

import (
	"fmt"
	"io"
)

// WriteMatcherReviewAssignment writes a validated input-only matcher packet.
func WriteMatcherReviewAssignment(
	w io.Writer,
	source Corpus,
	assignment MatcherReviewAssignment,
) error {
	if w == nil {
		return fmt.Errorf("matcher review assignment writer is nil")
	}
	if err := ValidateMatcherReviewAssignment(source, assignment); err != nil {
		return err
	}
	return writeReviewJSONL(w, assignment.Cases, "matcher review assignment")
}

// ReadMatcherReviewAssignment strictly verifies repeated metadata, opaque
// aliases, order, input hashes, and frozen input against source.
func ReadMatcherReviewAssignment(
	r io.Reader,
	source Corpus,
) (MatcherReviewAssignment, error) {
	records, err := readJSONL[MatcherReviewCaseRecord](r, "matcher review assignment")
	if err != nil {
		return MatcherReviewAssignment{}, err
	}
	first := records[0]
	assignment := MatcherReviewAssignment{
		SchemaVersion: first.SchemaVersion,
		AssignmentID:  first.AssignmentID,
		Kind:          first.Kind,
		ReviewSetID:   first.ReviewSetID,
		ReviewerID:    first.ReviewerID,
		PolicyVersion: first.PolicyVersion,
		ReviewProof:   first.ReviewProof,
		CorpusSHA256:  first.CorpusSHA256,
		CaseSetSHA256: first.CaseSetSHA256,
		Tasks:         append([]Task(nil), first.Tasks...),
		Cases:         records,
	}
	if err := ValidateMatcherReviewAssignment(source, assignment); err != nil {
		return MatcherReviewAssignment{}, err
	}
	return assignment, nil
}

// WriteMatcherReviewSubmission writes a complete canonical response.
func WriteMatcherReviewSubmission(
	w io.Writer,
	source Corpus,
	submission MatcherReviewSubmission,
) error {
	if w == nil {
		return fmt.Errorf("matcher review submission writer is nil")
	}
	if err := validateMatcherSubmission(source, submission); err != nil {
		return err
	}
	return writeReviewJSONL(w, submission.Responses, "matcher review submission")
}

// ReadMatcherReviewSubmission strictly reads and validates one complete human
// response to a matcher assignment.
func ReadMatcherReviewSubmission(
	r io.Reader,
	source Corpus,
	assignment MatcherReviewAssignment,
) (MatcherReviewSubmission, error) {
	responses, err := readJSONL[MatcherReviewResponseRecord](
		r,
		"matcher review submission",
	)
	if err != nil {
		return MatcherReviewSubmission{}, err
	}
	return NewMatcherReviewSubmission(source, assignment, responses)
}

package llmeval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// WriteReviewAssignment writes a source-verified, input-only blinded
// assignment as strict JSONL. Validation completes before any output is
// written.
func WriteReviewAssignment(
	w io.Writer,
	source Corpus,
	assignment ReviewAssignment,
) error {
	if w == nil {
		return fmt.Errorf("review assignment writer is nil")
	}
	if err := ValidateReviewAssignment(source, assignment); err != nil {
		return err
	}

	return writeReviewJSONL(w, assignment.Cases, "review assignment")
}

// ReadReviewAssignment strictly reads an assignment and verifies every
// repeated metadata field, opaque case alias, deterministic order, input hash,
// and frozen input against source.
func ReadReviewAssignment(r io.Reader, source Corpus) (ReviewAssignment, error) {
	records, err := readJSONL[ReviewCaseRecord](r, "review assignment")
	if err != nil {
		return ReviewAssignment{}, err
	}

	first := records[0]
	assignment := ReviewAssignment{
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
	if err := ValidateReviewAssignment(source, assignment); err != nil {
		return ReviewAssignment{}, err
	}

	return assignment, nil
}

// WriteReviewSubmission writes a complete validated response in the
// reviewer-specific assignment order.
func WriteReviewSubmission(
	w io.Writer,
	source Corpus,
	submission ReviewSubmission,
) error {
	if w == nil {
		return fmt.Errorf("review submission writer is nil")
	}
	if err := validateSubmission(source, submission); err != nil {
		return err
	}

	return writeReviewJSONL(w, submission.Responses, "review submission")
}

// ReadReviewSubmission strictly reads human labels, rejects unknown or
// duplicate fields, and requires exactly one valid response for every assigned
// case.
func ReadReviewSubmission(
	r io.Reader,
	source Corpus,
	assignment ReviewAssignment,
) (ReviewSubmission, error) {
	responses, err := readJSONL[ReviewResponseRecord](r, "review submission")
	if err != nil {
		return ReviewSubmission{}, err
	}

	return NewReviewSubmission(source, assignment, responses)
}

func writeReviewJSONL[T any](w io.Writer, records []T, artifact string) error {
	var payload bytes.Buffer
	for i := range records {
		line, err := json.Marshal(records[i])
		if err != nil {
			return fmt.Errorf("%s: marshal record %d: %w", artifact, i+1, err)
		}
		payload.Write(line)
		payload.WriteByte('\n')
	}
	if _, err := io.Copy(w, bytes.NewReader(payload.Bytes())); err != nil {
		return fmt.Errorf("%s: write: %w", artifact, err)
	}

	return nil
}

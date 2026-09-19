package llmeval

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestReviewAssignmentJSONLIsInputOnlyAndRoundTrips(t *testing.T) {
	corpus := reviewFixtureCorpus(t)
	assignment := mustReviewAssignment(t, corpus, "reviewer:a")

	var first bytes.Buffer
	if err := WriteReviewAssignment(&first, corpus, assignment); err != nil {
		t.Fatalf("WriteReviewAssignment first: %v", err)
	}
	var second bytes.Buffer
	if err := WriteReviewAssignment(&second, corpus, assignment); err != nil {
		t.Fatalf("WriteReviewAssignment second: %v", err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("review assignment JSONL is not byte deterministic")
	}

	for _, forbidden := range []string{
		`"expected"`,
		`"label"`,
		`"provenance"`,
		`"strength"`,
		`"slice_ids"`,
		`"group_id"`,
		`"model"`,
		`"result"`,
		"source:content-a",
		"source:junk-a",
		"teacher:legacy",
	} {
		if strings.Contains(first.String(), forbidden) {
			t.Fatalf("blinded assignment contains forbidden material %q:\n%s", forbidden, first.String())
		}
	}

	for lineNo, line := range strings.Split(strings.TrimSpace(first.String()), "\n") {
		var object map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &object); err != nil {
			t.Fatalf("line %d JSON: %v", lineNo+1, err)
		}
		payloads := 0
		if _, exists := object["contentfilter"]; exists {
			payloads++
		}
		if _, exists := object["junkpurge"]; exists {
			payloads++
		}
		if payloads != 1 {
			t.Fatalf("line %d input payload count = %d, want 1", lineNo+1, payloads)
		}
	}

	read, err := ReadReviewAssignment(bytes.NewReader(first.Bytes()), corpus)
	if err != nil {
		t.Fatalf("ReadReviewAssignment: %v", err)
	}
	if !reflect.DeepEqual(read, assignment) {
		t.Fatalf("round-trip assignment differs:\n got %#v\nwant %#v", read, assignment)
	}
}

func TestReadReviewAssignmentRejectsLabelsOutputsUnknownFieldsAndTampering(t *testing.T) {
	corpus := reviewFixtureCorpus(t)
	assignment := mustReviewAssignment(t, corpus, "reviewer:a")
	var raw bytes.Buffer
	if err := WriteReviewAssignment(&raw, corpus, assignment); err != nil {
		t.Fatalf("WriteReviewAssignment: %v", err)
	}

	for _, test := range []struct {
		name   string
		mutate func(string) string
		want   string
	}{
		{
			name: "expected label",
			mutate: func(value string) string {
				return strings.Replace(value, `"ordinal":`, `"expected":{"language":"english"},"ordinal":`, 1)
			},
			want: "unknown field",
		},
		{
			name: "model output",
			mutate: func(value string) string {
				return strings.Replace(value, `"ordinal":`, `"model_output":{"label":"junk"},"ordinal":`, 1)
			},
			want: "unknown field",
		},
		{
			name: "duplicate field",
			mutate: func(value string) string {
				return strings.Replace(
					value,
					`"ordinal":1`,
					`"ordinal":1,"ordinal":2`,
					1,
				)
			},
			want: "duplicate object key",
		},
		{
			name: "input changed",
			mutate: func(value string) string {
				return strings.Replace(value, "North Ridge", "Changed Ridge", 1)
			},
			want: "mismatch",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ReadReviewAssignment(
				strings.NewReader(test.mutate(raw.String())),
				corpus,
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ReadReviewAssignment error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestReviewSubmissionJSONLStrictRoundTrip(t *testing.T) {
	corpus := reviewFixtureCorpus(t)
	assignment := mustReviewAssignment(t, corpus, "reviewer:a")
	submission := mustReviewSubmission(
		t,
		corpus,
		assignment,
		concreteReviewLabels(corpus),
	)

	var raw bytes.Buffer
	if err := WriteReviewSubmission(&raw, corpus, submission); err != nil {
		t.Fatalf("WriteReviewSubmission: %v", err)
	}
	read, err := ReadReviewSubmission(bytes.NewReader(raw.Bytes()), corpus, assignment)
	if err != nil {
		t.Fatalf("ReadReviewSubmission: %v", err)
	}
	if !reflect.DeepEqual(read, submission) {
		t.Fatalf("round-trip submission differs:\n got %#v\nwant %#v", read, submission)
	}

	unknown := strings.Replace(
		raw.String(),
		`"label":`,
		`"model_output":"hidden","label":`,
		1,
	)
	if _, err := ReadReviewSubmission(
		strings.NewReader(unknown),
		corpus,
		assignment,
	); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown response field error = %v", err)
	}

	duplicate := strings.Replace(
		raw.String(),
		`"label":"english"`,
		`"label":"english","label":"non_english"`,
		1,
	)
	if duplicate == raw.String() {
		t.Fatal("test fixture did not contain an english response")
	}
	if _, err := ReadReviewSubmission(
		strings.NewReader(duplicate),
		corpus,
		assignment,
	); err == nil || !strings.Contains(err.Error(), "duplicate object key") {
		t.Fatalf("duplicate response field error = %v", err)
	}

	lines := strings.Split(strings.TrimSpace(raw.String()), "\n")
	missing := strings.Join(lines[:len(lines)-1], "\n") + "\n"
	if _, err := ReadReviewSubmission(
		strings.NewReader(missing),
		corpus,
		assignment,
	); err == nil || !strings.Contains(err.Error(), "rows") {
		t.Fatalf("missing response row error = %v", err)
	}
}

func TestReviewJSONLWritersRejectNilAndMutatedArtifacts(t *testing.T) {
	corpus := reviewFixtureCorpus(t)
	assignment := mustReviewAssignment(t, corpus, "reviewer:a")
	if err := WriteReviewAssignment(nil, corpus, assignment); err == nil ||
		!strings.Contains(err.Error(), "nil") {
		t.Fatalf("nil assignment writer error = %v", err)
	}

	mutated := cloneReviewAssignment(assignment)
	mutated.Cases[0], mutated.Cases[1] = mutated.Cases[1], mutated.Cases[0]
	if err := WriteReviewAssignment(&bytes.Buffer{}, corpus, mutated); err == nil ||
		!strings.Contains(err.Error(), "ordinal") {
		t.Fatalf("mutated assignment writer error = %v", err)
	}

	submission := mustReviewSubmission(
		t,
		corpus,
		assignment,
		concreteReviewLabels(corpus),
	)
	if err := WriteReviewSubmission(nil, corpus, submission); err == nil ||
		!strings.Contains(err.Error(), "nil") {
		t.Fatalf("nil submission writer error = %v", err)
	}

	submission.Responses[0], submission.Responses[1] =
		submission.Responses[1], submission.Responses[0]
	if err := WriteReviewSubmission(&bytes.Buffer{}, corpus, submission); err == nil ||
		!strings.Contains(err.Error(), "non-canonical") {
		t.Fatalf("mutated submission writer error = %v", err)
	}
}

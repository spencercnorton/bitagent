package llmeval

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

const (
	maxJSONLLineBytes     = 16 << 20
	maxJSONLArtifactBytes = 1 << 30
	maxJSONLRecords       = 250_000
)

// NewCorpus validates, deep-copies, canonically orders, and identifies a
// corpus. The SHA-256 covers the exact canonical JSONL bytes, including one
// trailing LF per record.
func NewCorpus(records []CorpusRecord) (Corpus, error) {
	payload, canonical, err := canonicalCorpusJSONL(records)
	if err != nil {
		return Corpus{}, err
	}
	sum := sha256.Sum256(payload)

	return Corpus{
		SHA256:  hex.EncodeToString(sum[:]),
		Records: canonical,
	}, nil
}

// CorpusIdentity returns the SHA-256 identity of canonical JSONL without
// writing it.
func CorpusIdentity(records []CorpusRecord) (string, error) {
	corpus, err := NewCorpus(records)
	if err != nil {
		return "", err
	}

	return corpus.SHA256, nil
}

// WriteCorpus writes validated canonical JSONL and returns its SHA-256
// identity. Validation and canonicalization complete before any bytes are
// written, so malformed input cannot leave a partial corpus artifact.
func WriteCorpus(w io.Writer, records []CorpusRecord) (string, error) {
	if w == nil {
		return "", fmt.Errorf("writer is nil")
	}

	payload, _, err := canonicalCorpusJSONL(records)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)

	if _, err := io.Copy(w, bytes.NewReader(payload)); err != nil {
		return "", fmt.Errorf("write corpus: %w", err)
	}

	return hex.EncodeToString(sum[:]), nil
}

// ReadCorpus strictly decodes JSONL, rejects unknown/duplicate JSON fields,
// validates the records, and returns canonical order plus corpus identity.
func ReadCorpus(r io.Reader) (Corpus, error) {
	records, err := readJSONL[CorpusRecord](r, "corpus")
	if err != nil {
		return Corpus{}, err
	}

	return NewCorpus(records)
}

// WriteResults writes result records in stable system/task/case order.
func WriteResults(w io.Writer, results []ResultRecord) error {
	if w == nil {
		return fmt.Errorf("writer is nil")
	}
	if err := ValidateResults(results); err != nil {
		return err
	}

	canonical := canonicalResults(results)
	var payload bytes.Buffer
	for i := range canonical {
		line, err := json.Marshal(canonical[i])
		if err != nil {
			return fmt.Errorf("marshal result %d: %w", i+1, err)
		}
		payload.Write(line)
		payload.WriteByte('\n')
	}

	if _, err := io.Copy(w, bytes.NewReader(payload.Bytes())); err != nil {
		return fmt.Errorf("write results: %w", err)
	}

	return nil
}

// ReadResults strictly decodes, validates, and canonically orders result
// records. It never retains raw provider envelopes or errors.
func ReadResults(r io.Reader) ([]ResultRecord, error) {
	results, err := readJSONL[ResultRecord](r, "results")
	if err != nil {
		return nil, err
	}
	if err := ValidateResults(results); err != nil {
		return nil, err
	}

	return canonicalResults(results), nil
}

// ReadResultsWithThresholds additionally proves that every normalized action
// is reachable under the exact run/roster thresholds supplied by the caller.
func ReadResultsWithThresholds(
	r io.Reader,
	thresholds DecisionThresholds,
) ([]ResultRecord, error) {
	results, err := readJSONL[ResultRecord](r, "results")
	if err != nil {
		return nil, err
	}
	if err := ValidateResultsWithThresholds(results, thresholds); err != nil {
		return nil, err
	}
	return canonicalResults(results), nil
}

func canonicalCorpusJSONL(records []CorpusRecord) ([]byte, []CorpusRecord, error) {
	if err := ValidateCorpus(records); err != nil {
		return nil, nil, err
	}

	canonical := make([]CorpusRecord, len(records))
	for i := range records {
		canonical[i] = canonicalCorpusRecord(records[i])
	}
	sort.Slice(canonical, func(i, j int) bool {
		if canonical[i].Task != canonical[j].Task {
			return canonical[i].Task < canonical[j].Task
		}
		return canonical[i].CaseID < canonical[j].CaseID
	})

	var payload bytes.Buffer
	for i := range canonical {
		line, err := json.Marshal(canonical[i])
		if err != nil {
			return nil, nil, fmt.Errorf("marshal corpus record %d: %w", i+1, err)
		}
		payload.Write(line)
		payload.WriteByte('\n')
	}

	return payload.Bytes(), canonical, nil
}

func canonicalCorpusRecord(in CorpusRecord) CorpusRecord {
	out := in
	out.SliceIDs = append([]string(nil), in.SliceIDs...)
	sort.Strings(out.SliceIDs)
	if in.Label.HumanReviewProof != nil {
		proof := *in.Label.HumanReviewProof
		out.Label.HumanReviewProof = &proof
	}
	if in.PrivacyAttestation != nil {
		attestation := *in.PrivacyAttestation
		out.PrivacyAttestation = &attestation
	}

	if in.MatcherExtract != nil {
		task := *in.MatcherExtract
		task.Input.FilePaths = append([]string(nil), in.MatcherExtract.Input.FilePaths...)
		task.Expected.Acceptable = append(
			[]MatcherExtraction(nil),
			in.MatcherExtract.Expected.Acceptable...,
		)
		sort.Slice(task.Expected.Acceptable, func(i, j int) bool {
			left, _ := json.Marshal(task.Expected.Acceptable[i])
			right, _ := json.Marshal(task.Expected.Acceptable[j])
			return bytes.Compare(left, right) < 0
		})
		out.MatcherExtract = &task
	}

	if in.MatcherRerank != nil {
		task := *in.MatcherRerank
		task.Input.Candidates = append(
			[]MatcherCandidate(nil),
			in.MatcherRerank.Input.Candidates...,
		)
		task.Expected.AcceptableTMDBIDs = append(
			[]int64(nil),
			in.MatcherRerank.Expected.AcceptableTMDBIDs...,
		)
		sort.Slice(task.Expected.AcceptableTMDBIDs, func(i, j int) bool {
			return task.Expected.AcceptableTMDBIDs[i] < task.Expected.AcceptableTMDBIDs[j]
		})
		out.MatcherRerank = &task
	}

	if in.ContentFilter != nil {
		task := *in.ContentFilter
		out.ContentFilter = &task
	}
	if in.JunkPurge != nil {
		task := *in.JunkPurge
		out.JunkPurge = &task
	}

	return out
}

func canonicalResults(results []ResultRecord) []ResultRecord {
	out := append([]ResultRecord(nil), results...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].System.SystemID != out[j].System.SystemID {
			return out[i].System.SystemID < out[j].System.SystemID
		}
		if out[i].Task != out[j].Task {
			return out[i].Task < out[j].Task
		}
		return out[i].CaseID < out[j].CaseID
	})

	return out
}

func readJSONL[T any](r io.Reader, artifact string) ([]T, error) {
	return readJSONLWithLimits[T](
		r,
		artifact,
		maxJSONLRecords,
		maxJSONLArtifactBytes,
	)
}

func readJSONLWithLimits[T any](
	r io.Reader,
	artifact string,
	maxRecords int,
	maxArtifactBytes int64,
) ([]T, error) {
	if r == nil {
		return nil, fmt.Errorf("%s reader is nil", artifact)
	}
	if maxRecords <= 0 || maxArtifactBytes <= 0 {
		return nil, fmt.Errorf("%s reader limits must be positive", artifact)
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), maxJSONLLineBytes)

	var (
		records    []T
		lineNo     int
		totalBytes int64
	)

	for scanner.Scan() {
		lineNo++
		if lineNo > maxRecords {
			return nil, fmt.Errorf(
				"%s exceeds the %d-record limit",
				artifact,
				maxRecords,
			)
		}
		line := append([]byte(nil), scanner.Bytes()...)
		lineBytes := int64(len(line)) + 1
		if lineBytes > maxArtifactBytes-totalBytes {
			return nil, fmt.Errorf(
				"%s exceeds the %d-byte limit",
				artifact,
				maxArtifactBytes,
			)
		}
		totalBytes += lineBytes
		if len(bytes.TrimSpace(line)) == 0 {
			return nil, fmt.Errorf("%s line %d: blank lines are not allowed", artifact, lineNo)
		}
		if err := rejectDuplicateJSONKeys(line); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", artifact, lineNo, err)
		}

		var record T
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&record); err != nil {
			return nil, fmt.Errorf("%s line %d: decode: %w", artifact, lineNo, err)
		}

		var extra any
		if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
			if err == nil {
				return nil, fmt.Errorf("%s line %d: multiple JSON values", artifact, lineNo)
			}
			return nil, fmt.Errorf("%s line %d: trailing data: %w", artifact, lineNo, err)
		}

		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf(
			"%s JSONL scan failed (maximum line size %d bytes): %w",
			artifact,
			maxJSONLLineBytes,
			err,
		)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("%s JSONL is empty", artifact)
	}

	return records, nil
}

// rejectDuplicateJSONKeys prevents ambiguous JSON objects before the standard
// library's last-key-wins decoding can erase the evidence.
func rejectDuplicateJSONKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := consumeJSONValue(dec); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("invalid JSON: multiple top-level values")
		}
		return fmt.Errorf("invalid JSON: trailing data: %w", err)
	}

	return nil
}

func consumeJSONValue(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}

	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return nil
	}

	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate object key %q", key)
			}
			seen[key] = struct{}{}
			if err := consumeJSONValue(dec); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("object ended with %v", end)
		}
	case '[':
		for dec.More() {
			if err := consumeJSONValue(dec); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("array ended with %v", end)
		}
	default:
		return fmt.Errorf("unexpected delimiter %q", delim)
	}

	return nil
}

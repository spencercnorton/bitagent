package v1

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"regexp"
	"slices"
	"time"
)

const MaxBody = 262144

var token = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._+-]{0,95}$`)
var hash = regexp.MustCompile(`^[a-f0-9]{64}$`)
var infohash = regexp.MustCompile(`^[a-f0-9]{40}$`)

// Decode rejects duplicate keys as well as unknown fields. Keeping both checks
// prevents different parsers from disagreeing about a binding or proposal.
func Decode(raw []byte, into any) error {
	return decodeBounded(raw, into, MaxBody)
}
func decodeBounded(raw []byte, into any, limit int) error {
	if len(raw) > limit {
		return errors.New("body_too_large")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := scan(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing_json")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(into)
}
func scan(d *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("json_depth")
	}
	t, e := d.Token()
	if e != nil {
		return e
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			t, e = d.Token()
			if e != nil {
				return e
			}
			k, ok := t.(string)
			if !ok || seen[k] {
				return errors.New("duplicate_key")
			}
			seen[k] = true
			if e = scan(d, depth+1); e != nil {
				return e
			}
		}
	case '[':
		for d.More() {
			if e = scan(d, depth+1); e != nil {
				return e
			}
		}
	default:
		return errors.New("invalid_json")
	}
	_, e = d.Token()
	return e
}
func (i Identity) Validate() error {
	if !token.MatchString(i.ID) || !token.MatchString(i.Version) || !token.MatchString(i.ModelVersion) || !token.MatchString(i.PolicyVersion) {
		return errors.New("invalid_identity")
	}
	if i.ArtifactDigest != "none" && !hash.MatchString(i.ArtifactDigest) {
		return errors.New("invalid_artifact_digest")
	}
	return nil
}
func (m Manifest) Validate() error {
	if m.Contract != Contract {
		return errors.New("incompatible_contract")
	}
	if e := m.Processor.Validate(); e != nil {
		return e
	}
	if len(m.Capabilities) == 0 || len(m.Capabilities) > len(Stages) {
		return errors.New("invalid_capabilities")
	}
	seen := map[Stage]bool{}
	for _, s := range m.Capabilities {
		if !slices.Contains(Stages, s) || seen[s] {
			return errors.New("invalid_capabilities")
		}
		seen[s] = true
	}
	if !slices.Contains([]string{"unqualified", "synthetic", "qualified"}, m.Qualification) {
		return errors.New("invalid_qualification")
	}
	if len(m.ConfigurationSchema) == 0 || len(m.ConfigurationSchema) > 16384 {
		return errors.New("invalid_configuration_schema")
	}
	var schema map[string]any
	if e := json.Unmarshal(m.ConfigurationSchema, &schema); e != nil {
		return e
	}
	var refs func(any) bool
	refs = func(v any) bool {
		switch x := v.(type) {
		case map[string]any:
			for k, v := range x {
				if k == "$ref" || refs(v) {
					return true
				}
			}
		case []any:
			for _, v := range x {
				if refs(v) {
					return true
				}
			}
		}
		return false
	}
	if schema["$schema"] != nil && schema["$schema"] != "http://json-schema.org/draft-07/schema#" {
		return errors.New("configuration_schema_version")
	}
	if schema["type"] != "object" {
		return errors.New("configuration_schema_type")
	}
	if refs(schema) {
		return errors.New("configuration_schema_refs_unsupported")
	}
	return nil
}
func (j Job) Validate() error {
	if j.Contract != Contract || !hash.MatchString(j.JobID) || !hash.MatchString(j.InputRevision) || !hash.MatchString(j.PipelineRevision) || !hash.MatchString(j.ConfigurationDigest) || !slices.Contains(Stages, j.Stage) {
		return errors.New("invalid_binding")
	}
	if e := j.Processor.Validate(); e != nil {
		return e
	}
	var configuration map[string]json.RawMessage
	if e := json.Unmarshal(j.Configuration, &configuration); e != nil || configuration == nil {
		return errors.New("invalid_configuration")
	}
	if !infohash.MatchString(j.Input.InfoHash) || len(j.Input.Name) == 0 || len(j.Input.Name) > 4096 || j.Input.Size < 0 || len(j.Input.Files) > 256 || len(j.Input.Candidates) > 32 || len(j.Input.PriorResults) > 96 || len(j.Configuration) > 4096 {
		return errors.New("invalid_input")
	}
	if _, e := time.Parse(time.RFC3339Nano, j.Deadline); e != nil {
		return errors.New("invalid_deadline")
	}
	if !slices.Contains([]string{"completed", "deferred", "not_attempted"}, j.Input.Deterministic.ProcessingOutcome) {
		return errors.New("invalid_processing_outcome")
	}
	if !slices.Contains([]string{"matched", "completed_unmatched", "deferred", "not_attempted"}, j.Input.Deterministic.MatchingOutcome) {
		return errors.New("invalid_matching_outcome")
	}
	if j.Input.FileCount != nil && *j.Input.FileCount < 0 {
		return errors.New("invalid_file_count")
	}
	if j.Input.Extension != nil && len(*j.Input.Extension) > 64 {
		return errors.New("invalid_extension")
	}
	for _, f := range j.Input.Files {
		if len(f.Path) > 4096 || f.Size < 0 {
			return errors.New("invalid_file")
		}
	}
	return nil
}
func (r Result) Validate(j Job) error {
	if !reflect.DeepEqual(r.Binding, j.Binding) {
		return errors.New("binding_mismatch")
	}
	if !slices.Contains([]Outcome{Completed, Abstained, Deferred, Failed, Excluded, Unmatched}, r.Outcome) {
		return errors.New("invalid_outcome")
	}
	if len(r.Reasons) > 8 || len(r.Evidence) > 8 || r.RetryAfterSeconds < 0 || r.RetryAfterSeconds > 3600 {
		return errors.New("result_bounds")
	}
	for _, s := range r.Reasons {
		if len(s) > 256 {
			return errors.New("reason_too_large")
		}
	}
	for _, e := range r.Evidence {
		if len(e.Kind) > 64 || len(e.Reference) > 512 {
			return errors.New("evidence_too_large")
		}
	}
	if e := r.Telemetry.Validate(); e != nil {
		return e
	}
	if r.Uncertainty != nil && (!finite(*r.Uncertainty) || *r.Uncertainty < 0 || *r.Uncertainty > 1) {
		return errors.New("invalid_uncertainty")
	}
	for _, v := range []*float64{r.Telemetry.InferenceMS, r.Telemetry.CPUMilliseconds} {
		if v != nil && (!finite(*v) || *v < 0) {
			return errors.New("invalid_telemetry")
		}
	}
	if r.Telemetry.RSSBytes != nil && *r.Telemetry.RSSBytes < 0 {
		return errors.New("invalid_telemetry")
	}
	if r.Outcome == Unmatched && (j.Stage != Match || j.Input.Deterministic.MatchingOutcome != "completed_unmatched") {
		return errors.New("unproven_complete_miss")
	}
	if r.Outcome != Deferred && r.RetryAfterSeconds != 0 {
		return errors.New("unexpected_retry_delay")
	}
	if r.Outcome != Completed && r.Outcome != Excluded {
		if r.Proposal != nil {
			return errors.New("unexpected_proposal")
		}
		return nil
	}
	if r.Proposal == nil {
		return errors.New("missing_proposal")
	}
	p := r.Proposal
	count := 0
	if p.ContentType != "" {
		count++
	}
	if p.Extraction != nil {
		count++
	}
	if p.Match != nil {
		count++
	}
	if p.Language != nil {
		count++
	}
	if p.Exclusion != nil {
		count++
	}
	if p.Embedding != nil {
		count++
	}
	if count != 1 {
		return errors.New("one_proposal_per_stage")
	}
	switch j.Stage {
	case Classify:
		if !slices.Contains([]string{"movie", "tv_show", "music", "ebook", "audiobook", "comic", "game", "software", "xxx"}, p.ContentType) {
			return errors.New("invalid_content_type")
		}
	case Extract:
		if p.Extraction == nil || len(p.Extraction.Title) == 0 || len(p.Extraction.Title) > 512 || p.Extraction.Year < 0 || p.Extraction.Year > 2200 || len(p.Extraction.Episodes) > 100 {
			return errors.New("invalid_extraction")
		}
		if p.Extraction.Season != nil && (*p.Extraction.Season < 0 || *p.Extraction.Season > 1000) {
			return errors.New("invalid_season")
		}
		for _, e := range p.Extraction.Episodes {
			if e < 0 || e > 10000 {
				return errors.New("invalid_episode")
			}
		}
	case Match:
		if p.Match == nil || !slices.ContainsFunc(j.Input.Candidates, func(c Candidate) bool { return c.MatchRef == *p.Match }) {
			return errors.New("unoffered_candidate")
		}
	case Language:
		if p.Language == nil || len(p.Language.Audio) > 16 || len(p.Language.Subtitles) > 16 || !slices.Contains([]string{"metadata_claim", "verified_reference"}, p.Language.Evidence) {
			return errors.New("invalid_language")
		}
		for _, l := range append(append([]string{}, p.Language.Audio...), p.Language.Subtitles...) {
			if !regexp.MustCompile(`^[a-z]{2,3}$`).MatchString(l) {
				return errors.New("invalid_language")
			}
		}
	case Policy:
		if p.Exclusion == nil || p.Exclusion.Scope != "publication" || len(p.Exclusion.Reason) == 0 || len(p.Exclusion.Reason) > 128 || r.Outcome != Excluded {
			return errors.New("invalid_exclusion")
		}
	case Embed:
		if p.Embedding == nil || !token.MatchString(p.Embedding.Space) || len(p.Embedding.Values) == 0 || len(p.Embedding.Values) > 2048 {
			return errors.New("invalid_embedding")
		}
		for _, v := range p.Embedding.Values {
			if !finite(v) {
				return errors.New("invalid_embedding")
			}
		}
	default:
		return fmt.Errorf("unsupported_stage")
	}
	if r.Outcome == Excluded && j.Stage != Policy {
		return errors.New("unexpected_exclusion")
	}
	return nil
}
func finite(n float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) }

func (t Telemetry) Validate() error {
	if t.ObservedAt != "" {
		ts, e := time.Parse(time.RFC3339Nano, t.ObservedAt)
		if e != nil || ts.After(time.Now().Add(time.Minute)) {
			return errors.New("invalid_telemetry_time")
		}
	}
	for _, v := range []*float64{t.InferenceMS, t.CPUMilliseconds} {
		if v != nil && (!finite(*v) || *v < 0) {
			return errors.New("invalid_telemetry")
		}
	}
	if t.RSSBytes != nil && *t.RSSBytes < 0 {
		return errors.New("invalid_telemetry")
	}
	return nil
}
func (s ServiceStatus) Validate() error {
	if s.Contract != Contract {
		return errors.New("incompatible_contract")
	}
	if e := s.Processor.Validate(); e != nil {
		return e
	}
	if !slices.Contains([]string{"warming", "ready", "draining", "unavailable"}, s.State) || len(s.InstanceID) > 128 {
		return errors.New("invalid_service_state")
	}
	if s.StartedAt != "" {
		if _, e := time.Parse(time.RFC3339Nano, s.StartedAt); e != nil {
			return errors.New("invalid_start_time")
		}
	}
	for _, n := range []*int64{s.Attempts, s.UniqueTorrents} {
		if n != nil && *n < 0 {
			return errors.New("invalid_counter")
		}
	}
	if len(s.Outcomes) > 6 {
		return errors.New("invalid_counters")
	}
	for k, n := range s.Outcomes {
		if !slices.Contains([]Outcome{Completed, Abstained, Deferred, Failed, Excluded, Unmatched}, k) || n < 0 {
			return errors.New("invalid_counter")
		}
	}
	return s.Telemetry.Validate()
}

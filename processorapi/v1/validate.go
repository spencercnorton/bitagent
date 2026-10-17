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
var tagToken = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,63}$`)
var langToken = regexp.MustCompile(`^[a-z]{2,3}$`)

var ValidContentTypes = []string{"movie", "tv_show", "music", "ebook", "audiobook", "comic", "game", "software", "xxx"}

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
func (s *Selector) Validate() error {
	if s == nil {
		return nil
	}
	if len(s.ContentTypes) > 16 || len(s.ExcludeTypes) > 16 || len(s.Extensions) > 64 || len(s.RequiredTags) > 32 || len(s.ScoreThresholds) > 16 {
		return errors.New("invalid_selector")
	}
	for _, ct := range append(append([]string{}, s.ContentTypes...), s.ExcludeTypes...) {
		if ct != "unknown" && !slices.Contains(ValidContentTypes, ct) {
			return errors.New("invalid_selector_content_type")
		}
	}
	for _, ext := range s.Extensions {
		if len(ext) == 0 || len(ext) > 32 {
			return errors.New("invalid_selector_extension")
		}
	}
	for _, tag := range s.RequiredTags {
		if !tagToken.MatchString(tag) {
			return errors.New("invalid_selector_tag")
		}
	}
	if s.MinUncertainty != nil && (!finite(*s.MinUncertainty) || *s.MinUncertainty < 0 || *s.MinUncertainty > 1) {
		return errors.New("invalid_selector_uncertainty")
	}
	if s.SampleBasisPoints != nil && (*s.SampleBasisPoints < 0 || *s.SampleBasisPoints > 10000) {
		return errors.New("invalid_selector_sample")
	}
	for _, st := range s.ScoreThresholds {
		if !token.MatchString(st.Dimension) {
			return errors.New("invalid_selector_threshold")
		}
		if st.Min != nil && !finite(*st.Min) {
			return errors.New("invalid_selector_threshold")
		}
		if st.Max != nil && !finite(*st.Max) {
			return errors.New("invalid_selector_threshold")
		}
		if st.Min != nil && st.Max != nil && *st.Min > *st.Max {
			return errors.New("invalid_selector_threshold")
		}
	}
	return nil
}
func (l *LearningConfig) Validate() error {
	if l == nil {
		return nil
	}
	if l.Mode != "" && !slices.Contains([]string{"disabled", "log_only", "online_calibrate", "accumulate_batch"}, l.Mode) {
		return errors.New("invalid_learning_mode")
	}
	if len(l.AcceptedTiers) > len(FeedbackTiers) {
		return errors.New("invalid_learning_tiers")
	}
	seen := map[string]bool{}
	for _, tier := range l.AcceptedTiers {
		if !slices.Contains(FeedbackTiers, tier) || seen[tier] {
			return errors.New("invalid_learning_tiers")
		}
		seen[tier] = true
	}
	return nil
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
	if len(j.Capabilities) > len(Stages) {
		return errors.New("invalid_capabilities")
	}
	seenCap := map[Stage]bool{}
	for _, s := range j.Capabilities {
		if !slices.Contains(Stages, s) || seenCap[s] {
			return errors.New("invalid_capabilities")
		}
		seenCap[s] = true
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
	if err := validateInputSourcesAndSwarm(j.Input.Sources, j.Input.Swarm); err != nil {
		return err
	}
	return nil
}

var privateSourceGuard = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(?:private|bitgrab|passkey|authkey|torrentleech|broadcastthenet|broadcasthenet|passthepopcorn|redacted|orpheus|bibliotik|myanonamouse|filelist|beyondhd|blutopia|uhdbits|morethantv|alpharatio|iptorrents|hdbits|gazellegames|animebytes|avistaz|cinemaz|exoticaz|empirnium)(?:$|[^a-z0-9])`)
var passkeyQueryGuard = regexp.MustCompile(`(?i)(?:\?|&)(?:passkey|authkey|apikey|key|token|uid|pid)=`)

func isPublicSafeSourceString(s string) bool {
	if len(s) == 0 || len(s) > 256 {
		return false
	}
	return !privateSourceGuard.MatchString(s) && !passkeyQueryGuard.MatchString(s)
}

func validateInputSourcesAndSwarm(sources []SourceObservation, sw *SwarmInput) error {
	if len(sources) > 32 {
		return errors.New("invalid_sources")
	}
	for _, src := range sources {
		if !isPublicSafeSourceString(src.Source) {
			return errors.New("private_or_invalid_source")
		}
		for _, v := range []*int{src.Seeders, src.Leechers, src.Completed} {
			if v != nil && (*v < 0 || *v > 100000000) {
				return errors.New("invalid_source_counts")
			}
		}
	}
	if sw == nil {
		return nil
	}
	for _, v := range []*int{sw.TrackerSeeders, sw.TrackerLeechers, sw.TrackerCompleted, sw.PrevSeeders, sw.PeakSeeders, sw.DHTSeeders, sw.DHTLeechers, sw.SuspectObservations} {
		if v != nil && (*v < 0 || *v > 100000000) {
			return errors.New("invalid_swarm_counts")
		}
	}
	if sw.BestTracker != "" && !isPublicSafeSourceString(sw.BestTracker) {
		return errors.New("private_or_invalid_tracker")
	}
	for _, ts := range []string{sw.LastPositiveAt, sw.CheckedAt} {
		if ts != "" {
			if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
				return errors.New("invalid_swarm_timestamp")
			}
		}
	}
	if sw.LivenessStatus != "" && !slices.Contains([]string{"alive", "suspect", "dead", "unknown"}, sw.LivenessStatus) {
		return errors.New("invalid_liveness_status")
	}
	if len(sw.LastQBState) > 64 || len(sw.AliveSource) > 64 {
		return errors.New("invalid_liveness_metadata")
	}
	if sw.AliveSource != "" && !isPublicSafeSourceString(sw.AliveSource) {
		return errors.New("private_or_invalid_alive_source")
	}
	if len(sw.OutcomePriors) > 32 {
		return errors.New("invalid_outcome_priors")
	}
	validPriorTypes := []string{"source", "release_group", "quality_tag", "codec", "container", "extension", "resolution", "language"}
	for _, p := range sw.OutcomePriors {
		if !slices.Contains(validPriorTypes, p.KeyType) || !isPublicSafeSourceString(p.KeyValue) || p.Alpha < 1 || p.Beta < 1 {
			return errors.New("invalid_outcome_prior")
		}
	}
	return nil
}

func validateClassifyProposal(contentType string) error {
	if !slices.Contains(ValidContentTypes, contentType) {
		return errors.New("invalid_content_type")
	}
	return nil
}

func validateExtractProposal(ex *Extraction) error {
	if ex == nil {
		return errors.New("invalid_extraction")
	}
	hasQualityOrMeta := ex.VideoResolution != "" || ex.VideoSource != "" || ex.VideoCodec != "" ||
		ex.Video3D != "" || ex.VideoModifier != "" || ex.ReleaseGroup != "" ||
		ex.ReleaseGranularity != "" || ex.ReleaseDate != "" || ex.IsAnime != nil ||
		ex.AnimeAbsoluteEpisode != nil || len(ex.AudioChannels) > 0 || len(ex.AudioFormats) > 0 ||
		len(ex.HDRFormats) > 0 || len(ex.VideoEditions) > 0 || len(ex.ReleaseFlags) > 0
	if len(ex.Title) == 0 && !hasQualityOrMeta {
		return errors.New("invalid_extraction")
	}
	if len(ex.Title) > 512 || ex.Year < 0 || ex.Year > 2200 || len(ex.Episodes) > 100 || len(ex.ReleaseGroup) > 128 {
		return errors.New("invalid_extraction")
	}
	if ex.Season != nil && (*ex.Season < 0 || *ex.Season > 1000) {
		return errors.New("invalid_season")
	}
	if ex.AnimeAbsoluteEpisode != nil && (*ex.AnimeAbsoluteEpisode < 0 || *ex.AnimeAbsoluteEpisode > 100000) {
		return errors.New("invalid_episode")
	}
	for _, e := range ex.Episodes {
		if e < 0 || e > 10000 {
			return errors.New("invalid_episode")
		}
	}
	return nil
}

func validateMatchProposal(m *MatchRef, candidates []Candidate, multiCap bool) error {
	if m == nil || m.Source == "" || m.ID == "" || len(m.Source) > 64 || len(m.ID) > 128 {
		return errors.New("unoffered_candidate")
	}
	if len(candidates) > 0 || !multiCap {
		if !slices.ContainsFunc(candidates, func(c Candidate) bool { return c.MatchRef == *m }) {
			return errors.New("unoffered_candidate")
		}
	}
	return nil
}

func validateLanguageProposal(l *LanguageClaim) error {
	if l == nil || len(l.Audio) > 16 || len(l.Subtitles) > 16 || !slices.Contains([]string{"metadata_claim", "verified_reference"}, l.Evidence) {
		return errors.New("invalid_language")
	}
	for _, code := range append(append([]string{}, l.Audio...), l.Subtitles...) {
		if !langToken.MatchString(code) {
			return errors.New("invalid_language")
		}
	}
	return nil
}

func validateLivenessProposal(l *LivenessClaim) error {
	if l == nil {
		return nil
	}
	if l.Seeders != nil && (*l.Seeders < 0 || *l.Seeders > 10000000) {
		return errors.New("invalid_liveness")
	}
	if l.Leechers != nil && (*l.Leechers < 0 || *l.Leechers > 10000000) {
		return errors.New("invalid_liveness")
	}
	if l.Completed != nil && (*l.Completed < 0 || *l.Completed > 100000000) {
		return errors.New("invalid_liveness")
	}
	if len(l.Observation) > 64 || len(l.Source) > 128 {
		return errors.New("invalid_liveness")
	}
	return nil
}

func validateTagsAndWeights(addTags, removeTags []string, weights map[string]float64) error {
	if len(addTags) > 32 || len(removeTags) > 32 || len(weights) > 32 {
		return errors.New("invalid_policy_operations")
	}
	for _, tag := range append(append([]string{}, addTags...), removeTags...) {
		if !tagToken.MatchString(tag) {
			return errors.New("invalid_tag")
		}
	}
	for k, v := range weights {
		if !token.MatchString(k) || !finite(v) || v < -1000 || v > 1000 {
			return errors.New("invalid_weight")
		}
	}
	return nil
}

func validateEmbedProposal(em *Embedding) error {
	if em == nil || !token.MatchString(em.Space) || len(em.Values) == 0 || len(em.Values) > 2048 {
		return errors.New("invalid_embedding")
	}
	for _, v := range em.Values {
		if !finite(v) {
			return errors.New("invalid_embedding")
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
	if len(r.Reasons) > 8 || len(r.Evidence) > 8 || r.RetryAfterSeconds < 0 || r.RetryAfterSeconds > 3600 || len(r.Trace) > 65536 {
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
	caps := j.EffectiveCapabilities()
	multiCap := len(j.Capabilities) > 1
	if r.Outcome == Unmatched && (!slices.Contains(caps, Match) || j.Input.Deterministic.MatchingOutcome != "completed_unmatched") {
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
	hasPolicyOps := p.Liveness != nil || len(p.AddTags) > 0 || len(p.RemoveTags) > 0 || len(p.Weights) > 0
	if !multiCap && !hasPolicyOps {
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
			if err := validateClassifyProposal(p.ContentType); err != nil {
				return err
			}
		case Extract:
			if err := validateExtractProposal(p.Extraction); err != nil {
				return err
			}
		case Match:
			if err := validateMatchProposal(p.Match, j.Input.Candidates, false); err != nil {
				return err
			}
		case Language:
			if err := validateLanguageProposal(p.Language); err != nil {
				return err
			}
		case Policy:
			if p.Exclusion == nil || p.Exclusion.Scope != "publication" || len(p.Exclusion.Reason) == 0 || len(p.Exclusion.Reason) > 128 || r.Outcome != Excluded {
				return errors.New("invalid_exclusion")
			}
		case Embed:
			if err := validateEmbedProposal(p.Embedding); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported_stage")
		}
		if r.Outcome == Excluded && j.Stage != Policy {
			return errors.New("unexpected_exclusion")
		}
		return nil
	}

	// Multi-capability single-pass or Policy operation patch validation.
	anyField := false
	if p.ContentType != "" {
		anyField = true
		if !slices.Contains(caps, Classify) {
			return errors.New("unauthorized_classify_proposal")
		}
		if err := validateClassifyProposal(p.ContentType); err != nil {
			return err
		}
	}
	if p.Extraction != nil {
		anyField = true
		if !slices.Contains(caps, Extract) {
			return errors.New("unauthorized_extract_proposal")
		}
		if err := validateExtractProposal(p.Extraction); err != nil {
			return err
		}
	}
	if p.Match != nil {
		anyField = true
		if !slices.Contains(caps, Match) {
			return errors.New("unauthorized_match_proposal")
		}
		if err := validateMatchProposal(p.Match, j.Input.Candidates, multiCap); err != nil {
			return err
		}
	}
	if p.Language != nil {
		anyField = true
		if !slices.Contains(caps, Language) {
			return errors.New("unauthorized_language_proposal")
		}
		if err := validateLanguageProposal(p.Language); err != nil {
			return err
		}
	}
	if p.Exclusion != nil {
		anyField = true
		if !slices.Contains(caps, Policy) {
			return errors.New("unexpected_exclusion")
		}
		if p.Exclusion.Scope != "publication" || len(p.Exclusion.Reason) == 0 || len(p.Exclusion.Reason) > 128 || r.Outcome != Excluded {
			return errors.New("invalid_exclusion")
		}
	}
	if hasPolicyOps {
		anyField = true
		if !slices.Contains(caps, Policy) && !multiCap {
			return errors.New("unauthorized_policy_proposal")
		}
		if err := validateLivenessProposal(p.Liveness); err != nil {
			return err
		}
		if err := validateTagsAndWeights(p.AddTags, p.RemoveTags, p.Weights); err != nil {
			return err
		}
	}
	if p.Embedding != nil {
		anyField = true
		if !slices.Contains(caps, Embed) {
			return errors.New("unauthorized_embed_proposal")
		}
		if err := validateEmbedProposal(p.Embedding); err != nil {
			return err
		}
	}
	if !anyField {
		return errors.New("missing_proposal")
	}
	if r.Outcome == Excluded && p.Exclusion == nil {
		return errors.New("invalid_exclusion")
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

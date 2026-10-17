// Package v1 is the framework-independent BitAgent processor wire contract.
package v1

import (
	"encoding/json"
	"slices"
)

const Contract = "bitagent.processor.v1"

type Stage string

const (
	Classify Stage = "classify"
	Extract  Stage = "extract"
	Match    Stage = "match"
	Language Stage = "language"
	Policy   Stage = "policy"
	Embed    Stage = "embed"
)

var Stages = []Stage{Classify, Extract, Match, Language, Policy, Embed}

const (
	DriverHTTP            = "http"
	DriverBuiltinFilter   = "builtin/filter-list"
	DriverBuiltinRegex    = "builtin/regex-weights"
	DriverBuiltinLiveness = "builtin/tracker-liveness"
	DriverBuiltinLLM      = "builtin/llm"
)

var Drivers = []string{
	DriverHTTP,
	DriverBuiltinFilter,
	DriverBuiltinRegex,
	DriverBuiltinLiveness,
	DriverBuiltinLLM,
}

const (
	FeedbackTierOperationalReceipt = "operational_receipt"
	FeedbackTierTeacherConsensus   = "teacher_consensus"
	FeedbackTierVerifiedImport     = "verified_import"
	FeedbackTierHumanGold          = "human_gold"
)

var FeedbackTiers = []string{
	FeedbackTierOperationalReceipt,
	FeedbackTierTeacherConsensus,
	FeedbackTierVerifiedImport,
	FeedbackTierHumanGold,
}

type ScoreThreshold struct {
	Dimension string   `json:"dimension"`
	Min       *float64 `json:"min,omitempty"`
	Max       *float64 `json:"max,omitempty"`
}

// Selector filters which torrents a Processor is invoked for as a torrent flows
// through the priority-ordered processor stack.
type Selector struct {
	ContentTypes      []string         `json:"content_types,omitempty"`
	ExcludeTypes      []string         `json:"exclude_types,omitempty"`
	Extensions        []string         `json:"extensions,omitempty"`
	IsAnime           *bool            `json:"is_anime,omitempty"`
	HasContentMatch   *bool            `json:"has_content_match,omitempty"`
	Excluded          *bool            `json:"excluded,omitempty"`
	MinUncertainty    *float64         `json:"min_uncertainty,omitempty"`
	RequiredTags      []string         `json:"required_tags,omitempty"`
	ScoreThresholds   []ScoreThreshold `json:"score_thresholds,omitempty"`
	SampleBasisPoints *int             `json:"sample_basis_points,omitempty"`
}

func (s *Selector) IsEmpty() bool {
	if s == nil {
		return true
	}
	return len(s.ContentTypes) == 0 &&
		len(s.ExcludeTypes) == 0 &&
		len(s.Extensions) == 0 &&
		s.IsAnime == nil &&
		s.HasContentMatch == nil &&
		s.Excluded == nil &&
		s.MinUncertainty == nil &&
		len(s.RequiredTags) == 0 &&
		len(s.ScoreThresholds) == 0 &&
		s.SampleBasisPoints == nil
}

// LearningConfig controls bi-directional feedback routing and online calibration.
type LearningConfig struct {
	Enabled       bool     `json:"enabled"`
	Mode          string   `json:"mode,omitempty"` // disabled, log_only, online_calibrate, accumulate_batch
	AcceptedTiers []string `json:"accepted_tiers,omitempty"`
	PushEnabled   bool     `json:"push_enabled,omitempty"`
}

func (l *LearningConfig) Accepts(tier string) bool {
	if l == nil || !l.Enabled || l.Mode == "disabled" {
		return false
	}
	if len(l.AcceptedTiers) == 0 {
		return true
	}
	return slices.Contains(l.AcceptedTiers, tier)
}

// Identity is immutable for the life of a service instance. Non-model
// processors use model_version="none" and artifact_digest="none".
type Identity struct {
	ID             string `json:"id"`
	Version        string `json:"version"`
	ModelVersion   string `json:"model_version"`
	PolicyVersion  string `json:"policy_version"`
	ArtifactDigest string `json:"artifact_digest"`
}

func (i *Identity) ApplyDefaults(fallbackID string) {
	if i.ID == "" {
		i.ID = fallbackID
	}
	if i.Version == "" {
		i.Version = "1.0.0"
	}
	if i.ModelVersion == "" {
		i.ModelVersion = "none"
	}
	if i.PolicyVersion == "" {
		i.PolicyVersion = "default"
	}
	if i.ArtifactDigest == "" {
		i.ArtifactDigest = "none"
	}
}

type Manifest struct {
	Contract            string          `json:"contract"`
	Processor           Identity        `json:"processor"`
	Capabilities        []Stage         `json:"capabilities"`
	ConfigurationSchema json.RawMessage `json:"configuration_schema"`
	Qualification       string          `json:"qualification"` // informational: unqualified, synthetic, qualified
}

func (m *Manifest) ApplyDefaults(fallbackID string) {
	if m.Contract == "" {
		m.Contract = Contract
	}
	m.Processor.ApplyDefaults(fallbackID)
	if m.Qualification == "" {
		m.Qualification = "unqualified"
	}
	if len(m.ConfigurationSchema) == 0 {
		m.ConfigurationSchema = json.RawMessage(`{"type":"object"}`)
	}
}

type File struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}
type MatchRef struct {
	Source string `json:"source"`
	ID     string `json:"id"`
}
type Extraction struct {
	Title                string   `json:"title"`
	Year                 int      `json:"year"`
	Season               *int     `json:"season,omitempty"`
	Episodes             []int    `json:"episodes,omitempty"`
	VideoResolution      string   `json:"video_resolution,omitempty"`
	VideoSource          string   `json:"video_source,omitempty"`
	VideoCodec           string   `json:"video_codec,omitempty"`
	Video3D              string   `json:"video_3d,omitempty"`
	VideoModifier        string   `json:"video_modifier,omitempty"`
	ReleaseGroup         string   `json:"release_group,omitempty"`
	ReleaseGranularity   string   `json:"release_granularity,omitempty"`
	ReleaseDate          string   `json:"release_date,omitempty"`
	IsAnime              *bool    `json:"is_anime,omitempty"`
	AnimeAbsoluteEpisode *int     `json:"anime_absolute_episode,omitempty"`
	AudioChannels        []string `json:"audio_channels,omitempty"`
	AudioFormats         []string `json:"audio_formats,omitempty"`
	HDRFormats           []string `json:"hdr_formats,omitempty"`
	VideoEditions        []string `json:"video_editions,omitempty"`
	ReleaseFlags         []string `json:"release_flags,omitempty"`
}
type Deterministic struct {
	ProcessingOutcome string    `json:"processing_outcome"` // completed, deferred, not_attempted normal core processing
	ContentType       string    `json:"content_type"`
	Title             string    `json:"title"`
	Year              int       `json:"year"`
	Match             *MatchRef `json:"match"`
	// matched, completed_unmatched, deferred, not_attempted. Only the core
	// sets this from an actual complete search; absence never means a miss.
	MatchingOutcome string `json:"matching_outcome"`
}
type SourceObservation struct {
	Source    string `json:"source"`
	Seeders   *int   `json:"seeders,omitempty"`
	Leechers  *int   `json:"leechers,omitempty"`
	Completed *int   `json:"completed,omitempty"`
}
type PriorObservation struct {
	KeyType  string `json:"key_type"`
	KeyValue string `json:"key_value"`
	Alpha    int64  `json:"alpha"`
	Beta     int64  `json:"beta"`
}
type SwarmInput struct {
	TrackerScraped      bool               `json:"tracker_scraped"`
	TrackerKnown        bool               `json:"tracker_known"`
	TrackerSeeders      *int               `json:"tracker_seeders,omitempty"`
	TrackerLeechers     *int               `json:"tracker_leechers,omitempty"`
	TrackerCompleted    *int               `json:"tracker_completed,omitempty"`
	PrevSeeders         *int               `json:"prev_seeders,omitempty"`
	PeakSeeders         *int               `json:"peak_seeders,omitempty"`
	BestTracker         string             `json:"best_tracker,omitempty"`
	LastPositiveAt      string             `json:"last_positive_at,omitempty"`
	CheckedAt           string             `json:"checked_at,omitempty"`
	DHTSeeders          *int               `json:"dht_seeders,omitempty"`
	DHTLeechers         *int               `json:"dht_leechers,omitempty"`
	LivenessStatus      string             `json:"liveness_status,omitempty"`
	LastQBState         string             `json:"last_qb_state,omitempty"`
	SuspectObservations *int               `json:"suspect_observations,omitempty"`
	AliveSource         string             `json:"alive_source,omitempty"`
	OutcomePriors       []PriorObservation `json:"outcome_priors,omitempty"`
}
type Input struct {
	InfoHash          string              `json:"info_hash"`
	Name              string              `json:"name"`
	Size              int64               `json:"size"`
	Files             []File              `json:"files"`
	FileCount         *int64              `json:"file_count"`
	Extension         *string             `json:"extension"`
	FilesComplete     bool                `json:"files_complete"`
	Deterministic     Deterministic       `json:"deterministic"`
	Candidates        []Candidate         `json:"candidates"`
	PriorResults      []Result            `json:"prior_results"`
	AccumulatedScores map[string]float64  `json:"accumulated_scores,omitempty"`
	AccumulatedTags   []string            `json:"accumulated_tags,omitempty"`
	Sources           []SourceObservation `json:"sources,omitempty"`
	Swarm             *SwarmInput         `json:"swarm,omitempty"`
}
type Candidate struct {
	MatchRef
	ContentType string   `json:"content_type"`
	Title       string   `json:"title"`
	Year        int      `json:"year"`
	Aliases     []string `json:"aliases"`
}

type Binding struct {
	Contract            string   `json:"contract"`
	JobID               string   `json:"job_id"`
	InputRevision       string   `json:"input_revision"`
	PipelineRevision    string   `json:"pipeline_revision"`
	ConfigurationDigest string   `json:"configuration_digest"`
	Processor           Identity `json:"processor"`
	Stage               Stage    `json:"stage"`
}
type Job struct {
	Binding
	Capabilities  []Stage         `json:"capabilities,omitempty"`
	Configuration json.RawMessage `json:"configuration"`
	Input         Input           `json:"input"`
	Deadline      string          `json:"deadline"` // RFC3339, bounded attempt deadline
}

func (j Job) EffectiveCapabilities() []Stage {
	if len(j.Capabilities) > 0 {
		return j.Capabilities
	}
	if j.Stage != "" {
		return []Stage{j.Stage}
	}
	return nil
}

type Outcome string

const (
	Completed Outcome = "completed"
	Abstained Outcome = "abstained"
	Deferred  Outcome = "deferred"
	Failed    Outcome = "failed"
	Excluded  Outcome = "excluded"
	Unmatched Outcome = "unmatched"
)

type LanguageClaim struct {
	Audio        []string `json:"audio"`
	Subtitles    []string `json:"subtitles"`
	Evidence     string   `json:"evidence"` // metadata_claim or verified_reference; never trusted merely by name
	EnglishAudio string   `json:"english_audio,omitempty"`
}
type LivenessClaim struct {
	Seeders     *int   `json:"seeders,omitempty"`
	Leechers    *int   `json:"leechers,omitempty"`
	Completed   *int   `json:"completed,omitempty"`
	Observation string `json:"observation,omitempty"` // live, dead, unknown, scrape_ok
	Source      string `json:"source,omitempty"`
}
type Exclusion struct {
	Reason string `json:"reason"`
	Scope  string `json:"scope"`
}
type Embedding struct {
	Space  string    `json:"space"`
	Values []float64 `json:"values"`
}
type Proposal struct {
	ContentType string             `json:"content_type,omitempty"`
	Extraction  *Extraction        `json:"extraction,omitempty"`
	Match       *MatchRef          `json:"match,omitempty"`
	Language    *LanguageClaim     `json:"language,omitempty"`
	Liveness    *LivenessClaim     `json:"liveness,omitempty"`
	AddTags     []string           `json:"add_tags,omitempty"`
	RemoveTags  []string           `json:"remove_tags,omitempty"`
	Weights     map[string]float64 `json:"weights,omitempty"`
	Exclusion   *Exclusion         `json:"exclusion,omitempty"`
	Embedding   *Embedding         `json:"embedding,omitempty"`
}
type Evidence struct {
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
}
type Result struct {
	Binding
	Outcome           Outcome         `json:"outcome"`
	Proposal          *Proposal       `json:"proposal"`
	Reasons           []string        `json:"reasons"`
	Uncertainty       *float64        `json:"uncertainty"`
	Evidence          []Evidence      `json:"evidence"`
	RetryAfterSeconds int             `json:"retry_after_seconds"`
	Telemetry         Telemetry       `json:"telemetry"`
	Trace             json.RawMessage `json:"trace,omitempty"`
	RequestFeedback   bool            `json:"request_feedback,omitempty"`
}

func (r *Result) FillDefaultBinding(j Job) {
	if r.Binding == (Binding{}) {
		r.Binding = j.Binding
	}
	if r.Reasons == nil {
		r.Reasons = []string{}
	}
	if r.Evidence == nil {
		r.Evidence = []Evidence{}
	}
}

// Null means unavailable, never zero. Counters are process-lifetime values;
// the core separately reports its durable jobs, attempts and unique torrents.
type Telemetry struct {
	ObservedAt      string   `json:"observed_at,omitempty"`
	InferenceMS     *float64 `json:"inference_ms"`
	RSSBytes        *int64   `json:"rss_bytes"`
	CPUMilliseconds *float64 `json:"cpu_milliseconds"`
}
type ServiceStatus struct {
	InstanceID     string            `json:"instance_id"`
	StartedAt      string            `json:"started_at"`
	Contract       string            `json:"contract"`
	Processor      Identity          `json:"processor"`
	State          string            `json:"state"` // warming, ready, draining, unavailable
	Attempts       *int64            `json:"attempts"`
	UniqueTorrents *int64            `json:"unique_torrents"`
	Outcomes       map[Outcome]int64 `json:"outcomes"`
	Telemetry      Telemetry         `json:"telemetry"`
}

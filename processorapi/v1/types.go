// Package v1 is the framework-independent BitAgent processor wire contract.
package v1

import "encoding/json"

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

// Identity is immutable for the life of a service instance. Non-model
// processors use model_version="none" and artifact_digest="none".
type Identity struct {
	ID             string `json:"id"`
	Version        string `json:"version"`
	ModelVersion   string `json:"model_version"`
	PolicyVersion  string `json:"policy_version"`
	ArtifactDigest string `json:"artifact_digest"`
}

type Manifest struct {
	Contract            string          `json:"contract"`
	Processor           Identity        `json:"processor"`
	Capabilities        []Stage         `json:"capabilities"`
	ConfigurationSchema json.RawMessage `json:"configuration_schema"`
	Qualification       string          `json:"qualification"` // informational: unqualified, synthetic, qualified
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
	Title    string `json:"title"`
	Year     int    `json:"year"`
	Season   *int   `json:"season,omitempty"`
	Episodes []int  `json:"episodes,omitempty"`
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
type Input struct {
	InfoHash      string        `json:"info_hash"`
	Name          string        `json:"name"`
	Size          int64         `json:"size"`
	Files         []File        `json:"files"`
	FileCount     *int64        `json:"file_count"`
	Extension     *string       `json:"extension"`
	FilesComplete bool          `json:"files_complete"`
	Deterministic Deterministic `json:"deterministic"`
	Candidates    []Candidate   `json:"candidates"`
	PriorResults  []Result      `json:"prior_results"`
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
	Configuration json.RawMessage `json:"configuration"`
	Input         Input           `json:"input"`
	Deadline      string          `json:"deadline"` // RFC3339, bounded attempt deadline
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
	Audio     []string `json:"audio"`
	Subtitles []string `json:"subtitles"`
	Evidence  string   `json:"evidence"` // metadata_claim or verified_reference; never trusted merely by name
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
	ContentType string         `json:"content_type,omitempty"`
	Extraction  *Extraction    `json:"extraction,omitempty"`
	Match       *MatchRef      `json:"match,omitempty"`
	Language    *LanguageClaim `json:"language,omitempty"`
	Exclusion   *Exclusion     `json:"exclusion,omitempty"`
	Embedding   *Embedding     `json:"embedding,omitempty"`
}
type Evidence struct {
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
}
type Result struct {
	Binding
	Outcome           Outcome    `json:"outcome"`
	Proposal          *Proposal  `json:"proposal"`
	Reasons           []string   `json:"reasons"`
	Uncertainty       *float64   `json:"uncertainty"`
	Evidence          []Evidence `json:"evidence"`
	RetryAfterSeconds int        `json:"retry_after_seconds"`
	Telemetry         Telemetry  `json:"telemetry"`
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

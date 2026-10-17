package v1

import "encoding/json"

const FeedbackContract = "bitagent.processors.feedback.v1"

// FeedbackGroundTruth captures the downstream outcome or authoritative truth
// joined back to a Processor's prior prediction and trace.
type FeedbackGroundTruth struct {
	Outcome     string         `json:"outcome"` // applied, rejected_ambiguous, rejected_policy, import_succeeded, import_failed, operator_override, teacher_consensus
	ContentType string         `json:"content_type,omitempty"`
	Extraction  *Extraction    `json:"extraction,omitempty"`
	Language    *LanguageClaim `json:"language,omitempty"`
	Match       *MatchRef      `json:"match,omitempty"`
	Liveness    *LivenessClaim `json:"liveness,omitempty"`
	Excluded    *bool          `json:"excluded,omitempty"`
	Reason      string         `json:"reason,omitempty"`
	Source      string         `json:"source,omitempty"`
}

type FeedbackEvent struct {
	Seq                 int64               `json:"seq"`
	EventID             string              `json:"event_id"`
	ProcessorID         string              `json:"processor_id"`
	ExecutionGeneration int64               `json:"execution_generation"`
	JobID               string              `json:"job_id,omitempty"`
	InfoHash            string              `json:"info_hash"`
	Tier                string              `json:"tier"`
	Input               *Input              `json:"input,omitempty"`
	Prediction          *Result             `json:"prediction,omitempty"`
	Trace               json.RawMessage     `json:"trace,omitempty"`
	GroundTruth         FeedbackGroundTruth `json:"ground_truth"`
	CreatedAt           string              `json:"created_at"`
}

type FeedbackBatch struct {
	Contract  string          `json:"contract"`
	Processor Identity        `json:"processor"`
	Events    []FeedbackEvent `json:"events"`
}

type FeedbackAck struct {
	Contract             string          `json:"contract"`
	Accepted             int             `json:"accepted"`
	UpdatedModelVersion  string          `json:"updated_model_version,omitempty"`
	LearnedState         json.RawMessage `json:"learned_state,omitempty"`
}

type FeedbackPage struct {
	Contract   string          `json:"contract"`
	Items      []FeedbackEvent `json:"items"`
	NextCursor int64           `json:"next_cursor"`
}

type FeedbackWrite struct {
	InfoHash    string              `json:"info_hash"`
	ProcessorID string              `json:"processor_id,omitempty"`
	Tier        string              `json:"tier"`
	GroundTruth FeedbackGroundTruth `json:"ground_truth"`
}

type FeedbackCursorAck struct {
	ProcessorID string `json:"processor_id"`
	ConsumerID  string `json:"consumer_id,omitempty"`
	LastSeq     int64  `json:"last_seq"`
}

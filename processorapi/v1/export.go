package v1

import "encoding/json"

const JobExportContract = "bitagent.processors.job-export.v1"
const MaxExportBody = 4 << 20

// JobExport is an authenticated capture of retained core evidence, not a
// replacement for raw service/provider payload or independent reference archives.
// Opaque source and plan snapshots are bound by their own schema identifiers.
type JobExport struct {
	Contract          string            `json:"contract"`
	CapturedAt        string            `json:"captured_at"`
	Job               JobSummary        `json:"job"`
	Input             Input             `json:"input"`
	SourceSchema      string            `json:"source_schema"`
	SourceSnapshot    json.RawMessage   `json:"source_snapshot"`
	PlanSchema        string            `json:"plan_schema"`
	FrozenPlan        json.RawMessage   `json:"frozen_plan"`
	PreparedJob       *Job              `json:"prepared_job"`
	Results           []AcceptedResult  `json:"results"`
	Attempts          []AttemptRecord   `json:"attempts"`
	LatestApplication json.RawMessage   `json:"latest_application"`
	LatestRestriction json.RawMessage   `json:"latest_restriction"`
	Coverage          map[string]string `json:"coverage"`
}

type AcceptedResult struct {
	Result     Result      `json:"result"`
	Candidates []Candidate `json:"candidates"`
}

type AttemptRecord struct {
	LeaseGeneration     int64    `json:"lease_generation"`
	ProcessorID         string   `json:"processor_id"`
	ExecutionGeneration int64    `json:"execution_generation"`
	Stage               Stage    `json:"stage"`
	StartedAt           string   `json:"started_at"`
	FinishedAt          *string  `json:"finished_at"`
	QueueWaitMS         float64  `json:"queue_wait_ms"`
	InferenceMS         *float64 `json:"inference_ms"`
	Outcome             string   `json:"outcome"`
	Reason              string   `json:"reason"`
}

// DecodeExport keeps strict duplicate/unknown-field checks with the larger,
// separately bounded operator-export limit. Service messages keep MaxBody.
func DecodeExport(raw []byte, into *JobExport) error {
	return decodeBounded(raw, into, MaxExportBody)
}

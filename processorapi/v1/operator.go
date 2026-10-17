package v1

import "encoding/json"

const StatusContract = "bitagent.processors.status.v1"

// RegistryWrite is used by PUT /api/processors/v1/registry/:id. Generation 0
// creates; every update must name the current generation. Configuration must
// satisfy the service manifest schema. AuthEnv references only a dedicated
// BITAGENT_PROCESSOR_TOKEN_* variable; its value is never returned or persisted.
type RegistryWrite struct {
	ExpectedProcessor  *Identity       `json:"expected_processor,omitempty"`
	ExpectedGeneration int64           `json:"expected_generation"`
	Endpoint           string          `json:"endpoint"`
	Mode               string          `json:"mode"`  // shadow or apply
	State              string          `json:"state"` // active, paused, draining
	Priority           int             `json:"priority"`
	Configuration      json.RawMessage `json:"configuration"`
	AuthEnv            string          `json:"auth_env"`
}
type Window struct {
	Start string `json:"start"`
	End   string `json:"end"`
}
type Measurement struct {
	Value             *float64 `json:"value"`
	Unit              string   `json:"unit"`
	Window            Window   `json:"window"`
	Population        string   `json:"population"`
	Source            string   `json:"source"`
	UnavailableReason string   `json:"unavailable_reason"`
}
type ProcessorStatus struct {
	Configuration       json.RawMessage        `json:"configuration"`
	AuthEnv             string                 `json:"auth_env"`
	ExecutionGeneration int64                  `json:"execution_generation"`
	ObservedProcessor   *Identity              `json:"observed_processor"`
	InstanceID          string                 `json:"instance_id"`
	StartedAt           string                 `json:"started_at"`
	ID                  string                 `json:"id"`
	Generation          int64                  `json:"generation"`
	Installed           bool                   `json:"installed"`
	Configured          bool                   `json:"configured"`
	Enabled             bool                   `json:"enabled"`
	Loaded              *bool                  `json:"loaded"`
	State               string                 `json:"state"`
	Mode                string                 `json:"mode"`
	OperatingMode       string                 `json:"operating_mode"`
	Endpoint            string                 `json:"endpoint"`
	Priority            int                    `json:"priority"`
	Contract            string                 `json:"contract"`
	Processor           Identity               `json:"processor"`
	Capabilities        []Stage                `json:"capabilities"`
	Compatibility       string                 `json:"compatibility"`
	Health              string                 `json:"health"`
	Readiness           string                 `json:"readiness"`
	LastCheckedAt       string                 `json:"last_checked_at"`
	LastError           string                 `json:"last_error"`
	Qualification       string                 `json:"qualification"`
	Metrics             map[string]Measurement `json:"metrics"`
	Evaluation          any                    `json:"evaluation"` // null in v1; qualification/uncertainty are not measured accuracy
}
type Status struct {
	Contract                   string                 `json:"contract"`
	ObservedAt                 string                 `json:"observed_at"`
	CoreReadiness              string                 `json:"core_readiness"`
	OptionalProcessorsRequired bool                   `json:"optional_processors_required"`
	PipelineRevision           string                 `json:"pipeline_revision"`
	Processors                 []ProcessorStatus      `json:"processors"`
	Queue                      map[string]Measurement `json:"queue"`
	Limits                     map[string]int         `json:"limits"`
}

// RegistryControl changes only supplied lifecycle fields; all deployment fields
// are retained. It uses the same generation CAS as registration.
type RegistryControl struct {
	ExpectedGeneration int64   `json:"expected_generation"`
	State              *string `json:"state,omitempty"`
	Mode               *string `json:"mode,omitempty"`
}

// JobSummary describes the current step (last step for terminal jobs). Results
// remain bound to immutable identities even after a registration is removed.
type JobSummary struct {
	JobID               string   `json:"job_id"`
	InputRevision       string   `json:"input_revision"`
	PipelineRevision    string   `json:"pipeline_revision"`
	State               string   `json:"state"`
	Stage               Stage    `json:"stage"`
	ProcessorID         string   `json:"processor_id"`
	Processor           Identity `json:"processor"`
	ExecutionGeneration int64    `json:"execution_generation"`
	Attempts            int      `json:"attempts"`
	PreparationFailures int      `json:"preparation_failures,omitempty"`
	CreatedAt           string   `json:"created_at"`
	AvailableAt         string   `json:"available_at"`
	LeaseUntil          *string  `json:"lease_until"`
	Outcome             string   `json:"outcome"`
	Reason              string   `json:"reason"`
}
type JobPage struct {
	Items      []JobSummary `json:"items"`
	NextCursor string       `json:"next_cursor"`
}

package v1

import "encoding/json"

const ReprocessingContract = "bitagent.processors.reprocessing.v1"

type ReprocessWrite struct {
	RequestID                string `json:"request_id"`
	ExpectedInputRevision    string `json:"expected_input_revision"`
	ExpectedPipelineRevision string `json:"expected_pipeline_revision"`
	Reason                   string `json:"reason"`
}

type ReprocessStatus struct {
	Contract                 string  `json:"contract"`
	RequestID                string  `json:"request_id"`
	OriginJobID              string  `json:"origin_job_id"`
	ExpectedPipelineRevision string  `json:"expected_pipeline_revision"`
	Reason                   string  `json:"reason"`
	State                    string  `json:"state"`
	StateReason              string  `json:"state_reason"`
	CoreJobID                string  `json:"core_job_id"`
	ResultJobID              *string `json:"result_job_id"`
	CreatedAt                string  `json:"created_at"`
	UpdatedAt                string  `json:"updated_at"`
}

const MaxReprocessingExportBody = 8 << 20

type ReprocessingAudit struct {
	Contract       string          `json:"contract"`
	Request        ReprocessStatus `json:"request"`
	OriginExport   JobExport       `json:"origin_export"`
	BeforeSnapshot json.RawMessage `json:"before_snapshot"`
	CoreReceipt    json.RawMessage `json:"core_receipt"`
}

func DecodeReprocessingAudit(raw []byte, into *ReprocessingAudit) error {
	return decodeBounded(raw, into, MaxReprocessingExportBody)
}

package v1

const OperationsContract = "bitagent.processors.operations.v1"
const AdmissionContract = "bitagent.processors.admission.v1"
const AdmissionAlgorithm = "sha256-seed-infohash-v1"

// OperationsCapabilities advertises optional operator extensions. Consumers
// must negotiate these independently of the unchanged processor service wire.
type OperationsCapabilities struct {
	Contract string            `json:"contract"`
	Features map[string]string `json:"features"`
}

// AdmissionPolicy is core-owned and applies to the whole ordered pipeline.
// BasisPoints 0 stops optional admission; 10000 includes all otherwise-eligible
// inputs. This does not grant publication authority or change core processing.
type AdmissionPolicy struct {
	Contract      string `json:"contract"`
	Generation    int64  `json:"generation"`
	Algorithm     string `json:"algorithm"`
	Seed          string `json:"seed"`
	BasisPoints   int    `json:"basis_points"`
	PolicyVersion string `json:"policy_version"`
}

type AdmissionWrite struct {
	ExpectedGeneration int64  `json:"expected_generation"`
	Seed               string `json:"seed"`
	BasisPoints        int    `json:"basis_points"`
	PolicyVersion      string `json:"policy_version"`
	Reason             string `json:"reason"`
}

type AdmissionChange struct {
	Policy     AdmissionPolicy `json:"policy"`
	Reason     string          `json:"reason"`
	RecordedAt string          `json:"recorded_at"`
}

type AdmissionHistory struct {
	Items      []AdmissionChange `json:"items"`
	NextBefore int64             `json:"next_before"`
}

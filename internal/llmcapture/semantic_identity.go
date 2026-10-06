package llmcapture

import (
	"crypto/sha256"
	"fmt"
)

// SemanticCaptureIdentity is the complete digest-only request contract used
// for cross-build dispatch equivalence. Build identity remains on evaluation
// captures and their original receipts; it alone cannot authorize another call.
type SemanticCaptureIdentity struct {
	Task                                                                 Task
	CandidateSource                                                      CandidateSource
	SamplingOrigin                                                       SamplingOrigin
	SourceSHA256, GroupSHA256, InputSHA256, PromptSHA256, EndpointSHA256 []byte
	Model, PromptVersion, ContractID                                     string
}

// Key validates and hashes every semantic dimension. A task key or matching
// name alone cannot authorize replay of a different request/candidate roster.
func (s SemanticCaptureIdentity) Key() ([]byte, error) {
	if err := validateTaskSource(s.Task, s.CandidateSource); err != nil {
		return nil, err
	}
	origin, err := normalizedSamplingOrigin(s.Task, s.SamplingOrigin)
	if err != nil {
		return nil, err
	}
	for _, v := range [][]byte{s.SourceSHA256, s.GroupSHA256, s.InputSHA256, s.PromptSHA256, s.EndpointSHA256} {
		if len(v) != sha256.Size {
			return nil, fmt.Errorf("%w: invalid semantic digest", ErrCaptureUnavailable)
		}
	}
	if s.Model == "" || s.PromptVersion == "" || s.ContractID == "" {
		return nil, fmt.Errorf("%w: incomplete semantic contract", ErrCaptureUnavailable)
	}
	key := digestParts([]byte("bitagent-llm-dispatch-semantic-v1"), []byte(s.Task), []byte(s.CandidateSource), []byte(origin),
		s.SourceSHA256, s.GroupSHA256, s.InputSHA256, s.PromptSHA256, s.EndpointSHA256, []byte(s.Model), []byte(s.PromptVersion), []byte(s.ContractID))
	return key[:], nil
}

// SemanticKeyForRequest computes the complete request equivalence digest. It
// does not grant replay/dispatch authority; the controller must prove the same
// identity from admitted capture evidence and retain the original generation.
func SemanticKeyForRequest(req Request) ([]byte, error) {
	modelInput, err := canonicalJSONObject(req.ModelInputJSON)
	if err != nil {
		return nil, err
	}
	taskInput, err := canonicalJSONObject(req.TaskInputJSON)
	if err != nil {
		return nil, err
	}
	if len(req.InfoHash) != 20 || len(req.GroupKey) == 0 {
		return nil, ErrCaptureUnavailable
	}
	source := namespacedDigest("bitagent-llm-evaluation-source-v1", req.InfoHash)
	group := namespacedDigest("bitagent-llm-evaluation-group-v1", req.GroupKey)
	input := digestParts(modelInput, taskInput)
	prompt := sha256.Sum256([]byte(req.SystemPrompt))
	endpoint := sha256.Sum256([]byte(req.Endpoint))
	return (SemanticCaptureIdentity{Task: req.Task, CandidateSource: req.CandidateSource, SamplingOrigin: req.SamplingOrigin,
		SourceSHA256: source[:], GroupSHA256: group[:], InputSHA256: input[:], PromptSHA256: prompt[:], EndpointSHA256: endpoint[:], Model: req.Model, PromptVersion: req.PromptVersion, ContractID: req.ContractID}).Key()
}

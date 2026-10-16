package llmmatch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

const matcherChatResponseLimit = 128 << 10

var (
	ErrMatcherChatResponseRead     = errors.New("read matcher chat response")
	ErrMatcherChatResponseTooLarge = fmt.Errorf(
		"%w: body exceeds %d bytes",
		ErrMatcherChatResponseRead,
		matcherChatResponseLimit,
	)
	ErrMatcherChatHTTPStatus = errors.New("matcher chat response status is not 200")
	ErrMatcherChatEnvelope   = errors.New("decode matcher chat response envelope")
	ErrMatcherChatNoChoices  = errors.New("no choices in chat response")
)

// ReadMatcherChatResponse is the live matcher's complete HTTP response
// boundary. Production-fidelity evaluation calls the same function so status,
// body-limit, envelope, refusal, and empty-content behavior cannot drift.
//
// Missing optional finish/role metadata is accepted for compatible providers.
// Explicit refusal, errors, incomplete output and ambiguous choices cannot
// authorize a match. Empty content is rejected by the downstream model decoder.
func ReadMatcherChatResponse(
	statusCode int,
	body io.Reader,
) (raw []byte, content []byte, err error) {
	if body == nil {
		return nil, nil, ErrMatcherChatResponseRead
	}
	// Read one byte beyond the retained bound so an oversized response cannot
	// masquerade as a complete valid JSON document when its bounded prefix ends
	// in whitespace. The retained prefix is useful for bounded diagnostics, but
	// its digest is deliberately not presented as the digest of the full body.
	raw, err = io.ReadAll(io.LimitReader(body, matcherChatResponseLimit+1))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrMatcherChatResponseRead, err)
	}
	if len(raw) > matcherChatResponseLimit {
		return raw[:matcherChatResponseLimit], nil, ErrMatcherChatResponseTooLarge
	}
	if statusCode != http.StatusOK {
		return raw, nil, fmt.Errorf("%w: %d", ErrMatcherChatHTTPStatus, statusCode)
	}
	if err := validateMatcherJSON(raw); err != nil {
		return raw, nil, fmt.Errorf("%w: %v", ErrMatcherChatEnvelope, err)
	}
	// Accounting is decoded separately by recordUsage. A malformed optional
	// usage field makes spend partial; it must not veto otherwise valid content.
	var outer struct {
		Error   json.RawMessage `json:"error"`
		Choices []struct {
			FinishReason json.RawMessage `json:"finish_reason"`
			Message      struct {
				Content string          `json:"content"`
				Role    json.RawMessage `json:"role"`
				Refusal json.RawMessage `json:"refusal"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &outer); err != nil {
		return raw, nil, fmt.Errorf("%w: %v", ErrMatcherChatEnvelope, err)
	}
	if len(outer.Choices) == 0 {
		return raw, nil, ErrMatcherChatNoChoices
	}
	if len(outer.Choices) != 1 {
		return raw, nil, fmt.Errorf("%w: expected exactly one choice", ErrMatcherChatEnvelope)
	}
	choice := outer.Choices[0]
	if matcherJSONValuePresent(outer.Error) || matcherRefusalRejected(choice.Message.Refusal) ||
		!matcherOptionalStringEquals(choice.FinishReason, "stop") ||
		!matcherOptionalStringEquals(choice.Message.Role, "assistant") {
		return raw, nil, fmt.Errorf("%w: incomplete, refused or errored output", ErrMatcherChatEnvelope)
	}
	return raw, []byte(outer.Choices[0].Message.Content), nil
}

func matcherJSONValuePresent(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func matcherRefusalRejected(raw json.RawMessage) bool {
	if !matcherJSONValuePresent(raw) {
		return false
	}
	var value string
	return json.Unmarshal(raw, &value) != nil || value != ""
}

func matcherOptionalStringEquals(raw json.RawMessage, expected string) bool {
	if len(raw) == 0 {
		return true
	}
	var value *string
	return json.Unmarshal(raw, &value) == nil && value != nil && *value == expected
}

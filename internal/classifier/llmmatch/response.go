package llmmatch

import (
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
// The matcher intentionally accepts the first choice's content exactly as
// returned. Refusal and empty-content handling belongs to the downstream model
// object decoder, matching the live action path.
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
	// Accounting is decoded separately by recordUsage. A malformed optional
	// usage field makes spend partial; it must not veto otherwise valid content.
	var outer struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &outer); err != nil {
		return raw, nil, fmt.Errorf("%w: %v", ErrMatcherChatEnvelope, err)
	}
	if len(outer.Choices) == 0 {
		return raw, nil, ErrMatcherChatNoChoices
	}
	return raw, []byte(outer.Choices[0].Message.Content), nil
}

package contentfilter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spencercnorton/bitagent/internal/llmcapture"
)

// EvaluationParseHTTPVerdict decodes retained HTTP response bytes under their
// exact captured request contract. It performs no I/O. The runtime clients use
// the same decoder before applying a verdict; HTTP status, privacy, request and
// response identity, policy cutoffs and live mode must still be checked by the
// caller. Unknown contracts, duplicate fields, ambiguous or explicitly
// incomplete/refused envelopes and malformed verdicts fail open with an error.
func EvaluationParseHTTPVerdict(raw []byte, contractID string) (LLMVerdict, error) {
	if len(raw) == 0 || len(raw) > llmcapture.MaxResultBodyBytes {
		return LLMVerdict{}, errors.New("contentfilter response exceeds bounded decode size")
	}
	if err := validateHTTPJSON(raw); err != nil {
		return LLMVerdict{}, err
	}
	var envelope struct {
		Status     string          `json:"status"`
		Error      json.RawMessage `json:"error"`
		Incomplete json.RawMessage `json:"incomplete_details"`
		Choices    []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role    string `json:"role"`
				Content string `json:"content"`
				Refusal string `json:"refusal"`
			} `json:"message"`
		} `json:"choices"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		Done       *bool  `json:"done"`
		DoneReason string `json:"done_reason"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return LLMVerdict{}, errors.New("invalid contentfilter HTTP envelope")
	}
	if presentJSONValue(envelope.Error) {
		return LLMVerdict{}, errors.New("contentfilter provider error envelope")
	}
	var text string
	switch contractID {
	case "contentfilter-responses-model-input-v1":
		if envelope.Status != "" && envelope.Status != "completed" || presentJSONValue(envelope.Incomplete) {
			return LLMVerdict{}, errors.New("incomplete contentfilter Responses envelope")
		}
		var err error
		text, err = extractOutputText(raw)
		if err != nil {
			return LLMVerdict{}, err
		}
	case "contentfilter-chat-model-input-v1", "contentfilter-chat-model-input-v2-openrouter", "contentfilter-chat-model-input-v3-openai-data-sharing":
		if len(envelope.Choices) != 1 {
			return LLMVerdict{}, errors.New("contentfilter Chat envelope requires exactly one choice")
		}
		choice := envelope.Choices[0]
		if choice.FinishReason != "" && choice.FinishReason != "stop" || choice.Message.Refusal != "" ||
			choice.Message.Role != "" && choice.Message.Role != "assistant" {
			return LLMVerdict{}, errors.New("incomplete or refused contentfilter Chat envelope")
		}
		text = choice.Message.Content
	case "contentfilter-ollama-model-input-v1":
		if envelope.Done != nil && !*envelope.Done || envelope.DoneReason != "" && envelope.DoneReason != "stop" ||
			envelope.Message.Role != "" && envelope.Message.Role != "assistant" {
			return LLMVerdict{}, errors.New("incomplete contentfilter Ollama envelope")
		}
		text = envelope.Message.Content
	default:
		return LLMVerdict{}, errors.New("unsupported contentfilter response contract")
	}
	if strings.TrimSpace(text) == "" {
		return LLMVerdict{}, errors.New("contentfilter HTTP envelope has no verdict text")
	}
	return parseLLMVerdict(text)
}

func presentJSONValue(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// Provider envelopes have extensible metadata, so unknown outer fields remain
// allowed. Duplicate fields and trailing documents have no unambiguous meaning
// and must be rejected before encoding/json's last-key-wins decoding.
func validateHTTPJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var consume func(int) error
	consume = func(depth int) error {
		if depth > 32 {
			return errors.New("contentfilter envelope nesting exceeds limit")
		}
		token, err := decoder.Token()
		if err != nil {
			return errors.New("invalid contentfilter envelope JSON")
		}
		delim, container := token.(json.Delim)
		if !container {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return errors.New("invalid contentfilter envelope object")
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("duplicate contentfilter envelope field")
				}
				seen[name] = true
				if err := consume(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := consume(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid contentfilter envelope delimiter")
		}
		closing, err := decoder.Token()
		if err != nil || delim == '{' && closing != json.Delim('}') || delim == '[' && closing != json.Delim(']') {
			return errors.New("invalid contentfilter envelope closing delimiter")
		}
		return nil
	}
	if err := consume(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("trailing contentfilter envelope data")
	}
	return nil
}

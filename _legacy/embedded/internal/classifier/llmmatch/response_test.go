package llmmatch

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadMatcherChatResponseMatchesLiveBoundary(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		wantContent string
		wantErr     error
	}{
		{
			name:        "exact 200",
			status:      http.StatusOK,
			body:        `{"choices":[{"message":{"content":"{\"title\":\"Example\"}"}}]}`,
			wantContent: `{"title":"Example"}`,
		},
		{
			name:        "malformed usage cannot veto valid content",
			status:      http.StatusOK,
			body:        `{"choices":[{"message":{"content":"{}"}}],"usage":{"prompt_tokens":"unknown"}}`,
			wantContent: `{}`,
		},
		{
			name:    "other 2xx rejected",
			status:  http.StatusCreated,
			body:    `{"choices":[{"message":{"content":"{}"}}]}`,
			wantErr: ErrMatcherChatHTTPStatus,
		},
		{
			name:    "refusal vetoes existing content",
			status:  http.StatusOK,
			body:    `{"choices":[{"message":{"content":"{}","refusal":"policy"}}]}`,
			wantErr: ErrMatcherChatEnvelope,
		},
		{
			name:        "empty refusal is compatible no-refusal metadata",
			status:      http.StatusOK,
			body:        `{"choices":[{"message":{"content":"{}","refusal":""}}]}`,
			wantContent: `{}`,
		},
		{
			name:        "explicit complete output with null refusal and error",
			status:      http.StatusOK,
			body:        `{"error":null,"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{}","refusal":null}}]}`,
			wantContent: `{}`,
		},
		{
			name:        "empty content reaches model decoder",
			status:      http.StatusOK,
			body:        `{"choices":[{"message":{"content":""}}]}`,
			wantContent: "",
		},
		{
			name:        "whitespace content reaches model decoder",
			status:      http.StatusOK,
			body:        `{"choices":[{"message":{"content":"  \n"}}]}`,
			wantContent: "  \n",
		},
		{
			name:    "missing choices rejected",
			status:  http.StatusOK,
			body:    `{"choices":[]}`,
			wantErr: ErrMatcherChatNoChoices,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, content, err := ReadMatcherChatResponse(
				test.status,
				strings.NewReader(test.body),
			)
			require.Equal(t, test.body, string(raw))
			if test.wantErr != nil {
				require.Error(t, err)
				require.True(t, errors.Is(err, test.wantErr))
				require.Nil(t, content)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.wantContent, string(content))
		})
	}
}

func TestReadMatcherChatResponseAcceptsExactSharedBoundary(t *testing.T) {
	body := `{"choices":[{"message":{"content":"ok"}}]}`
	body += strings.Repeat(" ", matcherChatResponseLimit-len(body))
	raw, content, err := ReadMatcherChatResponse(
		http.StatusOK,
		strings.NewReader(body),
	)
	require.NoError(t, err)
	require.Len(t, raw, matcherChatResponseLimit)
	require.Equal(t, "ok", string(content))
}

func TestReadMatcherChatResponseRejectsOversizedValidPrefixAtSharedBoundary(t *testing.T) {
	body := `{"choices":[{"message":{"content":"ok"}}]}`
	body += strings.Repeat(" ", matcherChatResponseLimit-len(body)+1)
	raw, content, err := ReadMatcherChatResponse(
		http.StatusOK,
		strings.NewReader(body),
	)
	require.Len(t, raw, matcherChatResponseLimit)
	require.Equal(t, body[:matcherChatResponseLimit], string(raw))
	require.Nil(t, content)
	require.ErrorIs(t, err, ErrMatcherChatResponseTooLarge)
	require.ErrorIs(t, err, ErrMatcherChatResponseRead)
}

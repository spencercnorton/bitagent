package tmdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestRequesterNeverLogsAPIKey(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // every attempt fails to connect: a *url.Error whose message is the full request URL

	core, logs := observer.New(zapcore.DebugLevel)
	client := newRestyClient(Config{BaseURL: srv.URL, APIKey: "secret123"}, zap.New(core).Sugar()).
		SetDebug(true).
		SetRetryWaitTime(time.Millisecond).
		SetRetryMaxWaitTime(time.Millisecond)

	_, err := requester{resty: client}.Request(context.Background(), "/authentication", nil, nil)
	require.Error(t, err)

	lines := []string{err.Error()}

	for _, level := range []zapcore.Level{zapcore.DebugLevel, zapcore.WarnLevel, zapcore.ErrorLevel} {
		entries := logs.FilterLevelExact(level).All()
		require.NotEmpty(t, entries, "resty logged nothing at %s", level)

		for _, e := range entries {
			lines = append(lines, e.Message)
		}
	}

	for _, line := range lines {
		assert.Contains(t, line, "api_key=REDACTED")
		assert.NotContains(t, line, "secret123")
	}
}

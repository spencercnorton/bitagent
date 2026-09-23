package tmdb

import (
	"context"
	"fmt"
	"regexp"

	"github.com/go-resty/resty/v2"
	"go.uber.org/zap"
)

// apiKeyParam matches the api_key query parameter resty adds to every request URL.
var apiKeyParam = regexp.MustCompile(`api_key=[^&"\s]+`)

func redactAPIKey(s string) string {
	return apiKeyParam.ReplaceAllString(s, "api_key=REDACTED")
}

// restyLogger is the logger resty gets: resty logs each failed attempt's *url.Error,
// whose message is the full request URL, api_key included.
type restyLogger struct {
	logger *zap.SugaredLogger
}

func (l restyLogger) Errorf(format string, v ...any) {
	l.logger.Error(redactAPIKey(fmt.Sprintf(format, v...)))
}

func (l restyLogger) Warnf(format string, v ...any) {
	l.logger.Warn(redactAPIKey(fmt.Sprintf(format, v...)))
}

func (l restyLogger) Debugf(format string, v ...any) {
	l.logger.Debug(redactAPIKey(fmt.Sprintf(format, v...)))
}

type requesterLogger struct {
	requester Requester
	logger    *zap.SugaredLogger
}

func (r requesterLogger) Request(
	ctx context.Context,
	path string,
	queryParams map[string]string,
	result any,
) (*resty.Response, error) {
	res, err := r.requester.Request(ctx, path, queryParams, result)
	kvs := []interface{}{"path", path, "queryParams", queryParams}

	if res != nil {
		kvs = append(kvs, "status", res.Status(), "trace", res.Request.TraceInfo())
	}

	if err == nil {
		r.logger.Debugw("request succeeded", kvs...)
	} else {
		kvs = append(kvs, "error", err)
		r.logger.Errorw("request failed", kvs...)
	}

	return res, err
}

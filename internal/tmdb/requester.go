package tmdb

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/go-resty/resty/v2"
)

type Requester interface {
	Request(ctx context.Context, path string, queryParams map[string]string, result any) (*resty.Response, error)
}

type requester struct {
	resty *resty.Client
}

func (r requester) Request(
	ctx context.Context,
	path string,
	queryParams map[string]string,
	result any,
) (*resty.Response, error) {
	res, err := r.resty.R().
		SetContext(ctx).
		SetQueryParams(queryParams).
		SetResult(&result).
		Get(path)
	if err == nil && !res.IsSuccess() {
		switch res.StatusCode() {
		case http.StatusUnauthorized:
			err = ErrUnauthorized
		case http.StatusNotFound:
			err = ErrNotFound
		default:
			err = newError(res.Status())
		}
	}

	// A transport failure is a *url.Error whose message is the full request URL: redact before anyone logs it.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		urlErr.URL = redactAPIKey(urlErr.URL)
	}

	return res, err
}

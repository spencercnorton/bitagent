package processors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
	"github.com/xeipuuv/gojsonschema"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var authName = regexp.MustCompile(`^BITAGENT_PROCESSOR_TOKEN_[A-Z0-9_]{1,64}$`)

type Transport struct {
	client              *http.Client
	CredentialDirectory string
}

func NewTransport(timeout time.Duration) *Transport {
	return &Transport{client: &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect_forbidden") }, Transport: &http.Transport{Proxy: nil, MaxConnsPerHost: 16, MaxIdleConns: 16, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: timeout, DisableCompression: true}}}
}
func validateEndpoint(endpoint, auth string) error {
	u, e := url.Parse(endpoint)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") || len(endpoint) > 1024 {
		return errors.New("invalid_endpoint")
	}
	if auth != "" && !authName.MatchString(auth) {
		return errors.New("invalid_auth_reference")
	}
	return nil
}
func (t *Transport) call(ctx context.Context, endpoint, auth, path string, body any, out any) error {
	if e := validateEndpoint(endpoint, auth); e != nil {
		return e
	}
	var raw []byte
	var e error
	method := http.MethodGet
	if body != nil {
		method = http.MethodPost
		raw, e = json.Marshal(body)
		if e != nil {
			return e
		}
		if len(raw) > v1.MaxBody {
			return errors.New("request_too_large")
		}
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(endpoint, "/")+path, bytes.NewReader(raw))
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		secret := os.Getenv(auth)
		if secret == "" && t.CredentialDirectory != "" {
			file, err := os.Open(filepath.Join(t.CredentialDirectory, auth))
			if err == nil {
				data, readErr := io.ReadAll(io.LimitReader(file, 4097))
				_ = file.Close()
				if readErr == nil && len(data) <= 4096 {
					secret = strings.TrimSpace(string(data))
				}
			}
		}
		if secret == "" {
			return errors.New("transport_credential_unavailable")
		}
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	res, e := t.client.Do(req)
	if e != nil {
		return errors.New("transport_failed")
	}
	defer res.Body.Close()
	media, _, mediaErr := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if mediaErr != nil || media != "application/json" {
		return errors.New("invalid_content_type")
	}
	if res.StatusCode != http.StatusOK {
		return errors.New("service_unavailable")
	}
	raw, e = io.ReadAll(io.LimitReader(res.Body, v1.MaxBody+1))
	if e != nil {
		return errors.New("response_read_failed")
	}
	return v1.Decode(raw, out)
}
func (t *Transport) Inspect(ctx context.Context, w v1.RegistryWrite) (v1.Manifest, v1.ServiceStatus, error) {
	var m v1.Manifest
	var s v1.ServiceStatus
	if e := t.call(ctx, w.Endpoint, w.AuthEnv, "/v1/manifest", nil, &m); e != nil {
		return m, s, e
	}
	if e := m.Validate(); e != nil {
		return m, s, e
	}
	if len(w.Configuration) == 0 || len(w.Configuration) > 4096 {
		return m, s, errors.New("invalid_configuration")
	}
	var config map[string]any
	if e := v1.Decode(w.Configuration, &config); e != nil {
		return m, s, e
	}
	if config == nil || containsCredentials(config) {
		return m, s, errors.New("configuration_contains_credentials")
	}
	checked, e := gojsonschema.Validate(gojsonschema.NewBytesLoader(m.ConfigurationSchema), gojsonschema.NewGoLoader(config))
	if e != nil || !checked.Valid() {
		return m, s, errors.New("configuration_schema_mismatch")
	}
	s, e = t.Status(ctx, w.Endpoint, w.AuthEnv)
	if e != nil {
		return m, s, e
	}
	if s.Contract != v1.Contract || s.Processor != m.Processor {
		return m, s, errors.New("service_identity_mismatch")
	}
	if w.State == "active" && s.State != "ready" {
		return m, s, errors.New("service_not_ready")
	}
	return m, s, nil
}
func (t *Transport) Status(ctx context.Context, endpoint, auth string) (v1.ServiceStatus, error) {
	var s v1.ServiceStatus
	e := t.call(ctx, endpoint, auth, "/v1/status", nil, &s)
	if e == nil {
		e = s.Validate()
	}
	return s, e
}
func (t *Transport) Process(ctx context.Context, endpoint, auth string, j v1.Job) (v1.Result, error) {
	var r v1.Result
	if e := j.Validate(); e != nil {
		return r, e
	}
	if e := t.call(ctx, endpoint, auth, "/v1/process", j, &r); e != nil {
		return r, e
	}
	return r, r.Validate(j)
}

func containsCredentials(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			key = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "_", ""), "-", ""))
			if strings.Contains(key, "password") || strings.Contains(key, "secret") || strings.Contains(key, "apikey") || strings.Contains(key, "credential") || key == "authorization" || key == "token" || key == "accesstoken" || key == "bearertoken" {
				return true
			}
			if containsCredentials(item) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if containsCredentials(item) {
				return true
			}
		}
	}
	return false
}

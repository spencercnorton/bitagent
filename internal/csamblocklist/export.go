package csamblocklist

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spencercnorton/bitagent/internal/protocol"
	"go.uber.org/zap"
)

// Exporter is the self-export sink for CSAM observations from this
// instance's post-fetch classifier. When the CEL workflow's
// `keywords.banned` rule fires, the processor calls Record(); the
// exporter checks the title against the banned keyword regex
// (defense-in-depth: ensures the export is only triggered for
// banned-list deletes, not for unrelated delete actions like
// flags.delete_xxx) and, if matched, double-hashes the infohash and
// appends to the local JSONL log.
//
// Optionally POSTs the same record to a configured upstream
// collection endpoint so other operators see the observation.
//
// All public methods are safe for concurrent use.
type Exporter interface {
	// Record is called by the processor when the CEL classifier emits
	// an `ErrDeleteTorrent`. The exporter independently verifies the
	// title matches the CSAM banned-keyword regex before writing
	// anything (so other delete reasons — `delete_xxx`,
	// `delete_content_types` — don't taint the export).
	//
	// Errors are logged + counted but never propagated. The processor's
	// path through this hook is non-fatal.
	Record(ctx context.Context, infoHash protocol.ID, title string, filePaths []string)

	// Close flushes any pending state. Safe to call multiple times.
	Close() error
}

// NewExporter constructs an Exporter per the operator's config. When
// ExportEnabled=false, returns a NoOp exporter that silently drops
// every Record call.
//
// If ExportUpstreamURL is set + ExportUpstreamAuthHeader is set + the
// URL is not https://, the constructor scrubs the upstream URL to
// avoid sending the Authorization token in cleartext (review finding
// finding 64bf02b7). Loopback URLs (localhost / 127.0.0.1 / ::1)
// are exempt to support local-stack testing. The scrub logs a
// warning so the operator notices.
func NewExporter(cfg Config, logger *zap.SugaredLogger, metrics *Metrics) Exporter {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	if !cfg.ExportEnabled {
		return noOpExporter{}
	}
	if cfg.ExportUpstreamURL != "" && cfg.ExportUpstreamAuthHeader != "" {
		if !isSafeUpstreamURL(cfg.ExportUpstreamURL) {
			logger.Warnw(
				"refusing to attach Authorization header to non-https / non-loopback ExportUpstreamURL — disabling upstream POST",
				"url", cfg.ExportUpstreamURL,
			)
			cfg.ExportUpstreamURL = ""
		}
	}
	httpClient := &http.Client{Timeout: cfg.ExportUpstreamTimeout}
	if cfg.ExportUpstreamTimeout <= 0 {
		httpClient.Timeout = 10 * time.Second
	}
	return &exporter{
		cfg:        cfg,
		logger:     logger,
		metrics:    metrics,
		httpClient: httpClient,
	}
}

// isSafeUpstreamURL returns true iff the URL uses HTTPS, OR uses HTTP
// against loopback (localhost / 127.0.0.1 / ::1) — the latter exempt
// for local-stack testing where TLS would require self-signed certs.
func isSafeUpstreamURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme == "http" {
		host := u.Hostname()
		switch host {
		case "localhost", "127.0.0.1", "::1":
			return true
		}
	}
	return false
}

type exporter struct {
	cfg     Config
	logger  *zap.SugaredLogger
	metrics *Metrics

	mu   sync.Mutex
	file *os.File
	// closed uses atomic.Bool so the Record fast-path can read it
	// without acquiring the mutex (Jeeves finding c0885ac0).
	// Close still acquires the mutex to coordinate with appendLocal.
	closed atomic.Bool

	httpClient *http.Client
}

// observationRecord is the JSONL line and POST body shape. The
// infohash is NEVER included — only its double-hash. Keeps the
// observation log itself non-useful as a CSAM directory if it leaks.
type observationRecord struct {
	Timestamp  string `json:"ts"`
	DoubleHash string `json:"double_hash"`
	Reason     string `json:"reason"`
	// Note: title and file paths are also intentionally omitted.
	// The post-classify pipeline already logs a redacted summary
	// elsewhere; this record is purely the queryable fingerprint.
}

func (e *exporter) Record(ctx context.Context, infoHash protocol.ID, title string, filePaths []string) {
	if e.closed.Load() {
		return
	}
	if !matchesBannedKeyword(title, filePaths) {
		if e.metrics != nil {
			e.metrics.ExportTotal.WithLabelValues("skipped_no_match").Inc()
		}
		return
	}

	rec := observationRecord{
		Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
		DoubleHash: DoubleHashHex(infoHash),
		Reason:     "banned_keyword",
	}

	// Local JSONL append.
	if err := e.appendLocal(rec); err != nil {
		if e.metrics != nil {
			e.metrics.ExportTotal.WithLabelValues("local_err").Inc()
		}
		e.logger.Warnw("csam export local-write failed", "err", err)
	} else {
		if e.metrics != nil {
			e.metrics.ExportTotal.WithLabelValues("local_ok").Inc()
		}
	}

	// Optional upstream POST.
	if e.cfg.ExportUpstreamURL != "" {
		if err := e.postUpstream(ctx, rec); err != nil {
			if e.metrics != nil {
				e.metrics.ExportTotal.WithLabelValues("upstream_err").Inc()
			}
			e.logger.Warnw("csam export upstream POST failed",
				"url", e.cfg.ExportUpstreamURL, "err", err)
		} else {
			if e.metrics != nil {
				e.metrics.ExportTotal.WithLabelValues("upstream_ok").Inc()
			}
		}
	}
}

func (e *exporter) appendLocal(rec observationRecord) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed.Load() {
		return errors.New("csam exporter closed")
	}
	if e.file == nil {
		path := e.cfg.ExportFilePath
		if path == "" {
			path = "data/csam-double-hashes.jsonl"
		}
		dir := filepath.Dir(path)
		if dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", dir, err)
			}
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		e.file = f
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	line = append(line, '\n')
	if _, err := e.file.Write(line); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

func (e *exporter) postUpstream(ctx context.Context, rec observationRecord) error {
	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.ExportUpstreamURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "bitagent-csam-export/1")
	if e.cfg.ExportUpstreamAuthHeader != "" {
		req.Header.Set("Authorization", e.cfg.ExportUpstreamAuthHeader)
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	return nil
}

func (e *exporter) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed.Load() || e.file == nil {
		e.closed.Store(true)
		return nil
	}
	err := e.file.Close()
	e.file = nil
	e.closed.Store(true)
	return err
}

type noOpExporter struct{}

func (noOpExporter) Record(_ context.Context, _ protocol.ID, _ string, _ []string) {}
func (noOpExporter) Close() error                                                  { return nil }

// bannedKeywordRegex matches the same keyword set as `keywords.banned`
// in internal/classifier/classifier.core.yml. This MUST stay in sync —
// see TestBannedKeywordParity in classifier package for the pin.
//
// Format note: the YAML uses glob-ish patterns (`pa?edo(fil*|phil*)?`,
// `kidd(y*|ie*) ?porn`, `(#|10|11|12|13|14|15|16|17) ?y ?o`). Those
// are CEL-classifier's own glob format. The Go regex below is the
// nearest-equivalent expansion. Whole-word semantics use \b on word
// boundaries; the multi-token patterns (e.g. "child ?porn", "young
// ?video ?models") allow optional spaces.
var bannedKeywordRegex = func() *regexp.Regexp {
	tokens := []string{
		`pa?edo(?:fil[a-z]*|phil[a-z]*)?`, // pedo / paedo / pedophile / paedophile / paedofilia
		`preteen`,
		`pthc`,
		`ptsc`,
		`lsbar`,
		`lsm`,
		`underage`,
		`hebefilia`,
		`opva`,
		`child[\s_-]?porn[a-z]*`,
		`child[\s_-]?lover[a-z]*`,
		`porno[\s_-]?child[a-z]*`,
		`kidd(?:y[a-z]*|ie[a-z]*)[\s_-]?porn`,
		`young[\s_-]?video[\s_-]?models`,
		`childfugga`,
		`kinderkutje`,
		`yvm`,
		`(?:#|1[0-7])[\s_-]?y[\s_-]?o`, // age qualifiers: #yo, 10yo..17yo, with optional separators
	}
	pat := `(?i)\b(?:` + strings.Join(tokens, "|") + `)\b`
	return regexp.MustCompile(pat)
}()

// MatchesBannedKeyword is the exported form of matchesBannedKeyword, for
// callers outside this package that must exclude CSAM observations from a
// non-CSAM code path. The processor uses it to decide whether a
// classifier-delete may have its torrent name recorded in the verdict
// ledger: a banned-keyword delete never may.
//
// Deliberately the SAME predicate rather than a second copy — the regex is
// parity-pinned against keywords.banned by TestBannedKeywordParity, and a
// divergent second implementation is exactly how a CSAM name would leak into
// an audit table.
func MatchesBannedKeyword(title string, filePaths []string) bool {
	return matchesBannedKeyword(title, filePaths)
}

// matchesBannedKeyword returns true iff the title or any file path
// matches the CSAM banned-keyword regex. Mirrors the CEL workflow's
// first step:
//
//	([torrent.baseName] + torrent.files.map(f, f.basePath)).join(' ').matches(keywords.banned)
//
// The Go test joins title + paths with a space and runs the unified
// regex once. Equivalent semantics; cheaper than per-token loop.
func matchesBannedKeyword(title string, filePaths []string) bool {
	if title == "" && len(filePaths) == 0 {
		return false
	}
	var sb strings.Builder
	sb.WriteString(title)
	for _, p := range filePaths {
		sb.WriteByte(' ')
		sb.WriteString(p)
	}
	return bannedKeywordRegex.MatchString(sb.String())
}

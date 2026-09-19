package csamblocklist

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// fetchFeed retrieves and parses one community feed. Returns the
// (deduped) list of double-hash entries plus the first parse error
// encountered (the caller logs but does not abort on per-feed errors).
//
// Wire format (lowercase hex, one per line):
//
//	# CSAM blocklist feed
//	# This is a comment
//	de47c9b27eb8d300dbb5f2c353e632c393262cf06340c4fa7f1b40c4cbd36f90
//	0a1b2c3d4e5f...        (64 chars, lowercase hex)
//
// Comment lines start with '#' (after optional whitespace). Blank
// lines are skipped. Anything else must be a 64-char lowercase hex
// double-hash; non-conforming lines increment a parse-error count
// but the rest of the feed is still ingested.
func (s *service) fetchFeed(ctx context.Context, feedURL string) ([][DoubleHashLen]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "bitagent-csam-blocklist/1")
	req.Header.Set("Accept", "text/plain")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	limit := s.cfg.FeedMaxBytes
	if limit <= 0 {
		limit = 64 * 1024 * 1024
	}
	body := io.LimitReader(resp.Body, limit+1)

	entries, parseErrs, err := parseFeed(body, limit)
	if err != nil {
		return nil, err
	}
	if parseErrs > 0 {
		// Log via service logger; non-fatal.
		s.logger.Warnw("csam blocklist feed had parse errors",
			"url", feedURL, "parse_errors", parseErrs, "valid_entries", len(entries))
	}
	return entries, nil
}

// parseFeed reads a feed body and returns parsed double-hashes. The
// first return value is deduplicated. The second is the count of
// non-conforming non-blank non-comment lines (parse errors). The
// third is non-nil only on transport-level read errors or when the
// body exceeds maxBytes.
//
// Pulled out of fetchFeed so it can be unit-tested without an HTTP
// stub.
func parseFeed(r io.Reader, maxBytes int64) ([][DoubleHashLen]byte, int, error) {
	scanner := bufio.NewScanner(r)
	// Allow long-line buffers — but each line is bounded by the
	// 64-char hex format + newline + small slack. 1024 is generous.
	scanner.Buffer(make([]byte, 0, 256), 1024)

	seen := make(map[[DoubleHashLen]byte]struct{}, 256)
	out := make([][DoubleHashLen]byte, 0, 256)
	parseErrs := 0
	var totalBytes int64

	for scanner.Scan() {
		line := scanner.Bytes()
		totalBytes += int64(len(line)) + 1 // +1 for the newline scanner stripped
		if totalBytes > maxBytes {
			return nil, 0, errors.New("feed exceeds max size")
		}
		s := strings.TrimSpace(string(line))
		// Strip inline `#` comments — the docs promise "anything
		// after # is ignored to end-of-line" (Jeeves finding
		// 74837a0f). Without this, an annotated entry like
		// `<64hex> # source` was counted as a parse error and
		// dropped, silently reducing pre-fetch protection for
		// curated community feeds.
		if i := strings.IndexByte(s, '#'); i >= 0 {
			s = strings.TrimSpace(s[:i])
		}
		if s == "" {
			continue
		}
		dh, err := ParseDoubleHashHex(s)
		if err != nil {
			parseErrs++
			continue
		}
		if _, dup := seen[dh]; dup {
			continue
		}
		seen[dh] = struct{}{}
		out = append(out, dh)
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, fmt.Errorf("scan feed: %w", err)
	}
	return out, parseErrs, nil
}

// feedLabel turns a feed URL into a low-cardinality Prometheus label.
// Drops scheme, query, fragment; keeps host + path. Falls back to
// "invalid" for malformed URLs.
func feedLabel(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "invalid"
	}
	if u.Host == "" {
		return "invalid"
	}
	return u.Host + u.Path
}

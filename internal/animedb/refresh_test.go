package animedb

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

func TestMaybeGunzip(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte("hello world")); err != nil {
		t.Fatal(err)
	}
	_ = zw.Close()

	r, err := maybeGunzip(buf.Bytes())
	if err != nil {
		t.Fatalf("maybeGunzip(gzip): %v", err)
	}
	got, _ := io.ReadAll(r)
	if string(got) != "hello world" {
		t.Errorf("gunzip = %q, want %q", got, "hello world")
	}

	// Plain (non-gzip) bytes pass through unchanged.
	r2, err := maybeGunzip([]byte("plain text"))
	if err != nil {
		t.Fatalf("maybeGunzip(plain): %v", err)
	}
	got2, _ := io.ReadAll(r2)
	if string(got2) != "plain text" {
		t.Errorf("passthrough = %q, want %q", got2, "plain text")
	}
}

func TestLanguageSet(t *testing.T) {
	t.Parallel()
	if languageSet(nil) != nil {
		t.Error("empty langs must yield a nil set (keep all)")
	}
	s := languageSet([]string{"x-jat", "en"})
	if !s["x-jat"] || !s["en"] || s["fr"] {
		t.Errorf("languageSet membership wrong: %v", s)
	}
}

// End-to-end dry-run: serve the sample XML and a gzipped titles dump over HTTP,
// then run Refresh(dry-run) and assert it parses, joins and builds without
// touching a store.
func TestRefreshDryRunEndToEnd(t *testing.T) {
	t.Parallel()
	xmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, sampleAnimeList)
	}))
	defer xmlSrv.Close()

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(sampleTitles))
	_ = zw.Close()
	datSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(gz.Bytes())
	}))
	defer datSrv.Close()

	cfg := NewDefaultConfig()
	cfg.AnimeListUrl = xmlSrv.URL
	cfg.AnidbTitlesUrl = datSrv.URL
	cfg.CacheDir = t.TempDir()
	cfg.Languages = []string{"x-jat", "en", "ja"}

	runner := NewRunner(cfg, nil /*store*/, nil /*resolver*/, nil /*metrics*/, zap.NewNop().Sugar())
	stats, err := runner.Refresh(context.Background(), false /*write*/)
	if err != nil {
		t.Fatalf("Refresh dry-run: %v", err)
	}
	if stats.Mappings != 2 {
		t.Errorf("mappings = %d, want 2", stats.Mappings)
	}
	if stats.Aliases == 0 {
		t.Errorf("expected non-zero aliases built, got 0")
	}
	if stats.Wrote || stats.Rows != 0 {
		t.Errorf("dry-run must not write: %+v", stats)
	}
}

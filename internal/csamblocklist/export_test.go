package csamblocklist

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/spencercnorton/bitagent/internal/protocol"
)

func TestMatchesBannedKeyword(t *testing.T) {
	mustMatch := []string{
		"some.preteen.thing",
		"PTHC.archive.zip",
		"young video models 2009",
		"underage.material",
		"random.kinderkutje.scene",
		"foo.opva.bar",
		"YVM-stuff",
		"event.10yo.party",
		"event.15 y o.party",
		"child porn 2",
		"child-porn-rip",
		"kiddyporn",
		"kiddieporn",
		"pornochild stuff",
		"hebefilia.pack",
		"paedophile-content",
		"pedophile-content",
	}
	for _, title := range mustMatch {
		t.Run("match/"+title, func(t *testing.T) {
			if !matchesBannedKeyword(title, nil) {
				t.Errorf("expected %q to match banned keyword regex", title)
			}
		})
	}

	mustNotMatch := []string{
		"",
		"Inception 2010 1080p BluRay",
		"Some.TV.Show.S01E02.1080p",
		"Music Album 2020",
		"linux.iso",
		"normal documentary about minors and law", // no banned token (whole-word)
		"underaged", // 'underage' wraps a longer word — \b boundary requires whole-word; check this is correct
		"kidporn",   // no separator — 'kid' isn't 'kiddy'/'kiddie'
		"oldyo",     // 'yo' alone isn't enough; pattern requires age qualifier
	}
	for _, title := range mustNotMatch {
		t.Run("no_match/"+title, func(t *testing.T) {
			if matchesBannedKeyword(title, nil) {
				t.Errorf("expected %q NOT to match banned keyword regex", title)
			}
		})
	}
}

func TestMatchesBannedKeyword_FilePathsToo(t *testing.T) {
	if !matchesBannedKeyword("innocuous.title", []string{"folder/preteen.thing/clip.mp4"}) {
		t.Error("file path with banned keyword did not match")
	}
}

func TestNewExporter_NoOpWhenDisabled(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.ExportEnabled = false
	exp := NewExporter(cfg, nil, NewMetrics())
	exp.Record(context.Background(), protocol.ID{1, 2, 3}, "preteen.thing", nil)
	if err := exp.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
}

func TestExporter_AppendsLocalJSONL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "csam-double-hashes.jsonl")
	cfg := NewDefaultConfig()
	cfg.ExportFilePath = path
	cfg.ExportUpstreamURL = "" // local only
	exp := NewExporter(cfg, nil, NewMetrics())
	defer exp.Close()

	var ih protocol.ID
	for i := range ih {
		ih[i] = byte(i + 0x10)
	}
	exp.Record(context.Background(), ih, "underage.thing", nil)
	exp.Record(context.Background(), protocol.ID{1, 2, 3}, "totally.fine.movie.2024", nil) // should be skipped

	if err := exp.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	lines := 0
	for scanner.Scan() {
		lines++
		var rec observationRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			t.Fatalf("unmarshal line %d: %v", lines, err)
		}
		if rec.DoubleHash != DoubleHashHex(ih) {
			t.Errorf("line %d: DoubleHash mismatch", lines)
		}
		if rec.Reason != "banned_keyword" {
			t.Errorf("line %d: Reason = %q", lines, rec.Reason)
		}
		if rec.Timestamp == "" {
			t.Errorf("line %d: empty Timestamp", lines)
		}
		// Must NOT contain the raw infohash.
		if strings.Contains(scanner.Text(), "010203") {
			t.Errorf("line %d: looks like raw infohash leaked", lines)
		}
	}
	if lines != 1 {
		t.Errorf("got %d lines, want 1 (the non-matching record should be skipped)", lines)
	}
}

func TestExporter_UpstreamPOST(t *testing.T) {
	var (
		hits int32
		mu   sync.Mutex
		body []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		mu.Lock()
		defer mu.Unlock()
		body, _ = readAll(r.Body)
		if r.Header.Get("Authorization") != "Bearer test" {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "ct", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := NewDefaultConfig()
	cfg.ExportFilePath = filepath.Join(dir, "x.jsonl")
	cfg.ExportUpstreamURL = srv.URL
	cfg.ExportUpstreamAuthHeader = "Bearer test"
	exp := NewExporter(cfg, nil, NewMetrics())
	defer exp.Close()

	exp.Record(context.Background(), protocol.ID{1, 2, 3}, "preteen.thing", nil)
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("upstream hits = %d, want 1", got)
	}
	mu.Lock()
	gotBody := string(body)
	mu.Unlock()
	if !strings.Contains(gotBody, `"reason":"banned_keyword"`) {
		t.Errorf("upstream body missing reason: %s", gotBody)
	}
}

func readAll(r interface{ Read(p []byte) (int, error) }) ([]byte, error) {
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 256)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return buf, err
		}
	}
}

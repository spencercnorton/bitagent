package llmeval

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestCheckedInProtocolSmokeCorpusIsValid(t *testing.T) {
	file, err := os.Open(opsFixture(t, "../../ops/llm-eval/protocol-smoke.jsonl"))
	if err != nil {
		t.Fatalf("open smoke corpus: %v", err)
	}
	defer file.Close()

	corpus, err := ReadCorpus(file)
	if err != nil {
		t.Fatalf("read smoke corpus: %v", err)
	}
	if len(corpus.Records) != 8 {
		t.Fatalf("smoke corpus records = %d, want 8", len(corpus.Records))
	}
	if len(corpus.SHA256) != 64 {
		t.Fatalf("smoke corpus SHA-256 length = %d, want 64", len(corpus.SHA256))
	}
}

func TestCanonicalCorpusJSONLStable(t *testing.T) {
	extractA := testExtraction("Example Movie")
	extractB := testExtraction("Example Film")

	firstExtract := testExtractRecord("extract:1", MatcherExtractExpected{
		Acceptable: []MatcherExtraction{extractB, extractA},
	})
	firstExtract.SliceIDs = []string{"z-slice", "a-slice"}
	rerank := testRerankRecord("rerank:1", MatcherRerankExpected{
		AcceptableTMDBIDs: []int64{202, 101},
	})

	var first bytes.Buffer
	firstID, err := WriteCorpus(&first, []CorpusRecord{rerank, firstExtract})
	if err != nil {
		t.Fatalf("WriteCorpus first: %v", err)
	}

	secondExtract := firstExtract
	secondExtract.SliceIDs = []string{"a-slice", "z-slice"}
	secondExtract.MatcherExtract = &MatcherExtractCase{
		Input: firstExtract.MatcherExtract.Input,
		Expected: MatcherExtractExpected{
			Acceptable: []MatcherExtraction{extractA, extractB},
		},
	}
	secondRerank := rerank
	secondRerank.MatcherRerank = &MatcherRerankCase{
		Input: rerank.MatcherRerank.Input,
		Expected: MatcherRerankExpected{
			AcceptableTMDBIDs: []int64{101, 202},
		},
	}

	var second bytes.Buffer
	secondID, err := WriteCorpus(&second, []CorpusRecord{secondExtract, secondRerank})
	if err != nil {
		t.Fatalf("WriteCorpus second: %v", err)
	}

	if firstID != secondID {
		t.Fatalf("corpus IDs differ: %s != %s", firstID, secondID)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatalf("canonical JSONL differs:\n%s\n---\n%s", first.String(), second.String())
	}
	if len(firstID) != 64 {
		t.Fatalf("SHA-256 length = %d, want 64", len(firstID))
	}
	if got := firstExtract.SliceIDs[0]; got != "z-slice" {
		t.Fatalf("WriteCorpus mutated caller record: first slice = %q", got)
	}

	read, err := ReadCorpus(bytes.NewReader(first.Bytes()))
	if err != nil {
		t.Fatalf("ReadCorpus: %v", err)
	}
	if read.SHA256 != firstID {
		t.Fatalf("read SHA-256 = %s, want %s", read.SHA256, firstID)
	}
	if read.Records[0].Task != TaskMatcherExtract || read.Records[1].Task != TaskMatcherRerank {
		t.Fatalf("records are not in canonical task order: %#v", read.Records)
	}
}

func TestReadCorpusRejectsUnknownAndDuplicateFields(t *testing.T) {
	record := testExtractRecord("extract:strict", MatcherExtractExpected{
		Acceptable: []MatcherExtraction{testExtraction("Example Movie")},
	})
	var valid bytes.Buffer
	if _, err := WriteCorpus(&valid, []CorpusRecord{record}); err != nil {
		t.Fatalf("WriteCorpus: %v", err)
	}

	unknown := strings.Replace(valid.String(), `"case_id"`, `"unexpected":true,"case_id"`, 1)
	if _, err := ReadCorpus(strings.NewReader(unknown)); err == nil ||
		!strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}

	duplicate := strings.Replace(
		valid.String(),
		`"case_id":"extract:strict"`,
		`"case_id":"extract:strict","case_id":"extract:other"`,
		1,
	)
	if _, err := ReadCorpus(strings.NewReader(duplicate)); err == nil ||
		!strings.Contains(err.Error(), "duplicate object key") {
		t.Fatalf("duplicate field error = %v", err)
	}
}

func TestReadJSONLWithLimitsRejectsOversizedArtifacts(t *testing.T) {
	if _, err := readJSONLWithLimits[map[string]any](
		strings.NewReader("{}\n{}\n"),
		"test",
		1,
		1024,
	); err == nil || !strings.Contains(err.Error(), "record limit") {
		t.Fatalf("record-limit error = %v", err)
	}

	if _, err := readJSONLWithLimits[map[string]any](
		strings.NewReader("{}\n"),
		"test",
		10,
		2,
	); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("byte-limit error = %v", err)
	}
}

func TestResultJSONLRoundTripAndCanonicalOrder(t *testing.T) {
	records := []CorpusRecord{
		testContentRecord("content:2", LanguageNonEnglish),
		testContentRecord("content:1", LanguageEnglish),
	}
	corpus := mustCorpus(t, records)

	keep := testResult(corpus, "content:1", TaskContentFilter)
	keep.ContentFilter = &ContentFilterResult{
		Action:     ContentFilterActionKeep,
		IsEnglish:  boolPointer(true),
		Confidence: 0.99,
		ReasonTag:  "english-clear",
	}
	drop := testResult(corpus, "content:2", TaskContentFilter)
	drop.ContentFilter = &ContentFilterResult{
		Action:     ContentFilterActionDrop,
		IsEnglish:  boolPointer(false),
		Confidence: 0.97,
		ReasonTag:  "spanish-article",
	}

	var raw bytes.Buffer
	if err := WriteResults(&raw, []ResultRecord{drop, keep}); err != nil {
		t.Fatalf("WriteResults: %v", err)
	}
	if strings.Index(raw.String(), `"case_id":"content:1"`) >
		strings.Index(raw.String(), `"case_id":"content:2"`) {
		t.Fatalf("results are not in canonical case order:\n%s", raw.String())
	}

	got, err := ReadResults(bytes.NewReader(raw.Bytes()))
	if err != nil {
		t.Fatalf("ReadResults: %v", err)
	}
	if len(got) != 2 || got[0].CaseID != "content:1" || got[1].CaseID != "content:2" {
		t.Fatalf("round-trip results = %#v", got)
	}
}

func TestReadResultsWithThresholdsRejectsActionThresholdTamper(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testRerankRecord("rerank:threshold", MatcherRerankExpected{
			AcceptableTMDBIDs: []int64{101},
		}),
	})
	result := testResult(corpus, "rerank:threshold", TaskMatcherRerank)
	result.MatcherRerank = &MatcherRerankResult{
		Action: MatcherRerankActionAttach, TMDBID: 101, Confidence: 0.40,
	}
	var raw bytes.Buffer
	if err := WriteResults(&raw, []ResultRecord{result}); err != nil {
		t.Fatalf("structural WriteResults: %v", err)
	}
	if _, err := ReadResults(bytes.NewReader(raw.Bytes())); err != nil {
		t.Fatalf("structural ReadResults: %v", err)
	}
	if _, err := ReadResultsWithThresholds(
		bytes.NewReader(raw.Bytes()),
		ProductionThresholds(),
	); err == nil || !strings.Contains(err.Error(), "attach action requires at least") {
		t.Fatalf("production-threshold read error = %v", err)
	}
	custom := ProductionThresholds()
	custom.MatcherAttachConfidence = 0.35
	if _, err := ReadResultsWithThresholds(
		bytes.NewReader(raw.Bytes()),
		custom,
	); err != nil {
		t.Fatalf("custom-threshold read rejected: %v", err)
	}
}

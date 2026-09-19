package llmmatch

import (
	"math"
	"strconv"
	"testing"
)

// These fixtures retain the tolerant transport shapes observed against the
// deployed request body on 2026-07-27 while supplying the complete action
// contract. The old json.Unmarshal-into-int path failed all except the plain
// cases; a separate regression below proves partial contracts now abstain.

func TestDecodeExtractionAcceptsRealModelReplies(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want Extraction
	}{
		{
			name: "plain object (gpt-5.4-nano)",
			raw:  `{"title":"Scream 7","year":2026,"type":"movie","season":0,"episode":0,"is_anime":false,"english":"unknown","is_pack":false,"is_adult":false}`,
			want: Extraction{Title: "Scream 7", Year: 2026, Type: "movie", English: "unknown"},
		},
		{
			name: "markdown fence (nova-micro)",
			raw:  "```json\n{\n  \"title\": \"In The Grey\",\n  \"year\": 2026,\n  \"type\": \"movie\",\n  \"is_anime\": false,\n  \"is_pack\": false,\n  \"is_adult\": false\n}\n```",
			want: Extraction{Title: "In The Grey", Year: 2026, Type: "movie"},
		},
		{
			name: "bare fence without info string",
			raw:  "```\n{\"title\":\"Apex\",\"year\":2026,\"type\":\"movie\",\"is_anime\":false,\"is_pack\":false,\"is_adult\":false}\n```",
			want: Extraction{Title: "Apex", Year: 2026, Type: "movie"},
		},
		{
			name: "null int (granite-4.1-8b, nova-micro)",
			raw:  `{"title":"The Super Mario Galaxy Movie","year":2026,"type":"movie","season":null,"episode":null,"is_anime":false,"is_pack":false,"is_adult":false}`,
			want: Extraction{Title: "The Super Mario Galaxy Movie", Year: 2026, Type: "movie"},
		},
		{
			name: "reasoning preamble",
			raw:  "<think>The name has a year and a scene tag, so it is a movie.</think>\n{\"title\":\"Signal One\",\"year\":2026,\"type\":\"movie\",\"is_anime\":false,\"is_pack\":false,\"is_adult\":false}",
			want: Extraction{Title: "Signal One", Year: 2026, Type: "movie"},
		},
		{
			name: "string-encoded numbers",
			raw:  `{"title":"Hacks","year":"2021","type":"tv","season":"2","episode":"7","is_anime":false,"is_pack":false,"is_adult":false}`,
			want: Extraction{Title: "Hacks", Year: 2021, Type: "tv", Season: 2, Episode: 7},
		},
		{
			name: "float year",
			raw:  `{"title":"Dune","year":2021.0,"type":"movie","is_anime":false,"is_pack":false,"is_adult":false}`,
			want: Extraction{Title: "Dune", Year: 2021, Type: "movie"},
		},
		{
			name: "string bools",
			raw:  `{"title":"Frieren","year":2023,"type":"tv","is_anime":"true","english":"sub","is_pack":"false","is_adult":"false"}`,
			want: Extraction{Title: "Frieren", Year: 2023, Type: "tv", IsAnime: true, English: "sub"},
		},
		{
			name: "prose either side",
			raw:  "Here is the extraction:\n{\"title\":\"Nosferatu\",\"year\":1922,\"type\":\"movie\",\"is_anime\":false,\"is_pack\":false,\"is_adult\":false}\nHope that helps!",
			want: Extraction{Title: "Nosferatu", Year: 1922, Type: "movie"},
		},
		{
			name: "braces inside a string literal do not confuse the scanner",
			raw:  `{"title":"A {weird} title","year":2020,"type":"movie","is_anime":false,"is_pack":false,"is_adult":false}`,
			want: Extraction{Title: "A {weird} title", Year: 2020, Type: "movie"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeExtraction([]byte(tc.raw))
			if err != nil {
				t.Fatalf("decodeExtraction: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestDecodeExtractionRejectsPartialActionContract(t *testing.T) {
	for _, raw := range []string{
		`{"title":"Extraction"}`,
		`{"title":"Extraction","type":"movie"}`,
		`{"title":"Extraction","type":"movie","is_anime":false,"is_pack":false}`,
		`{"title":"Extraction","type":"movie","is_anime":true,"is_pack":false,"is_adult":false}`,
		`{"title":"Extraction","type":"documentary","is_anime":false,"is_pack":false,"is_adult":false}`,
	} {
		if _, err := decodeExtraction([]byte(raw)); err == nil {
			t.Fatalf("partial/action-unsafe extraction must fail: %s", raw)
		}
	}
	// No title is a clean abstention even when the model answered with the
	// wrong schema; no action can flow from this object.
	if ext, err := decodeExtraction([]byte(`{"tmdb_id":1}`)); err != nil || ext != (Extraction{}) {
		t.Fatalf("wrong-schema decline = (%+v, %v), want zero extraction", ext, err)
	}
}

// gemma-3-12b answers a stage-1 call with the stage-2 schema. That must not be
// a hard error — it is a well-formed reply to the wrong question, and the
// matcher already has a representation for "the model declined": OK=false.
func TestDecodeExtractionTreatsWrongSchemaAsDeclined(t *testing.T) {
	ext, err := decodeExtraction([]byte(`{"tmdb_id":184683,"confidence":0.95}`))
	if err != nil {
		t.Fatalf("wrong-schema reply must not error: %v", err)
	}
	normalizeExtraction(&ext)
	if ext.OK {
		t.Fatalf("expected OK=false for a reply carrying no title, got %+v", ext)
	}
}

// A reply that is not JSON at all must still fail, or a broken endpoint would
// silently look like "every torrent declined".
func TestDecodeExtractionStillRejectsNonJSON(t *testing.T) {
	for _, raw := range []string{"", "   ", "I cannot help with that.", "[1,2,3]"} {
		if _, err := decodeExtraction([]byte(raw)); err == nil {
			t.Fatalf("expected error for %q", raw)
		}
	}
}

func TestDecodeRerankAcceptsRealModelReplies(t *testing.T) {
	for _, tc := range []struct {
		name     string
		raw      string
		wantID   int64
		wantConf float64
	}{
		{"plain", `{"tmdb_id":438631,"confidence":0.95}`, 438631, 0.95},
		{"fenced", "```json\n{\"tmdb_id\":841,\"confidence\":0.8}\n```", 841, 0.8},
		{"null confidence", `{"tmdb_id":841,"confidence":null}`, 841, 0},
		{"string id", `{"tmdb_id":"841","confidence":"0.7"}`, 841, 0.7},
		{"no match sentinel", `{"tmdb_id":0,"confidence":0}`, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, conf, err := decodeRerank([]byte(tc.raw))
			if err != nil {
				t.Fatalf("decodeRerank: %v", err)
			}
			if id != tc.wantID || conf != tc.wantConf {
				t.Fatalf("got (%d, %v), want (%d, %v)", id, conf, tc.wantID, tc.wantConf)
			}
		})
	}
}

func TestEvaluationDecodeRerankMatchesLiveSelectionNormalization(t *testing.T) {
	candidates := []Candidate{{ID: 42, Title: "Example"}}
	for _, tc := range []struct {
		name     string
		raw      string
		wantID   int64
		wantConf float64
	}{
		{"offered", `{"tmdb_id":42,"confidence":0.9}`, 42, 0.9},
		{"explicit decline preserves confidence", `{"tmdb_id":0,"confidence":0.2}`, 0, 0.2},
		{"non-offered declines", `{"tmdb_id":99,"confidence":0.9}`, 0, 0},
		{"negative declines", `{"tmdb_id":-1,"confidence":0.9}`, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, confidence, err := EvaluationDecodeRerank(
				[]byte(tc.raw),
				candidates,
			)
			if err != nil {
				t.Fatalf("EvaluationDecodeRerank: %v", err)
			}
			if id != tc.wantID || confidence != tc.wantConf {
				t.Fatalf(
					"got (%d, %v), want (%d, %v)",
					id,
					confidence,
					tc.wantID,
					tc.wantConf,
				)
			}
		})
	}
	if _, _, err := EvaluationDecodeRerank(
		[]byte(`{"tmdb_id":42,"confidence":1.1}`),
		candidates,
	); err == nil {
		t.Fatal("out-of-range confidence must fail the live action boundary")
	}
}

func TestEvaluationDecodeExtractionMatchesLiveActionBoundary(t *testing.T) {
	valid := `{"title":"Example","year":2024,"type":"movie",` +
		`"season":0,"episode":0,"is_anime":false,"english":"unknown",` +
		`"is_pack":false,"is_adult":false}`
	if _, err := EvaluationDecodeExtraction([]byte(valid)); err != nil {
		t.Fatalf("valid extraction: %v", err)
	}
	for _, raw := range []string{
		`{"title":"Example","year":-1,"type":"movie","season":0,"episode":0,"is_anime":false,"english":"unknown","is_pack":false,"is_adult":false}`,
		`{"title":"Example","year":2024,"type":"movie","season":-1,"episode":0,"is_anime":false,"english":"unknown","is_pack":false,"is_adult":false}`,
		`{"title":"Example","year":2024,"type":"movie","season":0,"episode":0,"is_anime":false,"english":"invented","is_pack":false,"is_adult":false}`,
	} {
		if _, err := EvaluationDecodeExtraction([]byte(raw)); err == nil {
			t.Fatalf("unsafe extraction must fail the shared action boundary: %s", raw)
		}
	}
}

func TestNumericCoercionFailsSafe(t *testing.T) {
	for _, raw := range []string{
		`{"tmdb_id":841.9,"confidence":0.99}`,
		`{"tmdb_id":"841.9","confidence":0.99}`,
		`{"tmdb_id":"1e100","confidence":0.99}`,
	} {
		id, _, err := decodeRerank([]byte(raw))
		if err != nil {
			t.Fatalf("decodeRerank(%q): %v", raw, err)
		}
		if id != 0 {
			t.Fatalf("decodeRerank(%q) id = %d, want safe zero", raw, id)
		}
	}

	for _, raw := range []string{
		`{"tmdb_id":841,"confidence":"NaN"}`,
		`{"tmdb_id":841,"confidence":"+Inf"}`,
		`{"tmdb_id":841,"confidence":"-Inf"}`,
	} {
		_, confidence, err := decodeRerank([]byte(raw))
		if err != nil {
			t.Fatalf("decodeRerank(%q): %v", raw, err)
		}
		if confidence != 0 || math.IsNaN(confidence) {
			t.Fatalf(
				"decodeRerank(%q) confidence = %v, want safe zero",
				raw,
				confidence,
			)
		}
	}

	overflow := strconv.FormatUint(uint64(^uint(0)>>1)+1, 10)
	ext, err := decodeExtraction([]byte(
		`{"title":"Example","year":"` + overflow + `","type":"movie","is_anime":false,"is_pack":false,"is_adult":false}`,
	))
	if err != nil {
		t.Fatalf("decodeExtraction overflow: %v", err)
	}
	if ext.Year != 0 {
		t.Fatalf("overflow year = %d, want safe zero", ext.Year)
	}
}

// The regression this whole file exists to prevent: the pre-fix decoder was a
// bare json.Unmarshal into a struct with int fields. Assert that each shape it
// choked on is now handled, so reverting to the strict path fails here.
func TestToleranceIsTheRegressionGuard(t *testing.T) {
	strictWouldFail := []string{
		"```json\n{\"title\":\"X\",\"year\":2020,\"type\":\"movie\",\"is_anime\":false,\"is_pack\":false,\"is_adult\":false}\n```",
		`{"title":"X","year":2020,"type":"movie","season":null,"is_anime":false,"is_pack":false,"is_adult":false}`,
		"<think>reasoning</think>{\"title\":\"X\",\"year\":2020,\"type\":\"movie\",\"is_anime\":false,\"is_pack\":false,\"is_adult\":false}",
	}
	for _, raw := range strictWouldFail {
		ext, err := decodeExtraction([]byte(raw))
		if err != nil {
			t.Fatalf("%q must decode: %v", raw, err)
		}
		normalizeExtraction(&ext)
		if !ext.OK || ext.Title != "X" {
			t.Fatalf("%q decoded to %+v", raw, ext)
		}
	}
}

// encoding/json unmarshals a bare `null` into a struct with NO error, leaving
// it zero-valued. Without an explicit object check, an endpoint returning
// `null` would look exactly like "every torrent declined" — silent, and
// indistinguishable from healthy operation. Raised in review.
func TestDecodeRejectsTopLevelNonObjects(t *testing.T) {
	for _, raw := range []string{"null", "true", "123", `"a string"`, "[1,2,3]", "[]"} {
		if _, err := decodeExtraction([]byte(raw)); err == nil {
			t.Fatalf("decodeExtraction(%q) must error", raw)
		}
		if _, _, err := decodeRerank([]byte(raw)); err == nil {
			t.Fatalf("decodeRerank(%q) must error", raw)
		}
	}
	// A fenced null is still a null.
	if _, err := decodeExtraction([]byte("```json\nnull\n```")); err == nil {
		t.Fatal("fenced null must error")
	}
}

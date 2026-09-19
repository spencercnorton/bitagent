package anime

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
)

// TestForeignAudioOnly_AgainstGoldCorpus runs the rule over the 457-case
// frontier-adjudicated content-filter corpus and bounds how often it
// fires on a case the corpus calls English.
//
// The corpus labels the language of the WORK; this rule reads the language of
// the RELEASE, so the two answer different questions and a head-to-head recall
// figure would be meaningless. What the corpus CAN do is catch over-reach, and
// it did: two earlier drafts dropped Polish-subtitled and Swedish-subtitled
// English films before subtitle markers were removed from the token set.
//
// One disagreement is expected and correct — a Polish lektor voice-over of
// Beetlejuice, an English work that cannot be watched in English. Raise this
// bound only with a named, justified case.
func TestForeignAudioOnly_AgainstGoldCorpus(t *testing.T) {
	const corpus = "../../ops/llm-eval/corpora/cf_gold.jsonl"
	const maxEnglishFirings = 1

	f, err := os.Open(corpus)
	if err != nil {
		t.Skipf("gold corpus not present: %v", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var cases, fired int
	var offenders []string
	for sc.Scan() {
		var row struct {
			Name      string `json:"name"`
			IsEnglish bool   `json:"is_english"`
		}
		if json.Unmarshal(sc.Bytes(), &row) != nil {
			continue
		}
		cases++
		if row.IsEnglish && ForeignAudioOnly(row.Name) {
			fired++
			offenders = append(offenders, row.Name)
		}
	}
	if cases == 0 {
		t.Fatal("gold corpus parsed to zero cases")
	}
	if fired > maxEnglishFirings {
		t.Errorf("rule fired on %d English-labelled cases (bound %d): %q",
			fired, maxEnglishFirings, offenders)
	}
	t.Logf("gold corpus: %d cases, rule fired on %d English-labelled", cases, fired)
}

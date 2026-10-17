package langgate

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
)

func TestRuZhTitleCases(t *testing.T) {
	cases := map[string]bool{
		"Скалолазы (2025) WEB-DLRip": true, "功夫熊猫全系列": true, "NikeのＢ@第一会所@DarkX": true,
		"Dune.2021.1080p": false, "Beyoncе - B'Day": false, "[Shinkiro-raw] 双子(フタゴ)ノ母(ハハ)": false,
	}
	for name, want := range cases {
		if got := RuZhTitle(name); got != want {
			t.Errorf("RuZhTitle(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestLocalizedTitle(t *testing.T) {
	cases := []struct {
		release, canonical, original, lang string
		want                               bool
	}{
		// English-original work under a localized title: excluded even with an
		// English track (the DUAL tag is Evidence's concern, not this one's).
		{"Um Maluco no Golfe 2", "Happy Gilmore 2", "Happy Gilmore 2", "en", true},
		{"Un Loco en el Golf 2", "Happy Gilmore 2", "Happy Gilmore 2", "en", true},
		{"Le Fou du Golf 2", "Happy Gilmore 2", "Happy Gilmore 2", "en", true},
		{"O Ídolo 2", "The Idol 2", "The Idol 2", "en", true},
		{"Um Maluco no Golfe 2", "Happy Gilmore 2", "Happy Gilmore 2", "EN", true},
		// The same work under its English title, and kept-by-ruling cases.
		{"Happy Gilmore 2", "Happy Gilmore 2", "Happy Gilmore 2", "en", false},
		{"Lupin", "Lupin", "Lupin", "fr", false},
		{"Merlin", "Merlin", "Merlin", "en", false},
		// A foreign-original work is never flagged, whatever its title.
		{"Um Maluco no Golfe", "Um Maluco no Golfe", "Um Maluco no Golfe", "pt", false},
		// English alternate titles, even with no token in common, stay kept:
		// a mismatch alone is not foreign evidence.
		{"Northern Lights", "The Golden Compass", "The Golden Compass", "en", false},
		{"Harry Potter and the Sorcerer's Stone", "Harry Potter and the Philosopher's Stone", "Harry Potter and the Philosopher's Stone", "en", false},
		{"Die Hard", "Die Hard", "Die Hard", "en", false},
		// Incomplete evidence: no catalogue title, one-word release title.
		{"Um Maluco no Golfe 2", "", "", "en", false},
		{"Golfe", "Happy Gilmore 2", "Happy Gilmore 2", "en", false},
	}
	for _, c := range cases {
		if got := LocalizedTitle(c.release, c.canonical, c.original, c.lang); got != c.want {
			t.Errorf("LocalizedTitle(%q, %q, %q, %q) = %v, want %v", c.release, c.canonical, c.original, c.lang, got, c.want)
		}
	}
}

// TestRuZhTitleParity compares against the Python policy over every name in the local oracle (skipped when absent).
func TestRuZhTitleParity(t *testing.T) {
	path := os.Getenv("LANGGATE_POLICY_ORACLE")
	f, err := os.Open(path)
	if err != nil {
		t.Skip("policy oracle not present")
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	n, bad := 0, 0
	for sc.Scan() {
		var r struct {
			Name string `json:"name"`
			RuZh bool   `json:"ru_zh"`
		}
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		n++
		if RuZhTitle(r.Name) != r.RuZh {
			bad++
			if bad <= 5 {
				t.Errorf("mismatch: %q python=%v", r.Name, r.RuZh)
			}
		}
	}
	if bad > 0 {
		t.Fatalf("%d of %d names differ", bad, n)
	}
	t.Logf("%d names, 0 mismatches", n)
}

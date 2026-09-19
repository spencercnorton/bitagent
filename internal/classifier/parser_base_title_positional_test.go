package classifier

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/parsers"
	"github.com/spencercnorton/bitagent/internal/model"
)

// TestParserProducesTheBaseTitlesThePositionalRuleAssumes closes the gap
// between cel_base_title_test.go and production.
//
// That test hardcodes the baseTitle the rule sees. This one runs the REAL
// parser over the same release names and asserts the values agree, so the
// positional rule is validated against what the pipeline actually computes
// rather than against a convenient assumption. A parser change that alters
// title extraction will fail here rather than silently changing which
// torrents get deleted.
func TestParserProducesTheBaseTitlesThePositionalRuleAssumes(t *testing.T) {
	cases := []struct {
		releaseName string
		wantTitle   string
	}{
		{"The.Italian.Job.2003.1080p.BluRay.x264", "The Italian Job"},
		{"The.Danish.Girl.2015.1080p.BluRay.x264", "The Danish Girl"},
		{"The.Chi.S06E01.1080p.WEB.h264", "The Chi"},
		{"Dan.Da.Dan.S01E01.1080p", "Dan Da Dan"},
		{"Russian.Doll.S02E01.1080p.NF.WEB-DL", "Russian Doll"},
		{"Shang-Chi.and.the.Legend.of.the.Ten.Rings.2021.UHD.BluRay.2160p", "Shang Chi and the Legend of the Ten Rings"},
		{"Some.Movie.2024.FRENCH.1080p.BluRay.x264", "Some Movie"},
		{"The.Anomaly.2014.iTALiAN.AC3.1080p.BluRay.x264", "The Anomaly"},
		{"Oppenheimer.2023.2160p.BluRay.x265", "Oppenheimer"},
	}

	for _, tc := range cases {
		t.Run(tc.releaseName, func(t *testing.T) {
			torrent := model.Torrent{
				Name:        tc.releaseName,
				FilesStatus: model.FilesStatusSingle,
				Size:        2_000_000_000,
			}
			attrs, err := parsers.ParseVideoContent(torrent, classification.Result{})
			if err != nil {
				t.Fatalf("parse %q: %v", tc.releaseName, err)
			}
			if !attrs.BaseTitle.Valid {
				t.Fatalf("no base title parsed from %q", tc.releaseName)
			}
			if attrs.BaseTitle.String != tc.wantTitle {
				t.Fatalf("baseTitle = %q, want %q — the positional rule in "+
					"cel_base_title_test.go assumes the latter",
					attrs.BaseTitle.String, tc.wantTitle)
			}
		})
	}
}

// TestPositionalRuleAgainstRealParserOutput is the end-to-end form: parse the
// release name with the production parser, feed the resulting title to the
// real CEL rule, and check the delete decision. This is the closest thing to
// production behaviour that can run without a database.
func TestPositionalRuleAgainstRealParserOutput(t *testing.T) {
	env := celEnvForRules(t)

	cases := []struct {
		releaseName   string
		wantWholeName bool
		wantCurrent   bool
		wantCandidate bool
	}{
		// Catalogued works the old whole-name rule destroyed on title alone.
		{"The.Italian.Job.2003.1080p.BluRay.x264", true, false, false},
		{"The.Danish.Girl.2015.1080p.BluRay.x264", true, false, false},
		{"The.Chi.S06E01.1080p.WEB.h264", true, false, false},
		{"Dan.Da.Dan.S01E01.1080p", true, false, false},
		{"Russian.Doll.S02E01.1080p.NF.WEB-DL", true, false, false},
		// The current dot/underscore-only normalisation still destroys this
		// exact live false positive because the parser changed '-' to ' '.
		{"Shang-Chi.and.the.Legend.of.the.Ten.Rings.2021.UHD.BluRay.2160p", true, true, false},
		// A real tag after the hyphenated title must still delete.
		{"Shang-Chi.and.the.Legend.of.the.Ten.Rings.2021.FRENCH.1080p", true, true, true},
		// Genuine foreign-tagged releases that must keep being deleted.
		{"Some.Movie.2024.FRENCH.1080p.BluRay.x264", true, true, true},
		{"The.Anomaly.2014.iTALiAN.AC3.1080p.BluRay.x264", true, true, true},
		{"Some.Movie.2024.PT-BR.1080p", true, true, true},
		// Raw PT BR was not in the historical delete set and stays out.
		{"Some.Movie.2024.PT BR.1080p", false, false, false},
		// Untouched by either rule.
		{"Oppenheimer.2023.2160p.BluRay.x265", false, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.releaseName, func(t *testing.T) {
			attrs, err := parsers.ParseVideoContent(model.Torrent{
				Name:        tc.releaseName,
				FilesStatus: model.FilesStatusSingle,
				Size:        2_000_000_000,
			}, classification.Result{})
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			title := ""
			if attrs.BaseTitle.Valid {
				title = attrs.BaseTitle.String
			}

			if got := evalRule(t, env, deployedNonEnglishRule, tc.releaseName, title); got != tc.wantWholeName {
				t.Errorf("whole-name rule = %v, want %v (parsed title %q)", got, tc.wantWholeName, title)
			}
			if got := evalRule(t, env, currentOutsideTitleNonEnglishRule, tc.releaseName, title); got != tc.wantCurrent {
				t.Errorf("current outside-title rule = %v, want %v (parsed title %q)", got, tc.wantCurrent, title)
			}
			if got := evalRule(t, env, outsideTitleNonEnglishRule, tc.releaseName, title); got != tc.wantCandidate {
				t.Errorf("candidate outside-title rule = %v, want %v (parsed title %q)", got, tc.wantCandidate, title)
			}
		})
	}
}

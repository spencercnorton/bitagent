package classifier

import (
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/spencercnorton/bitagent/internal/protobuf"
)

// The deployed non-English delete rule, and the positional variant that
// exposing result.baseTitle makes expressible. Both are evaluated verbatim so
// the test measures what an operator would actually write in classifier.yml.
const (
	deployedNonEnglishRule = `torrent.baseName.matches(keywords.non_english) && ` +
		`!torrent.baseName.matches(keywords.english_or_dual)`

	// positionalNonEnglishRule was the first attempt: exempt the release when
	// the parsed TITLE contains a language word. It is a PROXY for position,
	// not position itself, and review flagged the gap:
	// the exemption disables the whole rule, so "The Italian Job 2003 FRENCH"
	// is spared despite a genuine FRENCH tag. Kept here as the comparison arm.
	positionalNonEnglishRule = `torrent.baseName.matches(keywords.non_english) && ` +
		`!(result.hasBaseTitle && result.baseTitle.matches(keywords.non_english)) && ` +
		`!torrent.baseName.matches(keywords.english_or_dual)`

	// currentOutsideTitleNonEnglishRule is the live 2026-08-08 rule. It
	// normalises dots and underscores, but not the hyphens that cleanTitle
	// converts to spaces.
	currentOutsideTitleNonEnglishRule = `torrent.baseName.replace(".", " ").replace("_", " ")` +
		`.replace(result.hasBaseTitle ? result.baseTitle : " none ", "")` +
		`.matches(keywords.non_english) && ` +
		`!torrent.baseName.matches(keywords.english_or_dual)`

	// outsideTitleNonEnglishRule is actually positional: it SUBTRACTS the
	// parsed title from the name and matches only what is left.
	//
	// The separator normalisation is load-bearing and is why a bare replace()
	// does not work. baseName keeps its raw separators
	// ("The.Danish.Girl.2015.1080p") while baseTitle is space-normalised
	// ("The Danish Girl"), so the literal never matches, the title leaks back
	// into the match, and the rule re-deletes exactly the works it exists to
	// spare. cleanTitle also turns ASCII hyphens into spaces ("Shang-Chi" ->
	// "Shang Chi") and squeezes whitespace, so dots, underscores, hyphens,
	// and repeated spaces must all be normalised before subtraction.
	//
	// Hyphen normalisation changes the real language tag PT-BR to PT BR. Keep
	// that historical delete only when the raw name matched pt-?br AND the
	// normalised token remains after title subtraction. This preserves the
	// prior delete set without broadening it to raw "PT BR" names.
	//
	// The sentinel " none " avoids replace() with an empty needle when the name
	// was never parsed; that case then falls through to deployed behaviour.
	outsideTitleNonEnglishRule = `(` +
		`torrent.baseName.replace(".", " ").replace("_", " ").replace("-", " ")` +
		`.split(" ").filter(s, s != "").join(" ")` +
		`.replace(result.hasBaseTitle ? result.baseTitle : " none ", "")` +
		`.matches(keywords.non_english) || (` +
		`torrent.baseName.matches(keywords.pt_br_raw) && ` +
		`torrent.baseName.replace(".", " ").replace("_", " ").replace("-", " ")` +
		`.split(" ").filter(s, s != "").join(" ")` +
		`.replace(result.hasBaseTitle ? result.baseTitle : " none ", "")` +
		`.matches(keywords.pt_br_normalized))) && ` +
		`!torrent.baseName.matches(keywords.english_or_dual)`
)

// deployedKeywordSets are the exact lists from the live
// /config/bitmagnet/classifier.yml on Galactic-Torrent.
var deployedKeywordSets = map[string][]string{
	"non_english": {
		"french", "truefrench", "vost(fr)?", "ger(man)?", "deutsch", "ita(lian)?",
		"spa(nish)?", "castellano", "latino", "por(tuguese)?", "pt-?br", "pol(ish)?",
		"rus(sian)?", "ukr(ainian)?", "tur(kish)?", "hindi", "tam(il)?", "tel(ugu)?",
		"malayalam", "kan(nada)?", "jpn", "japanese", "kor", "korean", "chi(nese)?",
		"zho", "mandarin", "cantonese", "viet(sub)?", "thai", "dut(ch)?", "nederlands",
		"swe(dish)?", "dan(ish)?", "nor(wegian)?", "fin(nish)?",
	},
	"english_or_dual":  {"english", "eng", "dual-?audio", "dual", "multi", "dub(bed)?"},
	"pt_br_raw":        {"pt-?br"},
	"pt_br_normalized": {"pt br", "ptbr"},
}

func celEnvForRules(t *testing.T) *cel.Env {
	t.Helper()
	ctx := &compilerContext{}
	if err := celEnvOption(Source{Keywords: deployedKeywordSets}, ctx); err != nil {
		t.Fatalf("build cel env: %v", err)
	}
	return ctx.celEnv
}

func evalRule(t *testing.T, env *cel.Env, rule, baseName, baseTitle string) bool {
	t.Helper()
	ast, iss := env.Compile(rule)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("compile %q: %v", rule, iss.Err())
	}
	prg, err := env.Program(ast)
	if err != nil {
		t.Fatalf("program: %v", err)
	}
	cl := &protobuf.Classification{HasBaseTitle: baseTitle != ""}
	if baseTitle != "" {
		bt := baseTitle
		cl.BaseTitle = &bt
	}
	out, _, err := prg.Eval(map[string]any{
		"torrent": &protobuf.Torrent{BaseName: baseName},
		"result":  cl,
	})
	if err != nil {
		t.Fatalf("eval %q on %q: %v", rule, baseName, err)
	}
	b, ok := out.Value().(bool)
	if !ok {
		t.Fatalf("rule returned %T, want bool", out.Value())
	}
	return b
}

// TestPositionalNonEnglishRule is the acceptance evidence for exposing
// result.baseTitle: the deployed rule cannot tell a language word in the TITLE
// from a language TAG, and the positional variant can.
//
// Measured motivation: run against all 160,687 rows of the TMDB
// mirror, the deployed predicate deletes 229 catalogued works on their title
// alone — 25 with >=500 TMDB votes.
func TestPositionalNonEnglishRule(t *testing.T) {
	env := celEnvForRules(t)

	cases := []struct {
		name           string
		baseName       string
		baseTitle      string
		wantDeployed   bool
		wantPositional bool
	}{
		// --- false positives the deployed rule destroys ---
		{
			name:         "The Italian Job — language word is the title",
			baseName:     "The.Italian.Job.2003.1080p.BluRay.x264",
			baseTitle:    "The Italian Job",
			wantDeployed: true, wantPositional: false,
		},
		{
			name:         "Shang-Chi — chi token inside the title",
			baseName:     "Shang-Chi.and.the.Legend.of.the.Ten.Rings.2021.2160p.BluRay.x265",
			baseTitle:    "Shang-Chi and the Legend of the Ten Rings",
			wantDeployed: true, wantPositional: false,
		},
		{
			name:         "Dan Da Dan — dan matches dan(ish)?",
			baseName:     "Dan.Da.Dan.S01E01.1080p.WEB.h264",
			baseTitle:    "Dan Da Dan",
			wantDeployed: true, wantPositional: false,
		},
		{
			name:         "The Chi",
			baseName:     "The.Chi.S06E01.1080p.WEB.h264",
			baseTitle:    "The Chi",
			wantDeployed: true, wantPositional: false,
		},
		{
			name:         "The Danish Girl",
			baseName:     "The.Danish.Girl.2015.1080p.BluRay.x264",
			baseTitle:    "The Danish Girl",
			wantDeployed: true, wantPositional: false,
		},

		// --- true positives that must keep being deleted ---
		{
			name:         "genuine FRENCH release tag, title is clean",
			baseName:     "Some.Movie.2024.FRENCH.1080p.BluRay.x264",
			baseTitle:    "Some Movie",
			wantDeployed: true, wantPositional: true,
		},
		{
			name:         "genuine iTALiAN release tag",
			baseName:     "The.Anomaly.2014.iTALiAN.AC3.1080p.BluRay.x264",
			baseTitle:    "The Anomaly",
			wantDeployed: true, wantPositional: true,
		},
		{
			name:         "GERMAN DL",
			baseName:     "Irgendein.Film.2019.German.DL.1080p.BluRay.x264",
			baseTitle:    "Irgendein Film",
			wantDeployed: true, wantPositional: true,
		},

		// --- the english_or_dual exception still wins in both ---
		{
			name:         "MULTi spares it under both rules",
			baseName:     "Hallow.Road.2025.MULTi.FRENCH.2160p.WEB-DL.H265",
			baseTitle:    "Hallow Road",
			wantDeployed: false, wantPositional: false,
		},
		{
			name:         "FRENCH-ENG spares it under both rules",
			baseName:     "Dune.2021.FRENCH-ENG.1080p.WEB.x264",
			baseTitle:    "Dune",
			wantDeployed: false, wantPositional: false,
		},

		// --- no language token at all ---
		{
			name:         "ordinary English release untouched",
			baseName:     "Oppenheimer.2023.2160p.BluRay.x265",
			baseTitle:    "Oppenheimer",
			wantDeployed: false, wantPositional: false,
		},

		// --- unparsed title: the guard must not crash or over-spare ---
		{
			name:         "no base title falls back to deployed behaviour",
			baseName:     "Some.Movie.2024.FRENCH.1080p",
			baseTitle:    "",
			wantDeployed: true, wantPositional: true,
		},
		{
			// NAME CORRECTED. This case asserted wantPositional:false while
			// being called "still deletes" — the name said the opposite of the
			// assertion, and an MR description was written from the name rather
			// than the value. outsideTitleNonEnglishRule is what actually
			// deletes it.
			name:         "language word in title AND a real tag is SPARED by the proxy rule",
			baseName:     "The.Italian.Job.2003.iTALiAN.1080p.BluRay",
			baseTitle:    "The Italian Job",
			wantDeployed: true, wantPositional: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := evalRule(t, env, deployedNonEnglishRule, tc.baseName, tc.baseTitle); got != tc.wantDeployed {
				t.Errorf("deployed rule = %v, want %v", got, tc.wantDeployed)
			}
			if got := evalRule(t, env, positionalNonEnglishRule, tc.baseName, tc.baseTitle); got != tc.wantPositional {
				t.Errorf("positional rule = %v, want %v", got, tc.wantPositional)
			}
		})
	}
}

// TestPositionalRuleIsStrictlyMorePermissive proves the safety property that
// makes this change deployable without a recall measurement first: the
// positional rule adds a conjunct, so it can only ever delete FEWER torrents.
// It can never destroy something the deployed rule keeps.
func TestPositionalRuleIsStrictlyMorePermissive(t *testing.T) {
	env := celEnvForRules(t)

	names := []string{
		"The.Italian.Job.2003.1080p.BluRay.x264",
		"Some.Movie.2024.FRENCH.1080p.BluRay.x264",
		"Oppenheimer.2023.2160p.BluRay.x265",
		"Dune.2021.FRENCH-ENG.1080p.WEB.x264",
		"Shang-Chi.and.the.Legend.of.the.Ten.Rings.2021.2160p",
		"Russian.Doll.S02E01.1080p.NF.WEB-DL",
		"Irgendein.Film.2019.German.DL.1080p",
		"The.Spanish.Princess.S01E01.1080p.WEB",
		"A.Korean.Odyssey.S01E01.1080p.NF.WEB-DL",
		"Bare.Name.With.No.Tokens",
	}
	titles := []string{"", "Some Movie", "The Italian Job", "Russian Doll", "Dune"}

	for _, n := range names {
		for _, ti := range titles {
			deployed := evalRule(t, env, deployedNonEnglishRule, n, ti)
			positional := evalRule(t, env, positionalNonEnglishRule, n, ti)
			if positional && !deployed {
				t.Errorf("positional rule deletes %q (title %q) where deployed keeps it — "+
					"the change must only ever be more permissive", n, ti)
			}
		}
	}
}

// TestOutsideTitleRuleClosesTheJeevesGap covers the finding Jeeves raised on
// Review: the proxy rule exempts a release whenever the parsed
// TITLE contains any language word, which disables the whole rule even when a
// genuine foreign tag sits outside the title. "The Italian Job 2003 FRENCH" was
// spared despite being French.
//
// outsideTitleNonEnglishRule subtracts the title and matches the remainder, so
// position is actually tested rather than approximated.
func TestOutsideTitleRuleClosesTheJeevesGap(t *testing.T) {
	env := celEnvForRules(t)
	for _, tc := range []struct {
		what        string
		base, title string
		want        bool
	}{
		// The 229 catalogued works: the language word IS the title. SPARE.
		{"Italian Job", "The.Italian.Job.2003.1080p.BluRay.x264", "The Italian Job", false},
		{"Danish Girl", "The.Danish.Girl.2015.1080p.BluRay.x264", "The Danish Girl", false},
		{"The Chi", "The.Chi.S06E01.1080p.WEB.h264", "The Chi", false},
		{"Dan Da Dan", "Dan.Da.Dan.S01E01.1080p", "Dan Da Dan", false},
		{"Russian Doll", "Russian.Doll.S02E01.1080p.NF.WEB-DL", "Russian Doll", false},
		// ParseVideoContent normalises the raw hyphen to a space. Using a
		// hyphenated baseTitle here hid the production failure until the delete
		// audit captured this exact release on 2026-08-11.
		{"Shang-Chi", "Shang-Chi.and.the.Legend.of.the.Ten.Rings.2021.1080p", "Shang Chi and the Legend of the Ten Rings", false},
		{"Shang-Chi + FRENCH", "Shang-Chi.and.the.Legend.of.the.Ten.Rings.2021.FRENCH.1080p", "Shang Chi and the Legend of the Ten Rings", true},
		{"Shang--Chi repeated separator", "Shang--Chi.and.the.Legend.of.the.Ten.Rings.2021.1080p", "Shang Chi and the Legend of the Ten Rings", false},
		{"mixed and repeated separators", "The..Italian__Job.2003.1080p", "The Italian Job", false},

		// Genuine foreign tag outside a clean title. DELETE.
		{"genuine FRENCH", "Some.Movie.2024.FRENCH.1080p.BluRay.x264", "Some Movie", true},
		{"genuine iTALiAN", "The.Anomaly.2014.iTALiAN.AC3.1080p.BluRay.x264", "The Anomaly", true},
		{"genuine PT-BR remains deleted", "Some.Movie.2024.PT-BR.1080p", "Some Movie", true},
		{"genuine PTBR remains deleted", "Some.Movie.2024.PTBR.1080p", "Some Movie", true},
		{"raw PT BR remains outside historical delete set", "Some.Movie.2024.PT BR.1080p", "Some Movie", false},
		{"PT-BR inside title is spared", "A.PT-BR.Movie.2024.1080p", "A PT BR Movie", false},

		// THE GAP: language word in the title AND a real tag outside it. DELETE.
		{"Italian Job + FRENCH", "The.Italian.Job.2003.FRENCH.1080p.BluRay", "The Italian Job", true},
		{"Italian Job + iTALiAN", "The.Italian.Job.2003.iTALiAN.1080p.BluRay", "The Italian Job", true},
		{"Danish Girl + GERMAN", "The.Danish.Girl.2015.GERMAN.1080p", "The Danish Girl", true},
		{"space-separated too", "The Italian Job 2003 FRENCH 1080p", "The Italian Job", true},

		// The english/dual exception still wins over everything.
		{"MULTi", "Some.Movie.2024.MULTi.1080p", "Some Movie", false},
		{"FRENCH plus ENG", "Some.Movie.2024.FRENCH.ENG.1080p", "Some Movie", false},

		// Unparsed name falls back to deployed behaviour rather than sparing all.
		{"unparsed", "Some.Movie.2024.FRENCH.1080p", "", true},

		// Ordinary English release untouched.
		{"Oppenheimer", "Oppenheimer.2023.2160p.BluRay.x265", "Oppenheimer", false},
	} {
		t.Run(tc.what, func(t *testing.T) {
			if got := evalRule(t, env, outsideTitleNonEnglishRule, tc.base, tc.title); got != tc.want {
				t.Errorf("%q (title %q): deletes = %v, want %v", tc.base, tc.title, got, tc.want)
			}
		})
	}
}

// TestOutsideTitleRuleNeverDeletesMoreThanDeployed is the safety invariant.
// The outside-title rule deletes MORE than the proxy rule — that is the point —
// but it must never delete anything the CURRENTLY DEPLOYED rule spares, or
// shipping it would destroy content that survives today.
func TestOutsideTitleRuleNeverDeletesMoreThanDeployed(t *testing.T) {
	env := celEnvForRules(t)
	titles := []string{"The Italian Job", "The Danish Girl", "Some Movie", "Oppenheimer", "Russian Doll", ""}
	tails := []string{
		"", ".FRENCH", ".iTALiAN", ".GERMAN.DL", ".MULTi", ".FRENCH.ENG",
		".1080p.BluRay", ".2003.1080p", ".DUAL-AUDIO", ".JPN", ".KOR.SUB",
	}
	var checked int
	for _, title := range titles {
		for _, tail := range tails {
			base := strings.ReplaceAll(title, " ", ".") + tail + ".x264"
			checked++
			deployed := evalRule(t, env, deployedNonEnglishRule, base, title)
			outside := evalRule(t, env, outsideTitleNonEnglishRule, base, title)
			if outside && !deployed {
				t.Errorf("REGRESSION: outside-title rule deletes something deployed spares:\n  base=%q title=%q", base, title)
			}
		}
	}
	t.Logf("checked %d (name, title) combinations; outside-title is a strict subset of deployed", checked)
}

// TestHyphenNormalizationNeverDeletesMoreThanCurrent pins the rollout
// invariant against the rule that is live now, not merely against the older
// whole-name predicate. The repair may spare additional titles, but it must
// not create a new delete anywhere in this separator/tag cross-product.
func TestHyphenNormalizationNeverDeletesMoreThanCurrent(t *testing.T) {
	env := celEnvForRules(t)
	titles := []string{"The Italian Job", "The Danish Girl", "Some Movie", "Shang Chi", "A PT BR Movie", ""}
	separators := []string{" ", ".", "_", "-", "--", "..", "__"}
	tails := []string{
		"", ".FRENCH", ".iTALiAN", ".GERMAN.DL", ".PT-BR", ".PTBR",
		".PT BR", ".PT.BR", ".PT_BR", ".MULTi", ".FRENCH.ENG",
		".1080p.BluRay", ".DUAL-AUDIO", ".JPN", ".KOR.SUB",
	}
	var checked int
	for _, title := range titles {
		for _, separator := range separators {
			baseTitle := title
			base := strings.ReplaceAll(title, " ", separator)
			for _, tail := range tails {
				name := base + tail + ".x264"
				checked++
				current := evalRule(t, env, currentOutsideTitleNonEnglishRule, name, baseTitle)
				candidate := evalRule(t, env, outsideTitleNonEnglishRule, name, baseTitle)
				if candidate && !current {
					t.Errorf("REGRESSION: candidate newly deletes name=%q title=%q", name, baseTitle)
				}
			}
		}
	}
	t.Logf("checked %d (name, title) combinations; candidate never broadens the live delete set", checked)
}

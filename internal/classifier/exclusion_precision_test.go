package classifier

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
	"gopkg.in/yaml.v3"
)

// The exclusion classifiers (keywords.xxx_*, keywords.course_*, and the
// extension rules behind music/ebook/comic/audiobook/software) delete content at
// classification time, before it can reach quarantine or an LLM. A false
// positive is therefore unrecoverable, which makes their precision the number
// that matters.
//
// This test runs the REAL core workflow against a labelled corpus and reports
// per-classifier precision and recall. It is the feedback loop: a wrong call
// becomes a row in testdata/exclusion_precision_corpus.yml, and the row keeps
// it wrong forever after.
//
// To add a case: append to the corpus with expect: keep|delete and a `why`.
// To retune a keyword: move it between the strong and weak groups in
// classifier.core.yml and re-run. The numbers below move; the gate does not.

const corpusPath = "testdata/exclusion_precision_corpus.yml"

// recallFloor is the fraction of expect:delete cases that must still be
// deleted. It is deliberately below 1.0: the tiering trades recall on adult and
// course content (cheap to miss — it falls through to the LLM/quarantine path)
// for precision on real titles (unrecoverable to get wrong). Move it
// deliberately, with a corpus run to justify the move.
const recallFloor = 0.85

// expectKeep pairs with deleteName (action_delete.go) as the two corpus labels.
const expectKeep = "keep"

type precisionCase struct {
	Name   string   `yaml:"name"`
	Files  []string `yaml:"files"`
	Expect string   `yaml:"expect"` // keep | delete
	Type   string   `yaml:"type"`   // what it actually is, for reporting
	Why    string   `yaml:"why"`
	// Gap marks a case the classifiers are known not to catch and that we have
	// decided not to chase. It still shows in the per-type recall table — the
	// point is to keep the limitation visible — but it does not move the gate.
	Gap bool `yaml:"gap"`
}

type precisionCorpus struct {
	Cases []precisionCase `yaml:"cases"`
}

// productionFlags mirrors the live deployment: CLASSIFIER_DELETE_XXX=true and
// CLASSIFIER_DELETE_CONTENT_TYPES=music,ebook,audiobook,comic,game,software
// (plus `course`, which this branch adds). Search and TMDB are off so that the
// measurement isolates the exclusion rules from the matcher — the matcher runs
// after these rules and cannot rescue anything they delete.
func productionFlags() Flags {
	return Flags{
		"local_search_enabled": false,
		"apis_enabled":         false,
		"tmdb_enabled":         false,
		"llm_match_enabled":    false,
		"delete_xxx":           true,
		"delete_content_types": []any{
			"music", "ebook", "audiobook", "comic", "game", "software", "course",
		},
	}
}

// torrentFromCase builds a torrent whose file list drives the size-weighted
// extension rules. "path/name.ext|4200" means a 4200 MB file.
func torrentFromCase(t *testing.T, c precisionCase) model.Torrent {
	t.Helper()

	files := make([]model.TorrentFile, 0, len(c.Files))
	total := uint(0)

	for i, spec := range c.Files {
		path, sizeStr, ok := strings.Cut(spec, "|")
		if !ok {
			t.Fatalf("case %q: file spec %q must be \"path|sizeMB\"", c.Name, spec)
		}

		sizeMB, err := strconv.ParseFloat(strings.TrimSpace(sizeStr), 64)
		if err != nil {
			t.Fatalf("case %q: bad size in %q: %v", c.Name, spec, err)
		}

		size := uint(sizeMB * 1_000_000)
		total += size

		files = append(files, model.TorrentFile{
			Index:     uint(i),
			Path:      path,
			Extension: model.FileExtensionFromPath(path),
			Size:      size,
		})
	}

	// FilesStatusSingle means the torrent IS the file, so its name carries the
	// extension. Extension must then come from the NAME, not from the file entry:
	// Torrent.BaseName() strips len(Extension)+1 characters off the end of Name,
	// so an extension taken from elsewhere silently truncates the text the
	// keyword rules match against. A one-file torrent whose name has no
	// extension is a folder containing one file — that is Multi.
	if ext := model.FileExtensionFromPath(c.Name); len(files) == 1 && ext.Valid {
		return model.Torrent{
			Name:        c.Name,
			Size:        total,
			FilesStatus: model.FilesStatusSingle,
			Extension:   ext,
		}
	}

	return model.Torrent{
		Name:        c.Name,
		Size:        total,
		FilesStatus: model.FilesStatusMulti,
		Files:       files,
	}
}

type tally struct{ tp, fp, fn, tn int }

// caseOutcome interprets what workflow.Run returned for one corpus case.
//
// The exclusion path has exactly two normal outcomes: no error (the torrent
// survives) or ErrDeleteTorrent (a classifier deleted it). Anything else — a
// CEL runtime error, a broken rule, a top-level ErrUnmatched — means the
// workflow did not reach a decision, and production agrees: processor.go:206
// routes every non-ErrDeleteTorrent error to failedHashes, never to a keep.
//
// Reading such an error as "not deleted" is what this function exists to
// prevent. Every expect:keep case would then score a true negative, so a
// workflow that could not evaluate at all would post a clean sweep and sail
// through the zero-false-positive gate — the gate would be measuring nothing.
func caseOutcome(runErr error) (deleted bool, err error) {
	switch {
	case runErr == nil:
		return false, nil
	case errors.Is(runErr, classification.ErrDeleteTorrent):
		return true, nil
	default:
		return false, runErr
	}
}

func TestExclusionClassifierPrecision(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}

	var corpus precisionCorpus
	if err := yaml.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}

	if len(corpus.Cases) == 0 {
		t.Fatal("corpus is empty")
	}

	mocks := newTestClassifierMocks(t)

	source, err := yamlSourceProvider{rawSourceProvider: coreSourceProvider{}}.source()
	if err != nil {
		t.Fatalf("load core source: %v", err)
	}

	workflow, err := mocks.compiler.Compile(source)
	if err != nil {
		t.Fatalf("compile core workflow: %v", err)
	}

	flags := productionFlags()
	byType := map[string]*tally{}
	overall := tally{}

	var falsePositives, falseNegatives []string

	gaps := 0

	for _, c := range corpus.Cases {
		if c.Expect != expectKeep && c.Expect != deleteName {
			t.Fatalf("case %q: expect must be keep or delete, got %q", c.Name, c.Expect)
		}

		res, runErr := workflow.Run(context.Background(), "default", flags, torrentFromCase(t, c))

		// A case the workflow could not decide is a broken harness, not a
		// keep. Fail it loudly and keep it out of the confusion matrix
		// entirely — counting it either way would launder a runtime failure
		// into a precision/recall number.
		deleted, outcomeErr := caseOutcome(runErr)
		if outcomeErr != nil {
			t.Errorf("case %q: workflow returned an unexpected error, so the case reached no keep/delete decision: %v", c.Name, outcomeErr)

			continue
		}

		// The rule path is exactly what production records as evidence on a
		// classifier_delete verdict, so reporting it here means a corpus failure
		// and a production verdict are described in the same terms.
		got := "-"
		if res.ContentType.Valid {
			got = string(res.ContentType.ContentType)
		}

		var re classification.RuntimeError
		if errors.As(runErr, &re) && len(re.Path) > 0 {
			got = strings.Join(re.Path, "/")
		}

		key := c.Type
		if key == "" {
			key = "unspecified"
		}

		if byType[key] == nil {
			byType[key] = &tally{}
		}

		switch {
		case c.Expect == deleteName && deleted:
			byType[key].tp++
			overall.tp++
		case c.Expect == deleteName && !deleted:
			byType[key].fn++

			if c.Gap {
				gaps++
			} else {
				overall.fn++

				falseNegatives = append(falseNegatives, fmt.Sprintf("[typed %s] %s  (%s)", got, c.Name, c.Why))
			}
		case c.Expect == expectKeep && deleted:
			byType[key].fp++
			overall.fp++

			falsePositives = append(falsePositives, fmt.Sprintf("[typed %s] %s  (%s)", got, c.Name, c.Why))
		default:
			byType[key].tn++
			overall.tn++
		}
	}

	keys := make([]string, 0, len(byType))
	for k := range byType {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	t.Logf("exclusion classifier precision over %d cases", len(corpus.Cases))
	t.Logf("%-14s %6s %6s %6s %6s  %9s %9s", "type", "TP", "FP", "FN", "TN", "precision", "recall")

	for _, k := range keys {
		v := byType[k]
		t.Logf("%-14s %6d %6d %6d %6d  %9s %9s",
			k, v.tp, v.fp, v.fn, v.tn, ratio(v.tp, v.tp+v.fp), ratio(v.tp, v.tp+v.fn))
	}

	t.Logf("%-14s %6d %6d %6d %6d  %9s %9s", "OVERALL",
		overall.tp, overall.fp, overall.fn, overall.tn,
		ratio(overall.tp, overall.tp+overall.fp), ratio(overall.tp, overall.tp+overall.fn))

	if gaps > 0 {
		t.Logf("%d documented gap case(s) excluded from the recall gate", gaps)
	}

	// Precision gate. A deleted keep-worthy title cannot be recovered: the row is
	// dropped from `torrents` and the infohash is added to the blocking bloom
	// filter, which has no removal operation. There is no acceptable count here
	// other than zero.
	if len(falsePositives) > 0 {
		t.Errorf("%d legitimate title(s) deleted by an exclusion classifier:", len(falsePositives))

		for _, fp := range falsePositives {
			t.Errorf("  DELETED: %s", fp)
		}
	}

	// Recall floor. Misses are cheap — they fall through to the LLM/quarantine
	// path — but a silent collapse would mean the classifiers stopped working.
	if got := len(falseNegatives); overall.tp+got > 0 {
		recall := float64(overall.tp) / float64(overall.tp+got)
		if recall < recallFloor {
			t.Errorf("recall %.3f below floor %.3f (%d missed):", recall, recallFloor, got)

			for _, fn := range falseNegatives {
				t.Errorf("  MISSED: %s", fn)
			}
		} else {
			for _, fn := range falseNegatives {
				t.Logf("  missed (within floor): %s", fn)
			}
		}
	}
}

func ratio(num, den int) string {
	if den == 0 {
		return "n/a"
	}

	return fmt.Sprintf("%.3f", float64(num)/float64(den))
}

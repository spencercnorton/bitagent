package classifier

import (
	"context"
	"errors"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
)

// rawYAML is an inline rawSourceProvider, so a test can compile a workflow
// that is not classifier.core.yml.
type rawYAML string

func (r rawYAML) source() ([]byte, error) { return []byte(r), nil }

// brokenWorkflow compiles and then fails at evaluation time: it uses the
// torrent name as a regex pattern, and the test feeds it a name that is not a
// valid regex. The pattern is not a constant, so CEL cannot fold it away at
// compile time — this produces a genuine runtime error from the real CEL
// engine, which is the failure mode the guard exists for (a mis-edited rule
// in classifier.core.yml that only blows up once it sees a torrent).
const brokenWorkflow = `
$schema: "https://bitmagnet.io/schemas/classifier-0.1.json"
workflows:
  default:
    - if_else:
        condition: "torrent.baseName.matches(torrent.baseName)"
        if_action: delete
`

// A workflow runtime error must fail its case, never be counted as a keep.
//
// A review finding: the harness derived `deleted` with a bare
// errors.Is(runErr, ErrDeleteTorrent), so ANY other error read as
// deleted == false. Every expect:keep case would then be scored a true
// negative, and a workflow that could not evaluate at all — broken CEL, a
// typo in a rule — would post zero false positives and pass the gate.
func TestCaseOutcomeRejectsUnexpectedWorkflowError(t *testing.T) {
	t.Parallel()

	mocks := newTestClassifierMocks(t)

	source, err := yamlSourceProvider{rawSourceProvider: rawYAML(brokenWorkflow)}.source()
	if err != nil {
		t.Fatalf("load broken source: %v", err)
	}

	workflow, err := mocks.compiler.Compile(source)
	if err != nil {
		t.Fatalf("compile broken workflow: %v", err)
	}

	// "*" is a quantifier with nothing to repeat — not a valid pattern.
	_, runErr := workflow.Run(context.Background(), "default", productionFlags(), model.Torrent{
		Name:        "*",
		FilesStatus: model.FilesStatusMulti,
	})

	if runErr == nil {
		t.Fatal("broken workflow ran without error: the test no longer exercises a runtime failure, so it proves nothing — pick a pattern CEL still rejects")
	}

	if errors.Is(runErr, classification.ErrDeleteTorrent) {
		t.Fatalf("broken workflow returned ErrDeleteTorrent (%v); the test needs an error that is NOT the delete sentinel", runErr)
	}

	deleted, outcomeErr := caseOutcome(runErr)
	if outcomeErr == nil {
		t.Fatalf("runtime error was swallowed: caseOutcome(%v) returned deleted=%v with no error. "+
			"An expect:keep case would score a true negative and the zero-false-positive gate would pass on a workflow that cannot run.", runErr, deleted)
	}

	if deleted {
		t.Errorf("caseOutcome reported deleted=true for a non-delete error %v", runErr)
	}
}

// The two normal outcomes must keep reading correctly — a guard that fails
// everything would be just as useless as one that fails nothing.
func TestCaseOutcomeNormalOutcomes(t *testing.T) {
	t.Parallel()

	if deleted, err := caseOutcome(nil); deleted || err != nil {
		t.Errorf("caseOutcome(nil) = (%v, %v), want (false, nil)", deleted, err)
	}

	// How action_delete.go actually returns it: wrapped, carrying the rule path.
	wrapped := classification.RuntimeError{
		Cause: classification.ErrDeleteTorrent,
		Path:  []string{"flags", "delete_xxx"},
	}
	if deleted, err := caseOutcome(wrapped); !deleted || err != nil {
		t.Errorf("caseOutcome(wrapped ErrDeleteTorrent) = (%v, %v), want (true, nil)", deleted, err)
	}

	if deleted, err := caseOutcome(classification.ErrDeleteTorrent); !deleted || err != nil {
		t.Errorf("caseOutcome(bare ErrDeleteTorrent) = (%v, %v), want (true, nil)", deleted, err)
	}

	// ErrUnmatched is NOT a keep. The core workflow's find_match swallows it
	// internally, so it never surfaces for the corpus; if it ever reaches the
	// top level the workflow reached no decision, and production treats it the
	// same way (processor.go routes it to failedHashes).
	if deleted, err := caseOutcome(classification.ErrUnmatched); deleted || err == nil {
		t.Errorf("caseOutcome(ErrUnmatched) = (%v, %v), want (false, non-nil)", deleted, err)
	}
}

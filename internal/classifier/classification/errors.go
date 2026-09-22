package classification

import (
	"fmt"
	"strings"

	"github.com/spencercnorton/bitagent/internal/model"
)

type Error interface {
	error
	Key() string
}

type WorkflowError struct {
	key     string
	message string
}

func (e WorkflowError) Error() string {
	if e.message != "" {
		return e.message
	}

	return fmt.Sprintf("workflow unmarshalError: %s", e.key)
}

func (e WorkflowError) Key() string {
	return e.key
}

var ErrUnmatched = WorkflowError{
	key: "unmatched",
}

var ErrDeleteTorrent = WorkflowError{
	key: "delete_torrent",
}

type RuntimeError struct {
	Path  []string
	Cause error
	// ContentType is the type the workflow had assigned when the error was
	// raised. The action-sequence runner returns an empty Result alongside any
	// error, so a delete's content type survives only here — it is what
	// bitagent_classifier_deleted_total{content_type} reports.
	ContentType model.NullContentType
}

func (e RuntimeError) Error() string {
	return fmt.Sprintf("runtime error at Path %s: %s", strings.Join(e.Path, "."), e.Cause)
}

func (e RuntimeError) Unwrap() error {
	return e.Cause
}

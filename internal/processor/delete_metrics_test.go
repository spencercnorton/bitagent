package processor

import (
	"errors"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
)

func TestDeleteLabels(t *testing.T) {
	typed := model.NewNullContentType(model.ContentTypeMusic)
	pathErr := classification.RuntimeError{
		Path:  []string{"workflows", "default", "actions", "3"},
		Cause: classification.ErrDeleteTorrent,
	}
	cases := []struct {
		name         string
		ct           model.NullContentType
		err          error
		wantCT, want string
	}{
		{"typed + rule path", typed, pathErr, "music", "workflows.default.actions.3"},
		{"untyped banned-keyword delete", model.NullContentType{}, pathErr, "unknown", "workflows.default.actions.3"},
		{"no runtime error wrapper", typed, classification.ErrDeleteTorrent, "music", "unknown"},
		{"wrapped runtime error", typed, errors.Join(errors.New("ctx"), pathErr), "music", "workflows.default.actions.3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ct, rule := deleteLabels(tc.ct, tc.err)
			if ct != tc.wantCT || rule != tc.want {
				t.Fatalf("got (%q,%q) want (%q,%q)", ct, rule, tc.wantCT, tc.want)
			}
		})
	}
	// Observe must be nil-safe: narrow wirings do not provide the metrics.
	var m *DeleteMetrics
	m.Observe(typed, pathErr)
}

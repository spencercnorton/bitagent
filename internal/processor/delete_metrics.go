package processor

import (
	"errors"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

// DeleteMetrics counts classifier-driven torrent deletes at the one place
// they become real — where the processor turns ErrDeleteTorrent into a row
// on infoHashesToDelete. Until this existed the `default` workflow's
// delete rules were the only armed destructive path with no metric at all
// (GROUND-TRUTH F-1): six content types sat at zero rows in a 7M-row
// catalogue and nothing reported the rate.
//
// Labels: content_type is the type the classifier had assigned before the
// delete fired ("unknown" when none — the banned-keyword rule runs before
// parsing); rule is the dotted policy path of the rule that fired, so a
// CSAM-keyword delete and a delete_content_types delete are separable.
type DeleteMetrics struct {
	deleted *dualemit.CounterVec
}

func NewDeleteMetrics() *DeleteMetrics {
	return &DeleteMetrics{
		deleted: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: "bitagent", Subsystem: "classifier", Name: "deleted_total",
			Help: "Torrents deleted by a classifier workflow rule, labelled by the content type " +
				"assigned before the delete and the dotted path of the rule that fired.",
		}, []string{"content_type", "rule"}),
	}
}

func (m *DeleteMetrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.deleted}
}

// Observe records one delete. Nil-safe so narrow wirings need not provide it.
func (m *DeleteMetrics) Observe(ct model.NullContentType, classifyErr error) {
	if m == nil {
		return
	}
	contentType, rule := deleteLabels(ct, classifyErr)
	m.deleted.WithLabelValues(contentType, rule).Inc()
}

func deleteLabels(ct model.NullContentType, classifyErr error) (contentType, rule string) {
	contentType, rule = "unknown", "unknown"
	if ct.Valid {
		contentType = ct.ContentType.String()
	}
	var re classification.RuntimeError
	if errors.As(classifyErr, &re) && len(re.Path) > 0 {
		rule = strings.Join(re.Path, ".")
	}
	return contentType, rule
}

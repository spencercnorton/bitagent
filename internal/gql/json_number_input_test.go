package gql

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTorrentContentSearchInputAcceptsJSONNumberAggregationBudget(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		json string
		want float64
	}{
		{name: "integer zero", json: `{"input":{"aggregationBudget":0}}`, want: 0},
		{name: "integer budget", json: `{"input":{"aggregationBudget":5000}}`, want: 5000},
		{name: "fractional budget", json: `{"input":{"aggregationBudget":0.5}}`, want: 0.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var body struct {
				Input map[string]any `json:"input"`
			}
			decoder := json.NewDecoder(strings.NewReader(tc.json))
			decoder.UseNumber()
			require.NoError(t, decoder.Decode(&body))

			var ec executionContext
			got, err := ec.unmarshalInputTorrentContentSearchQueryInput(context.Background(), body.Input)
			require.NoError(t, err)
			require.True(t, got.AggregationBudget.Valid)
			require.Equal(t, tc.want, got.AggregationBudget.Float64)
		})
	}
}

package model

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNullFloat64UnmarshalGQLJSONNumber(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		input json.Number
		want  float64
	}{
		{name: "zero", input: json.Number("0"), want: 0},
		{name: "decimal", input: json.Number("12.5"), want: 12.5},
		{name: "exponent", input: json.Number("1e3"), want: 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var got NullFloat64
			require.NoError(t, got.UnmarshalGQL(tc.input))
			require.True(t, got.Valid)
			require.Equal(t, tc.want, got.Float64)
		})
	}
}

func TestNullFloat32UnmarshalGQLJSONNumber(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		input json.Number
		want  float32
	}{
		{name: "zero", input: json.Number("0"), want: 0},
		{name: "decimal", input: json.Number("12.5"), want: 12.5},
		{name: "exponent", input: json.Number("1e3"), want: 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var got NullFloat32
			require.NoError(t, got.UnmarshalGQL(tc.input))
			require.True(t, got.Valid)
			require.Equal(t, tc.want, got.Float32)
		})
	}
}

func TestNullFloatJSONNumberErrorsAndNulls(t *testing.T) {
	t.Parallel()

	t.Run("float64 malformed", func(t *testing.T) {
		var got NullFloat64
		require.Error(t, got.UnmarshalGQL(json.Number("not-a-number")))
		require.False(t, got.Valid)
	})

	t.Run("float32 overflow", func(t *testing.T) {
		var got NullFloat32
		require.Error(t, got.UnmarshalGQL(json.Number("1e1000")))
		require.False(t, got.Valid)
	})

	t.Run("null clears validity", func(t *testing.T) {
		float64Value := NewNullFloat64(12.5)
		require.NoError(t, float64Value.UnmarshalGQL(nil))
		require.False(t, float64Value.Valid)

		float32Value := NewNullFloat32(12.5)
		require.NoError(t, float32Value.UnmarshalGQL(nil))
		require.False(t, float32Value.Valid)
	})
}

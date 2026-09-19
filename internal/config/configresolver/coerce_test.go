package configresolver

import (
	"reflect"
	"testing"
	"time"
)

func TestCoerceStringValue(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		typ     reflect.Type
		want    interface{}
		wantErr bool
	}{
		{"string", "hello", reflect.TypeOf(""), "hello", false},
		{"bool_true", "true", reflect.TypeOf(false), true, false},
		{"bool_1", "1", reflect.TypeOf(false), true, false},
		{"int", "42", reflect.TypeOf(0), 42, false},
		{"uint", "7", reflect.TypeOf(uint(0)), uint64(7), false},
		// Floats — the case this fix adds. A float env value (e.g.
		// CLASSIFIER_LLM_MATCH_MIN_CONFIDENCE) previously crashed config load.
		{"float64", "0.6", reflect.TypeOf(float64(0)), 0.6, false},
		{"float64_int_form", "1", reflect.TypeOf(float64(0)), 1.0, false},
		{"float32", "0.75", reflect.TypeOf(float32(0)), float32(0.75), false},
		{"float64_bad", "notafloat", reflect.TypeOf(float64(0)), nil, true},
		{"duration", "60s", reflect.TypeOf(time.Duration(0)), 60 * time.Second, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := coerceStringValue(c.in, c.typ)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %#v, want %#v", got, c.want)
			}
		})
	}
}

// TestCoerceFloatSlice guards the slice path recursing into the new float case.
func TestCoerceFloatSlice(t *testing.T) {
	got, err := coerceStringValue("0.1,0.2,0.3", reflect.TypeOf([]float64{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	vals, ok := got.([]interface{})
	if !ok || len(vals) != 3 {
		t.Fatalf("got %#v", got)
	}
	if vals[0].(float64) != 0.1 || vals[2].(float64) != 0.3 {
		t.Fatalf("bad slice values: %#v", vals)
	}
}

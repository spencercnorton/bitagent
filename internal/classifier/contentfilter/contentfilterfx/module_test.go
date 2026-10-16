package contentfilterfx

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"go.uber.org/zap"
)

// TestApplyDropLossyAudioOnlyBackCompat pins the legacy-env shim that lets a
// running deploy still set the pre-rename CONTENT_FILTER_DROP_MP_3_ONLY var
// (the old binding for what is now DropLossyAudioOnly) without silently
// changing behavior on upgrade. Precedence: the new env var always wins; the
// legacy var is honored only when the new one is unset.
func TestApplyDropLossyAudioOnlyBackCompat(t *testing.T) {
	logger := zap.NewNop().Sugar()

	cases := []struct {
		name     string
		newEnv   *string // nil = unset
		oldEnv   *string // nil = unset
		startVal bool    // cfg.DropLossyAudioOnly as the resolver left it
		want     bool
	}{
		{
			name:     "neither set keeps resolver value (default true)",
			startVal: true,
			want:     true,
		},
		{
			name:     "legacy false honored when new unset",
			oldEnv:   ptr("false"),
			startVal: true,
			want:     false,
		},
		{
			name:     "new false wins, legacy true ignored",
			newEnv:   ptr("false"),
			oldEnv:   ptr("true"),
			startVal: false, // resolver already applied the new var
			want:     false,
		},
		{
			name:     "legacy unparseable is ignored, resolver value survives",
			oldEnv:   ptr("notabool"),
			startVal: true,
			want:     true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.newEnv != nil {
				t.Setenv("CONTENT_FILTER_DROP_LOSSY_AUDIO_ONLY", *c.newEnv)
			}
			if c.oldEnv != nil {
				t.Setenv("CONTENT_FILTER_DROP_MP_3_ONLY", *c.oldEnv)
			}
			cfg := contentfilter.NewDefaultConfig()
			cfg.DropLossyAudioOnly = c.startVal
			got := applyDropLossyAudioOnlyBackCompat(cfg, logger)
			if got.DropLossyAudioOnly != c.want {
				t.Fatalf("DropLossyAudioOnly = %v, want %v", got.DropLossyAudioOnly, c.want)
			}
		})
	}
}

func ptr(s string) *string { return &s }

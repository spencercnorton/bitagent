package config

import (
	"github.com/spencercnorton/bitagent/internal/serving"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestServingAdultExclusionIsExplicitAndDefaultsOff(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		env := map[string]string{}
		if enabled {
			env["SERVING_EXCLUDE_ADULT"] = "true"
		}
		cfg := resolveSectionFromEnv(t, "serving", serving.Config{}, env).(serving.Config)
		require.Equal(t, enabled, cfg.ExcludeAdult)
	}
}

package backloglocalclassifycmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocalOnlyFlagsDisableExternalResolution(t *testing.T) {
	t.Parallel()

	flags := localOnlyFlags()

	require.Equal(t, false, flags["apis_enabled"])
	require.Equal(t, false, flags["llm_match_enabled"])
	require.Equal(t, false, flags["llm_stage_enabled"])
}

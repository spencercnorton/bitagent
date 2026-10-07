package policyshadowcmd

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPolicyShadowCursorBounds(t *testing.T) {
	for _, s := range []string{"abc", "not-a-hash", "ffffffffffffffffffffffffffffffffffffffzz"} {
		_, err := parseCursor(s)
		require.Error(t, err)
	}
	h, err := parseCursor("1111111111111111111111111111111111111111")
	require.NoError(t, err)
	require.Len(t, h, 20)
	h, err = parseCursor("")
	require.NoError(t, err)
	require.Empty(t, h)
	c := New(Params{}).Command
	require.Equal(t, "catalogue-policy-shadow", c.Name)
}

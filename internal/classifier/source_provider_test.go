package classifier

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfigSourceProviderDeleteContentTypesFlag(t *testing.T) {
	t.Parallel()

	cfg := NewDefaultConfig()
	cfg.DeleteContentTypes = []string{"music", "ebook"}

	src, err := configSourceProvider{config: cfg, tmdbEnabled: true}.source()
	require.NoError(t, err)
	require.Equal(t, []any{"music", "ebook"}, src.Flags["delete_content_types"])
}

func TestConfigSourceProviderDeleteContentTypesUnsetWhenEmpty(t *testing.T) {
	t.Parallel()

	src, err := configSourceProvider{config: NewDefaultConfig(), tmdbEnabled: true}.source()
	require.NoError(t, err)

	_, ok := src.Flags["delete_content_types"]
	require.False(t, ok)
}

func TestDeleteContentTypesFlagDecodesAsContentTypeList(t *testing.T) {
	t.Parallel()

	cfg := NewDefaultConfig()
	cfg.DeleteContentTypes = []string{"music", "ebook", "audiobook", "comic", "game", "software"}

	src, err := configSourceProvider{config: cfg, tmdbEnabled: true}.source()
	require.NoError(t, err)

	_, err = FlagTypeContentTypeList.celVal(src.Flags["delete_content_types"])
	require.NoError(t, err)
}

func TestDeleteContentTypesFlagRejectsInvalidTypeAtDecode(t *testing.T) {
	t.Parallel()

	cfg := NewDefaultConfig()
	cfg.DeleteContentTypes = []string{"musik"}

	src, err := configSourceProvider{config: cfg, tmdbEnabled: true}.source()
	require.NoError(t, err)

	_, err = FlagTypeContentTypeList.celVal(src.Flags["delete_content_types"])
	require.ErrorContains(t, err, "could not parse content type")
}

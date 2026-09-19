package csamblocklist

import (
	"path/filepath"
	"testing"
)

// TestDefaultExportFilePath_XDG pins the write-safe default-path
// resolution. The exporter uses ExportFilePath verbatim (no data-root
// resolution), so the relative "data/..." default fails under the
// production image (no WORKDIR → CWD "/", non-root uid → mkdir denied).
// When XDG_CONFIG_HOME is set we must default to an absolute path under
// it; otherwise we keep the legacy relative fallback for dev `go run`.
func TestDefaultExportFilePath_XDG(t *testing.T) {
	t.Run("xdg set -> absolute path under it", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "/config")
		want := filepath.Join("/config", "csam", "csam-double-hashes.jsonl")
		if got := defaultExportFilePath(); got != want {
			t.Fatalf("defaultExportFilePath() = %q, want %q", got, want)
		}
		// And NewDefaultConfig must surface the same value.
		if got := NewDefaultConfig().ExportFilePath; got != want {
			t.Fatalf("NewDefaultConfig().ExportFilePath = %q, want %q", got, want)
		}
	})

	t.Run("xdg set to non-/config -> joined under it", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "/home/user/.config")
		want := filepath.Join("/home/user/.config", "csam", "csam-double-hashes.jsonl")
		if got := defaultExportFilePath(); got != want {
			t.Fatalf("defaultExportFilePath() = %q, want %q", got, want)
		}
	})

	t.Run("xdg empty -> legacy relative fallback", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "")
		want := "data/csam-double-hashes.jsonl"
		if got := defaultExportFilePath(); got != want {
			t.Fatalf("defaultExportFilePath() = %q, want %q", got, want)
		}
		if got := NewDefaultConfig().ExportFilePath; got != want {
			t.Fatalf("NewDefaultConfig().ExportFilePath = %q, want %q", got, want)
		}
	})
}

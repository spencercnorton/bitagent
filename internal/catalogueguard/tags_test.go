package catalogueguard

import "testing"

func TestQualifiedProtectionAndWantedMatcherPolicy(t *testing.T) {
	for _, prefix := range []string{"manual", "reference", "bitgrab", "wanted"} {
		for _, suffix := range []string{"", ":record", "/record", "-record", "_record"} {
			name := " \t" + prefix + suffix + "\r\n"
			if !ProtectedTag(name, true) {
				t.Errorf("maintenance failed to protect %q", name)
			}
			if got := ProtectedTag(name, false); got != (prefix != "wanted") {
				t.Errorf("matcher policy for %q = %t", name, got)
			}
		}
	}
	for _, name := range []string{"manualish", "referenceable", "bitgrabbing", "wantedness", "ordinary", "manually:record"} {
		if ProtectedTag(name, true) {
			t.Errorf("ordinary tag protected: %q", name)
		}
	}
	if !ProtectedTag(" \tMaNuAl_note\r\n", true) {
		t.Fatal("case/whitespace qualification lost")
	}
}

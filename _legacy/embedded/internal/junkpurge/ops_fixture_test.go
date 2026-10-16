package junkpurge

import (
	"os"
	"testing"
)

// opsFixture returns rel unchanged when the private ops fixture exists and
// skips the test when it does not: the ops/ corpus is not part of the public
// source tree, so a public checkout runs every other test and skips these.
func opsFixture(t *testing.T, rel string) string {
	t.Helper()
	if _, err := os.Stat(rel); err != nil {
		t.Skipf("private ops fixture %s is not present in this checkout", rel)
	}
	return rel
}

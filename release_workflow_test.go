package nuntius

import (
	"os"
	"strings"
	"testing"
)

func TestReleaseVerificationNodeParity(t *testing.T) {
	for _, path := range []string{".github/workflows/verify.yml", ".github/workflows/release.yml"} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		if !strings.Contains(text, "actions/setup-node@949feb2413d6458794dcd2491c4babbbce0c15c1") || !strings.Contains(text, "node-version: '26.8.1'") {
			t.Fatal("verification requires the pinned offline Node runtime")
		}
		if strings.Index(text, "actions/setup-node@") > strings.Index(text, "run: make verify") {
			t.Fatal("Node setup must precede verification")
		}
	}
}

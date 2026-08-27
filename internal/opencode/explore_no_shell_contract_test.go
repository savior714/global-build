package opencode

import (
	"strings"
	"testing"
)

func TestCanonicalWorkerDelegatesExploreWithoutShell(t *testing.T) {
	body, err := CanonicalWorkerBody()
	if err != nil {
		t.Fatalf("CanonicalWorkerBody: %v", err)
	}

	for _, required := range []string{
		"only OpenCode's dedicated `list`, `glob`, `grep`, and `read` tools",
		"Do not use\n  `bash` or any shell command, even for read-only discovery",
		"do not duplicate that investigation",
		"primary worker is the sole mutation owner",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("canonical worker prompt missing Explore no-shell contract fragment %q", required)
		}
	}

	if strings.Contains(body, "do not run a\n  shell command whose purpose is mutation") {
		t.Error("canonical worker still permits read-only shell discovery in delegated Explore prompts")
	}
}

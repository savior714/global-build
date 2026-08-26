package runner

import (
	"strings"
	"testing"

	"global-build/internal/opencode"
)

// TestAgentFinalProtocolExamplesAreBare guards the exact-output contract against
// Markdown examples that can train a worker model to emit fenced protocol text.
func TestAgentFinalProtocolExamplesAreBare(t *testing.T) {
	raw, err := opencode.EmbeddedWorkerSource()
	if err != nil {
		t.Fatalf("cannot read embedded worker source: %v", err)
	}
	parts := strings.SplitN(string(raw), "---", 3)
	if len(parts) < 3 {
		t.Fatal("agent file lacks frontmatter delimiters")
	}
	body := parts[2]

	if strings.Contains(body, "```") {
		t.Fatal("worker final-protocol examples must not contain Markdown code fences")
	}
	for _, required := range []string{
		"Do NOT wrap the protocol in Markdown code fences",
		"first non-whitespace characters of the final assistant message must be `RESULT:`",
		"RESULT: COMPLETE\nPRIMARY_PROOF: PASS",
		"RESULT: CONTINUABLE\nADMITTED_BASE:",
		"RESULT: BLOCKED\nBLOCKER:",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("worker body missing bare-protocol guard %q", required)
		}
	}
}

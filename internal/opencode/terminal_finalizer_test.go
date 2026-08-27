package opencode

import (
	"encoding/json"
	"testing"
)

func TestSessionIDFromEventLine(t *testing.T) {
	id, ok := sessionIDFromEventLine(`{"type":"step_start","timestamp":1,"sessionID":"ses_exact","part":{"id":"p1"}}`)
	if !ok || id != "ses_exact" {
		t.Fatalf("sessionIDFromEventLine = %q, %v; want ses_exact, true", id, ok)
	}
	if _, ok := sessionIDFromEventLine(`not-json`); ok {
		t.Fatal("invalid event line unexpectedly produced a session id")
	}
	if _, ok := sessionIDFromEventLine(`{"type":"step_start","sessionID":""}`); ok {
		t.Fatal("empty session id unexpectedly accepted")
	}
}

func TestBuildFinalizerInlineConfigIsToollessAndPreservesWorkerModel(t *testing.T) {
	existing := `{
  "agent": {
    "global-build-worker": {
      "model": "anthropic/claude-opus-4",
      "variant": "high"
    }
  }
}`
	merged, err := BuildFinalizerInlineConfig(existing, t.TempDir())
	if err != nil {
		t.Fatalf("BuildFinalizerInlineConfig: %v", err)
	}

	var root struct {
		Agent map[string]json.RawMessage `json:"agent"`
	}
	if err := json.Unmarshal([]byte(merged), &root); err != nil {
		t.Fatalf("generated config invalid: %v", err)
	}
	raw, ok := root.Agent[FinalizerAgentName]
	if !ok {
		t.Fatalf("finalizer agent missing:\n%s", merged)
	}
	var finalizer struct {
		Mode       string            `json:"mode"`
		Steps      int               `json:"steps"`
		Prompt     string            `json:"prompt"`
		Model      string            `json:"model"`
		Variant    string            `json:"variant"`
		Permission map[string]string `json:"permission"`
	}
	if err := json.Unmarshal(raw, &finalizer); err != nil {
		t.Fatalf("finalizer block invalid: %v", err)
	}
	if finalizer.Mode != "primary" || finalizer.Steps != 2 {
		t.Errorf("finalizer execution shape = mode %q steps %d", finalizer.Mode, finalizer.Steps)
	}
	if finalizer.Model != "anthropic/claude-opus-4" || finalizer.Variant != "high" {
		t.Errorf("finalizer model/variant not preserved: %q/%q", finalizer.Model, finalizer.Variant)
	}
	if len(finalizer.Permission) != 1 || finalizer.Permission["*"] != "deny" {
		t.Errorf("finalizer permission must be exact deny-all, got %#v", finalizer.Permission)
	}
	if finalizer.Prompt == "" {
		t.Error("finalizer prompt missing")
	}

	// The normal Explore restriction must remain present in the finalizer config.
	explorePerm, _ := explorePermission(t, merged)
	assertCanonicalExplorePermission(t, explorePerm)
}

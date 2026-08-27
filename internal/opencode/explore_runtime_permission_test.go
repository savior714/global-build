package opencode

import (
	"encoding/json"
	"testing"
)

func explorePermission(t *testing.T, content string, agentName string) (map[string]json.RawMessage, map[string]json.RawMessage) {
	t.Helper()
	var root struct {
		Agent map[string]json.RawMessage `json:"agent"`
	}
	if err := json.Unmarshal([]byte(content), &root); err != nil {
		t.Fatalf("generated config is not valid JSON: %v\n%s", err, content)
	}
	raw, ok := root.Agent[agentName]
	if !ok {
		t.Fatalf("agent %s missing in generated config:\n%s", agentName, content)
	}
	var explore map[string]json.RawMessage
	if err := json.Unmarshal(raw, &explore); err != nil {
		t.Fatalf("explore agent block invalid: %v", err)
	}
	var permission map[string]json.RawMessage
	if err := json.Unmarshal(explore["permission"], &permission); err != nil {
		t.Fatalf("explore permission block invalid: %v", err)
	}
	return permission, explore
}

func assertCanonicalExplorePermission(t *testing.T, permission map[string]json.RawMessage) {
	t.Helper()
	want := map[string]string{
		"*":    "deny",
		"glob": "allow",
		"grep": "allow",
		"list": "allow",
		"read": "allow",
	}
	for key, wantValue := range want {
		raw, ok := permission[key]
		if !ok {
			t.Errorf("explore permission[%q] missing", key)
			continue
		}
		var got string
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("explore permission[%q] is not a string: %v", key, err)
			continue
		}
		if got != wantValue {
			t.Errorf("explore permission[%q] = %q, want %q", key, got, wantValue)
		}
	}
}

func TestBuildInlineConfigInjectsCanonicalExploreAgent(t *testing.T) {
	merged, err := BuildInlineConfig("", t.TempDir())
	if err != nil {
		t.Fatalf("BuildInlineConfig: %v", err)
	}

	// The canonical global-build-explore agent must be present with correct
	// permission, mode, steps, and prompt.
	permission, explore := explorePermission(t, merged, GlobalBuildExploreAgentName)
	assertCanonicalExplorePermission(t, permission)

	var mode string
	if err := json.Unmarshal(explore["mode"], &mode); err != nil {
		t.Fatalf("explore mode invalid: %v", err)
	}
	if mode != "subagent" {
		t.Errorf("explore mode = %q, want subagent", mode)
	}
	var steps int
	if err := json.Unmarshal(explore["steps"], &steps); err != nil {
		t.Fatalf("explore steps invalid: %v", err)
	}
	if steps != 10 {
		t.Errorf("explore steps = %d, want 10", steps)
	}
	var prompt string
	if err := json.Unmarshal(explore["prompt"], &prompt); err != nil {
		t.Fatalf("explore prompt invalid: %v", err)
	}
	if prompt == "" {
		t.Error("explore prompt is empty")
	}

	// The legacy built-in `explore` agent must NOT be injected when there is no
	// existing config (it should only appear if the caller already had it).
	var root struct {
		Agent map[string]json.RawMessage `json:"agent"`
	}
	if err := json.Unmarshal([]byte(merged), &root); err != nil {
		t.Fatalf("generated config is not valid JSON: %v\n%s", err, merged)
	}
	if _, exists := root.Agent[ExploreAgentName]; exists {
		t.Error("legacy explore agent must not be injected when no existing config provides it")
	}
}

func TestBuildInlineConfigOverridesStaleExplorePermissionButPreservesMetadata(t *testing.T) {
	existing := `{
  "agent": {
    "explore": {
      "model": "anthropic/claude-opus-4",
      "variant": "fast",
      "steps": 17,
      "prompt": "CUSTOM EXPLORE PROMPT",
      "permission": {
        "bash": "allow",
        "webfetch": "allow",
        "websearch": "allow",
        "edit": "allow",
        "task": "allow",
        "read": "deny"
      }
    }
  }
}`

	merged, err := BuildInlineConfig(existing, t.TempDir())
	if err != nil {
		t.Fatalf("BuildInlineConfig: %v", err)
	}

	// Legacy explore agent permission is overridden.
	permission, explore := explorePermission(t, merged, ExploreAgentName)
	assertCanonicalExplorePermission(t, permission)

	// Non-permission metadata is preserved.
	for key, want := range map[string]string{
		"model":  "anthropic/claude-opus-4",
		"variant": "fast",
		"prompt": "CUSTOM EXPLORE PROMPT",
	} {
		var got string
		if err := json.Unmarshal(explore[key], &got); err != nil {
			t.Fatalf("explore %s invalid: %v", key, err)
		}
		if got != want {
			t.Errorf("explore %s = %q, want %q (preserved)", key, got, want)
		}
	}
	var steps int
	if err := json.Unmarshal(explore["steps"], &steps); err != nil {
		t.Fatalf("explore steps invalid: %v", err)
	}
	if steps != 17 {
		t.Errorf("explore steps = %d, want 17 (preserved)", steps)
	}

	// Canonical global-build-explore agent is also injected.
	canonPerm, canonExplore := explorePermission(t, merged, GlobalBuildExploreAgentName)
	assertCanonicalExplorePermission(t, canonPerm)
	var canonMode string
	if err := json.Unmarshal(canonExplore["mode"], &canonMode); err != nil {
		t.Fatalf("canonical explore mode invalid: %v", err)
	}
	if canonMode != "subagent" {
		t.Errorf("canonical explore mode = %q, want subagent", canonMode)
	}
}

func TestBuildInlineConfigRejectsMalformedExploreBlock(t *testing.T) {
	existing := `{"agent":{"explore":"not-an-object"}}`
	if _, err := BuildInlineConfig(existing, t.TempDir()); err == nil {
		t.Fatal("BuildInlineConfig accepted malformed explore agent block")
	}
}

func TestBuildInlineConfigRejectsMalformedCanonicalExploreBlock(t *testing.T) {
	// Stale config that provides a non-object for the canonical agent must fail closed.
	existing := `{"agent":{"global-build-explore":"not-an-object"}}`
	if _, err := BuildInlineConfig(existing, t.TempDir()); err == nil {
		t.Fatal("BuildInlineConfig accepted malformed global-build-explore agent block")
	}
}

package opencode

import (
	"encoding/json"
	"testing"
)

func explorePermission(t *testing.T, content string) (map[string]string, map[string]json.RawMessage) {
	t.Helper()
	var root struct {
		Agent map[string]json.RawMessage `json:"agent"`
	}
	if err := json.Unmarshal([]byte(content), &root); err != nil {
		t.Fatalf("generated config is not valid JSON: %v\n%s", err, content)
	}
	raw, ok := root.Agent[ExploreAgentName]
	if !ok {
		t.Fatalf("agent %s missing in generated config:\n%s", ExploreAgentName, content)
	}
	var explore map[string]json.RawMessage
	if err := json.Unmarshal(raw, &explore); err != nil {
		t.Fatalf("explore agent block invalid: %v", err)
	}
	var permission map[string]string
	if err := json.Unmarshal(explore["permission"], &permission); err != nil {
		t.Fatalf("explore permission block invalid: %v", err)
	}
	return permission, explore
}

func assertCanonicalExplorePermission(t *testing.T, permission map[string]string) {
	t.Helper()
	want := map[string]string{
		"*":    "deny",
		"glob": "allow",
		"grep": "allow",
		"list": "allow",
		"read": "allow",
	}
	if len(permission) != len(want) {
		t.Fatalf("explore permission = %#v, want exactly %#v", permission, want)
	}
	for key, wantValue := range want {
		if got := permission[key]; got != wantValue {
			t.Errorf("explore permission[%q] = %q, want %q", key, got, wantValue)
		}
	}
}

func TestBuildInlineConfigRestrictsExploreToDedicatedReadTools(t *testing.T) {
	merged, err := BuildInlineConfig("", t.TempDir())
	if err != nil {
		t.Fatalf("BuildInlineConfig: %v", err)
	}
	permission, _ := explorePermission(t, merged)
	assertCanonicalExplorePermission(t, permission)
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
	permission, explore := explorePermission(t, merged)
	assertCanonicalExplorePermission(t, permission)

	for key, want := range map[string]string{
		"model":   "anthropic/claude-opus-4",
		"variant": "fast",
		"prompt":  "CUSTOM EXPLORE PROMPT",
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
}

func TestBuildInlineConfigRejectsMalformedExploreBlock(t *testing.T) {
	existing := `{"agent":{"explore":"not-an-object"}}`
	if _, err := BuildInlineConfig(existing, t.TempDir()); err == nil {
		t.Fatal("BuildInlineConfig accepted malformed explore agent block")
	}
}

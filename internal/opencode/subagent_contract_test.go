package opencode

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCanonicalWorkerAllowsOnlyExploreSubagent(t *testing.T) {
	stale := `{
  "agent": {
    "global-build-worker": {
      "permission": {
        "task": "allow"
      }
    }
  }
}`

	merged, err := BuildInlineConfig(stale, t.TempDir())
	if err != nil {
		t.Fatalf("BuildInlineConfig: %v", err)
	}

	var root struct {
		Agent map[string]struct {
			Prompt     string                     `json:"prompt"`
			Permission map[string]json.RawMessage `json:"permission"`
		} `json:"agent"`
	}
	if err := json.Unmarshal([]byte(merged), &root); err != nil {
		t.Fatalf("generated config is not valid JSON: %v\n%s", err, merged)
	}

	worker, ok := root.Agent[AgentName]
	if !ok {
		t.Fatalf("agent %s missing in generated config", AgentName)
	}

	var task map[string]string
	if raw := worker.Permission["task"]; raw == nil {
		t.Fatal("canonical task permission missing")
	} else if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatalf("canonical task permission is not a map: %v", err)
	}

	if got := task["*"]; got != "deny" {
		t.Errorf("task[*] = %q, want deny", got)
	}
	if got := task["global-build-explore"]; got != "allow" {
		t.Errorf("task[global-build-explore] = %q, want allow", got)
	}
	if len(task) != 2 {
		t.Errorf("task permission must admit only global-build-explore; got %v", task)
	}
	for _, forbidden := range []string{"general", "scout", "build", "plan"} {
		if got, exists := task[forbidden]; exists && got != "deny" {
			t.Errorf("task[%s] = %q, must not be admitted", forbidden, got)
		}
	}

	for _, required := range []string{
		"hard maximum of three Explore calls total in this BUILD",
		"launch them in parallel in the same turn",
		"do not duplicate that investigation",
		"primary worker is the sole mutation owner",
		"Every delegated prompt must explicitly require read-only investigation",
	} {
		if !strings.Contains(worker.Prompt, required) {
			t.Errorf("canonical worker prompt missing delegation contract fragment %q", required)
		}
	}
}

func TestCanonicalTaskAllowListOverridesStaleBroadTaskMap(t *testing.T) {
	stale := `{
  "agent": {
    "global-build-worker": {
      "permission": {
        "task": {
          "*": "allow",
          "general": "allow",
          "scout": "allow"
        }
      }
    }
  }
}`

	merged, err := BuildInlineConfig(stale, t.TempDir())
	if err != nil {
		t.Fatalf("BuildInlineConfig: %v", err)
	}

	var root struct {
		Agent map[string]struct {
			Permission map[string]json.RawMessage `json:"permission"`
		} `json:"agent"`
	}
	if err := json.Unmarshal([]byte(merged), &root); err != nil {
		t.Fatalf("generated config is not valid JSON: %v", err)
	}

	var task map[string]string
	if err := json.Unmarshal(root.Agent[AgentName].Permission["task"], &task); err != nil {
		t.Fatalf("canonical task permission is not a map: %v", err)
	}
	want := map[string]string{"*": "deny", "global-build-explore": "allow"}
	if len(task) != len(want) {
		t.Fatalf("canonical task permission = %v, want %v", task, want)
	}
	for key, value := range want {
		if task[key] != value {
			t.Errorf("task[%s] = %q, want %q", key, task[key], value)
		}
	}
}

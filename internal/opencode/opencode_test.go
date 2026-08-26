package opencode

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"global-build/internal/gitx"
)

// --- helpers ----------------------------------------------------------------

// extractConfigContent pulls the OPENCODE_CONFIG_CONTENT value from an env slice.
func extractConfigContent(env []string) string {
	prefix := EnvConfigContent + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix)
		}
	}
	return ""
}

// workerExtDir parses only the global-build-worker external_directory map out of
// a generated config string.
func workerExtDir(t *testing.T, content string) map[string]string {
	t.Helper()
	var root struct {
		Agent map[string]struct {
			Permission struct {
				ExternalDirectory map[string]string `json:"external_directory"`
			} `json:"permission"`
		} `json:"agent"`
	}
	if err := json.Unmarshal([]byte(content), &root); err != nil {
		t.Fatalf("generated config is not valid JSON: %v\n%s", err, content)
	}
	w, ok := root.Agent[AgentName]
	if !ok {
		t.Fatalf("agent %s missing in generated config:\n%s", AgentName, content)
	}
	return w.Permission.ExternalDirectory
}

func mustContain(t *testing.T, m map[string]string, key, want string) {
	t.Helper()
	got, ok := m[key]
	if !ok {
		t.Errorf("expected external_directory rule %q present; got keys %v", key, keysOf(m))
		return
	}
	if got != want {
		t.Errorf("external_directory[%q] = %q, want %q", key, got, want)
	}
}

func mustNotContain(t *testing.T, m map[string]string, key string) {
	t.Helper()
	if _, ok := m[key]; ok {
		t.Errorf("external_directory rule %q must NOT be present; got keys %v", key, keysOf(m))
	}
}

func keysOf(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// canonicalAllowKey mirrors the runner's canonical worktree identity (the same
// gitx.CanonicalPath the runner uses) and returns the exact allow glob the
// inline config must produce for runID under an existing base directory.
func canonicalAllowKey(t *testing.T, base, runID string) (wt, allowKey string) {
	t.Helper()
	wt = filepath.Join(base, runID)
	// The runner always canonicalizes against an existing parent (preflight
	// MkdirAlls the worktree parent), so this resolves with a leading slash.
	canon := gitx.CanonicalPath(wt)
	allowKey = path.Join(canon, "**")
	return wt, allowKey
}

// --- proof 1: child environment allows exactly the current worktree ---------

func TestChildEnvAllowsExactWorktree(t *testing.T) {
	t.Setenv(EnvConfigContent, "")
	base := t.TempDir()
	wt, allowKey := canonicalAllowKey(t, base, "run-a")

	childEnv, err := buildChildEnv(wt)
	if err != nil {
		t.Fatalf("buildChildEnv: %v", err)
	}
	content := extractConfigContent(childEnv)
	if content == "" {
		t.Fatal("OPENCODE_CONFIG_CONTENT not present in child environment")
	}

	ext := workerExtDir(t, content)
	mustContain(t, ext, "*", "deny")
	mustContain(t, ext, allowKey, "allow")
}

// --- proof 2: sibling / broad paths are denied -------------------------------

func TestChildEnvDeniesSiblingsAndBroadPaths(t *testing.T) {
	t.Setenv(EnvConfigContent, "")
	base := t.TempDir()
	wt, allowKey := canonicalAllowKey(t, base, "run-a")

	childEnv, err := buildChildEnv(wt)
	if err != nil {
		t.Fatalf("buildChildEnv: %v", err)
	}
	ext := workerExtDir(t, extractConfigContent(childEnv))

	// The only allow rule is the exact current worktree.
	mustContain(t, ext, allowKey, "allow")

	// Never broad, never sibling, never other system roots.
	canonBase := gitx.CanonicalPath(base)
	mustNotContain(t, ext, path.Join(canonBase, "run-b", "**"))
	mustNotContain(t, ext, path.Join(canonBase, "**"))
	mustNotContain(t, ext, "/tmp/**")
	mustNotContain(t, ext, "/var/tmp/**")

	home, herr := os.UserHomeDir()
	if herr == nil && home != "" {
		mustNotContain(t, ext, path.Join(home, "**"))
		// Cache root broadly is also not granted.
		mustNotContain(t, ext, path.Join(home, "Library", "Caches", "global-build", "worktrees", "**"))
	}
}

// --- proof 3: existing inline config is preserved ----------------------------

func TestExistingInlineConfigPreserved(t *testing.T) {
	existing := `{
  "model": { "provider": "anthropic", "name": "claude" },
  "agent": {
    "global-build-worker": {
      "permission": { "edit": "allow", "shell": "ask" }
    },
    "some-other-agent": {
      "permission": { "external_directory": { "/elsewhere/**": "allow" } }
    }
  }
}`
	t.Setenv(EnvConfigContent, existing)
	base := t.TempDir()
	wt, allowKey := canonicalAllowKey(t, base, "run-a")

	merged, err := BuildInlineConfig(existing, wt)
	if err != nil {
		t.Fatalf("BuildInlineConfig: %v", err)
	}

	// Top-level unrelated field preserved.
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(merged), &root); err != nil {
		t.Fatalf("merged config invalid: %v", err)
	}
	if _, ok := root["model"]; !ok {
		t.Errorf("top-level 'model' field was dropped during merge:\n%s", merged)
	}

	// Other agent untouched (including its external_directory allow).
	var agents map[string]json.RawMessage
	if err := json.Unmarshal(root["agent"], &agents); err != nil {
		t.Fatalf("agent block invalid: %v", err)
	}
	var other struct {
		Permission struct {
			ExternalDirectory map[string]string `json:"external_directory"`
		} `json:"permission"`
	}
	if err := json.Unmarshal(agents["some-other-agent"], &other); err != nil {
		t.Fatalf("some-other-agent invalid: %v", err)
	}
	mustContain(t, other.Permission.ExternalDirectory, "/elsewhere/**", "allow")

	// Worker's unrelated permission keys preserved, override only replaces
	// external_directory.
	var worker struct {
		Permission map[string]json.RawMessage `json:"permission"`
	}
	if err := json.Unmarshal(agents[AgentName], &worker); err != nil {
		t.Fatalf("worker block invalid: %v", err)
	}
	if _, ok := worker.Permission["edit"]; !ok {
		t.Errorf("worker permission 'edit' dropped during merge:\n%s", merged)
	}
	if _, ok := worker.Permission["shell"]; !ok {
		t.Errorf("worker permission 'shell' dropped during merge:\n%s", merged)
	}

	// And the override is effective.
	ext := workerExtDir(t, merged)
	mustContain(t, ext, allowKey, "allow")
	mustContain(t, ext, "*", "deny")
}

// --- proof 4: malformed existing config fails closed -------------------------

func TestMalformedExistingConfigFailsClosed(t *testing.T) {
	// The merged product must not be produced from broken input.
	if _, err := BuildInlineConfig("{ this is not json", "/wt/run-a"); err == nil {
		t.Fatal("BuildInlineConfig accepted malformed existing content")
	}

	// And the spawn environment builder must refuse before model execution.
	t.Setenv(EnvConfigContent, "{ broken")
	if _, err := buildChildEnv("/wt/run-a"); err == nil {
		t.Fatal("buildChildEnv accepted malformed existing OPENCODE_CONFIG_CONTENT")
	}
}

// --- proof 5: override applies only to global-build-worker -------------------

func TestOverrideScopedToWorkerAgent(t *testing.T) {
	existing := `{
  "agent": {
    "global-build-worker": { "permission": { "edit": "allow" } },
    "partner-agent": {
      "permission": { "external_directory": { "/partner-space/**": "allow", "*": "deny" } }
    }
  }
}`
	t.Setenv(EnvConfigContent, existing)
	base := t.TempDir()
	wt, allowKey := canonicalAllowKey(t, base, "run-a")

	merged, err := BuildInlineConfig(existing, wt)
	if err != nil {
		t.Fatalf("BuildInlineConfig: %v", err)
	}

	var root struct {
		Agent map[string]struct {
			Permission struct {
				ExternalDirectory map[string]string `json:"external_directory"`
			} `json:"permission"`
		} `json:"agent"`
	}
	if err := json.Unmarshal([]byte(merged), &root); err != nil {
		t.Fatalf("merged config invalid: %v", err)
	}

	// Worker gets the run-specific override.
	mustContain(t, root.Agent[AgentName].Permission.ExternalDirectory, allowKey, "allow")

	// partner-agent is entirely unchanged.
	partner := root.Agent["partner-agent"].Permission.ExternalDirectory
	if _, ok := partner[allowKey]; ok {
		t.Errorf("override leaked into partner-agent:\n%s", merged)
	}
	mustContain(t, partner, "/partner-space/**", "allow")
	mustContain(t, partner, "*", "deny")
}

// --- proof: empty existing config produces a valid scoped config -------------

func TestEmptyExistingConfigIsValid(t *testing.T) {
	t.Setenv(EnvConfigContent, "")
	base := t.TempDir()
	wt, allowKey := canonicalAllowKey(t, base, "run-a")

	merged, err := BuildInlineConfig("", wt)
	if err != nil {
		t.Fatalf("BuildInlineConfig: %v", err)
	}
	ext := workerExtDir(t, merged)
	mustContain(t, ext, "*", "deny")
	mustContain(t, ext, allowKey, "allow")
	// No global external_directory: allow anywhere.
	if strings.Contains(merged, `"external_directory": "allow"`) {
		t.Errorf("global external_directory: allow must never be set:\n%s", merged)
	}
}

// --- proof: canonical worker overrides stale inline config -------------------

func TestCanonicalWorkerOverridesStaleConfig(t *testing.T) {
	// Simulate an invocation that carries stale worker fields from a prior
	// run or from the installed home-directory agent file: steps still at 35,
	// websearch weakened to allow, and an invocation-specific model value.
	stale := `{
  "model": { "provider": "openai", "name": "gpt-5" },
  "agent": {
    "global-build-worker": {
      "steps": 35,
      "mode": "secondary",
      "permission": {
        "edit": "allow",
        "websearch": "allow",
        "shell": "ask"
      }
    },
    "some-other-agent": {
      "description": "untouched",
      "permission": { "external_directory": { "/other/**": "allow" } }
    }
  },
  "topLevelKey": "preserved"
}`
	t.Setenv(EnvConfigContent, stale)
	base := t.TempDir()
	wt, allowKey := canonicalAllowKey(t, base, "run-a")

	merged, err := BuildInlineConfig(stale, wt)
	if err != nil {
		t.Fatalf("BuildInlineConfig: %v", err)
	}

	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(merged), &root); err != nil {
		t.Fatalf("merged config invalid: %v", err)
	}

	// Top-level unrelated field preserved.
	if _, ok := root["topLevelKey"]; !ok {
		t.Errorf("top-level 'topLevelKey' was dropped during merge")
	}

	// Other agent untouched.
	var agents map[string]json.RawMessage
	if err := json.Unmarshal(root["agent"], &agents); err != nil {
		t.Fatalf("agent block invalid: %v", err)
	}
	var other struct {
		Description string `json:"description"`
		Permission  struct {
			ExternalDirectory map[string]string `json:"external_directory"`
		} `json:"permission"`
	}
	if err := json.Unmarshal(agents["some-other-agent"], &other); err != nil {
		t.Fatalf("some-other-agent invalid: %v", err)
	}
	if other.Description != "untouched" {
		t.Errorf("some-other-agent description changed")
	}
	mustContain(t, other.Permission.ExternalDirectory, "/other/**", "allow")

	// --- Worker fields: canonical wins ----------------------------------------
	var worker struct {
		Description string                     `json:"description"`
		Mode        string                     `json:"mode"`
		Steps       int                        `json:"steps"`
		Permission  map[string]json.RawMessage `json:"permission"`
	}
	if err := json.Unmarshal(agents[AgentName], &worker); err != nil {
		t.Fatalf("worker block invalid: %v", err)
	}

	if worker.Mode != "primary" {
		t.Errorf("mode = %q, want primary (canonical override)", worker.Mode)
	}
	if worker.Steps != 50 {
		t.Errorf("steps = %d, want 50 (canonical override of stale 35)", worker.Steps)
	}

	perm := worker.Permission

	if perm["websearch"] == nil {
		t.Errorf("canonical websearch deny missing from merged permission")
	} else {
		var ws string
		if err := json.Unmarshal(perm["websearch"], &ws); err != nil {
			t.Fatalf("websearch value invalid: %v", err)
		}
		if ws != "deny" {
			t.Errorf("websearch = %q, want deny (canonical override of stale allow)", ws)
		}
	}

	if perm["edit"] == nil {
		t.Errorf("canonical edit allow missing from merged permission")
	} else {
		var ed string
		if err := json.Unmarshal(perm["edit"], &ed); err != nil {
			t.Fatalf("edit value invalid: %v", ed)
		}
		if ed != "allow" {
			t.Errorf("edit = %q, want allow", ed)
		}
	}

	// External invocation-specific model is preserved because canonical does not own it.
	if _, ok := root["model"]; !ok {
		t.Errorf("invocation-specific model was dropped during merge")
	}

	// External_directory dynamic rule is still applied.
	ext := workerExtDir(t, merged)
	mustContain(t, ext, "*", "deny")
	mustContain(t, ext, allowKey, "allow")

	// Canonical description is present.
	if worker.Description == "" {
		t.Errorf("canonical description missing from merged worker")
	}
}

// --- proof: empty incoming config produces full canonical worker definition --

func TestEmptyIncomingConfigProducesFullCanonicalWorker(t *testing.T) {
	t.Setenv(EnvConfigContent, "")
	base := t.TempDir()
	wt, allowKey := canonicalAllowKey(t, base, "run-a")

	merged, err := BuildInlineConfig("", wt)
	if err != nil {
		t.Fatalf("BuildInlineConfig: %v", err)
	}

	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(merged), &root); err != nil {
		t.Fatalf("merged config invalid: %v", err)
	}

	var agents map[string]json.RawMessage
	if err := json.Unmarshal(root["agent"], &agents); err != nil {
		t.Fatalf("agent block invalid: %v", err)
	}

	var worker struct {
		Description string                     `json:"description"`
		Mode        string                     `json:"mode"`
		Steps       int                        `json:"steps"`
		Permission  map[string]json.RawMessage `json:"permission"`
	}
	if err := json.Unmarshal(agents[AgentName], &worker); err != nil {
		t.Fatalf("worker block invalid: %v", err)
	}

	if worker.Mode != "primary" {
		t.Errorf("mode = %q, want primary", worker.Mode)
	}
	if worker.Steps != 50 {
		t.Errorf("steps = %d, want 50", worker.Steps)
	}
	if worker.Description == "" {
		t.Errorf("canonical description missing from empty-input config")
	}

	perm := worker.Permission
	for _, key := range []string{"edit", "webfetch", "websearch", "question", "task"} {
		if perm[key] == nil {
			t.Errorf("canonical permission key %q missing from empty-input config", key)
		}
	}

	ext := workerExtDir(t, merged)
	mustContain(t, ext, "*", "deny")
	mustContain(t, ext, allowKey, "allow")
}

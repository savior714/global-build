package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIRuntimePersistsAndInspectsAutonomousFrontier(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "runtime.json")
	event := map[string]any{
		"type": "add_task",
		"task": map[string]any{
			"id":             "A",
			"scope":          "bounded runtime CLI proof",
			"bounded":        true,
			"positive_value": true,
			"value_reason":   "the state transition is directly useful",
			"brief_status":   "PUBLISHED",
			"watch_surfaces": []string{"src/"},
		},
	}
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	code, out, stderr := runBin(t, []string{"runtime", "--state", statePath, "apply"}, string(body), nil)
	if code != 0 {
		t.Fatalf("runtime apply exit=%d\nstdout=%s\nstderr=%s", code, out, stderr)
	}
	if !strings.Contains(out, `"version": "0.2.3"`) || !strings.Contains(out, `"state": "ACTIVE"`) {
		t.Fatalf("runtime did not persist/promote ACTIVE task:\n%s", out)
	}

	code, out, stderr = runBin(t, []string{"runtime", "--state", statePath, "snapshot"}, "", nil)
	if code != 0 || stderr != "" || !strings.Contains(out, `"state": "ACTIVE"`) {
		t.Fatalf("runtime snapshot code=%d stderr=%s stdout=%s", code, stderr, out)
	}
	code, out, stderr = runBin(t, []string{"runtime", "--state", statePath, "stop-check"}, "", nil)
	if code != 0 || stderr != "" || !strings.Contains(out, `"stop": false`) {
		t.Fatalf("runtime stop check code=%d stderr=%s stdout=%s", code, stderr, out)
	}
}

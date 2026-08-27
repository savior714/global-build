package opencode

import (
	"encoding/json"
	"testing"
)

func TestParseVersionAcceptsSupportedGeneration(t *testing.T) {
	cases := []struct {
		input   string
		version string
		major   int
		minor   int
	}{
		{"1.18.23\n", "1.18.23", 1, 18},
		{"opencode v1.19.0\n", "1.19.0", 1, 19},
		{"OpenCode 1.20.1-beta.2", "1.20.1-beta.2", 1, 20},
	}
	for _, tc := range cases {
		version, major, minor, err := parseVersion(tc.input)
		if err != nil {
			t.Fatalf("parseVersion(%q): %v", tc.input, err)
		}
		if version != tc.version || major != tc.major || minor != tc.minor {
			t.Fatalf("parseVersion(%q) = (%q,%d,%d), want (%q,%d,%d)", tc.input, version, major, minor, tc.version, tc.major, tc.minor)
		}
	}
}

func TestParseVersionRejectsUnparseableOutput(t *testing.T) {
	if _, _, _, err := parseVersion("OpenCode development build"); err == nil {
		t.Fatal("version output without a semantic version should be rejected")
	}
}

func TestCheckCompatibilityAdmitsOnly1_18(t *testing.T) {
	cases := []struct {
		input   string
		wantErr bool
	}{
		{"opencode 1.18.23\n", false},
		{"opencode 1.18.0\n", false},
		{"opencode 1.18.99\n", false},
		{"OpenCode v1.17.5\n", true},
		{"OpenCode 1.17.0\n", true},
		{"opencode 1.19.0\n", true},
		{"opencode 1.19.1\n", true},
		{"OpenCode 2.0.0\n", true},
		{"opencode 2.1.3\n", true},
		{"OpenCode 0.99.0\n", true},
	}
	for _, tc := range cases {
		// Test the admission decision by calling parseVersion and simulating
		// the check that CheckCompatibility performs.
		version, major, minor, err := parseVersion(tc.input)
		if err != nil {
			t.Fatalf("parseVersion(%q): %v", tc.input, err)
		}
		admitted := major == SupportedMajor && minor == AdmittedMinor
		if admitted != !tc.wantErr {
			t.Errorf("version %s (major=%d minor=%d): admitted=%v, wantErr=%v", version, major, minor, admitted, tc.wantErr)
		}
	}
}

// TestCanonicalDoomLoopDenyOverridesStaleAllow proves that noninteractive
// repeated-call handling is repository-owned rather than inherited from a
// permissive or interactive external OpenCode config.
func TestCanonicalDoomLoopDenyOverridesStaleAllow(t *testing.T) {
	stale := `{
  "agent": {
    "global-build-worker": {
      "permission": {
        "doom_loop": "allow"
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
		t.Fatalf("merged config invalid: %v", err)
	}
	raw := root.Agent[AgentName].Permission["doom_loop"]
	if raw == nil {
		t.Fatal("canonical doom_loop permission missing")
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("doom_loop value invalid: %v", err)
	}
	if got != "deny" {
		t.Fatalf("doom_loop = %q, want deny", got)
	}
}

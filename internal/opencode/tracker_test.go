package opencode

import "testing"

// TestTrackerTerminalTextUsesLatestDistinctTextPart keeps multi-text stream
// selection as an explicit opencode concern. CLI/repository fixtures can then
// remain focused on their own admission/candidate contracts.
func TestTrackerTerminalTextUsesLatestDistinctTextPart(t *testing.T) {
	trk := NewTracker()
	for _, line := range []string{
		`{"type":"text","timestamp":2,"sessionID":"ses_1","part":{"id":"p2","type":"text","text":"Working on the prepared task."}}`,
		`{"type":"tool_use","timestamp":3,"sessionID":"ses_1","part":{"id":"p3","type":"tool","tool":"bash","state":{"status":"completed"}}}`,
		`{"type":"text","timestamp":4,"sessionID":"ses_1","part":{"id":"p4","type":"text","text":"RESULT: COMPLETE\nPRIMARY_PROOF: PASS"}}`,
	} {
		if err := trk.Observe(line); err != nil {
			t.Fatalf("Observe: %v", err)
		}
	}

	got, ok := trk.TerminalText()
	if !ok {
		t.Fatal("TerminalText returned no text")
	}
	want := "RESULT: COMPLETE\nPRIMARY_PROOF: PASS"
	if got != want {
		t.Fatalf("TerminalText = %q, want %q", got, want)
	}
}

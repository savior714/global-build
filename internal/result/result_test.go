package result

import "testing"

// TestParseCompleteBareAccepts verifies the strict parser accepts the minimal
// bare protocol line with no surrounding noise.
func TestParseCompleteBareAccepts(t *testing.T) {
	in := "RESULT: COMPLETE\nPRIMARY_PROOF: PASS"
	r, err := Parse(in)
	if err != nil {
		t.Fatalf("bare COMPLETE should be accepted: %v", err)
	}
	if r.Kind != Complete {
		t.Errorf("kind = %q, want COMPLETE", r.Kind)
	}
	if got := r.Fields["PRIMARY_PROOF"]; got != "PASS" {
		t.Errorf("PRIMARY_PROOF = %q, want PASS", got)
	}
}

// TestParseCompleteMarkdownFenceRejected verifies that wrapping the protocol in
// Markdown code fences is rejected — a real failure mode that previously caused
// workers to end as MALFORMED_RESULT.
func TestParseCompleteMarkdownFenceRejected(t *testing.T) {
	in := "```\nRESULT: COMPLETE\nPRIMARY_PROOF: PASS\n```"
	_, err := Parse(in)
	if err == nil {
		t.Fatal("Markdown-fenced COMPLETE should be rejected")
	}
}

// TestParseProseBeforeResultRejected verifies that any prose preceding the
// RESULT declaration causes a strict rejection.
func TestParseProseBeforeResultRejected(t *testing.T) {
	in := "Some prose before the protocol.\nRESULT: COMPLETE\nPRIMARY_PROOF: PASS"
	_, err := Parse(in)
	if err == nil {
		t.Fatal("prose before RESULT should be rejected")
	}
}

// TestParseProseAfterCompleteRejected verifies that any prose following an
// otherwise valid COMPLETE declaration causes a strict rejection.
func TestParseProseAfterCompleteRejected(t *testing.T) {
	in := "RESULT: COMPLETE\nPRIMARY_PROOF: PASS\nSome prose after the protocol."
	_, err := Parse(in)
	if err == nil {
		t.Fatal("prose after COMPLETE should be rejected")
	}
}

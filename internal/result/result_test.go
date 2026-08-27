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

// TestParseTrailingBareProseRejectedForEveryOutcome guards the asymmetry that
// previously let CONTINUABLE/BLOCKED absorb trailing prose into their last
// multi-line field while COMPLETE happened to reject it via PRIMARY_PROOF.
func TestParseTrailingBareProseRejectedForEveryOutcome(t *testing.T) {
	cases := map[string]string{
		"complete": "RESULT: COMPLETE\nPRIMARY_PROOF: PASS\nSome prose after the protocol.",
		"continuable": "RESULT: CONTINUABLE\nADMITTED_BASE: 0123456789012345678901234567890123456789\nCOMPLETED: scaffolding\nREMAINING: implementation\nNEXT_ACTION: continue\nDO_NOT_REOPEN: settled facts\nVERIFICATION_ALREADY_DONE: go test\nWORKTREE_STATE: partial edit\nSome prose after the protocol.",
		"blocked": "RESULT: BLOCKED\nBLOCKER: dependency unavailable\nEVIDENCE: direct evidence\nSome prose after the protocol.",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(in); err == nil {
				t.Fatalf("%s with trailing bare prose should be rejected", name)
			}
		})
	}
}

// TestParseExplicitIndentedContinuationAccepted keeps intentional multi-line
// protocol values available while making the continuation syntax unambiguous.
func TestParseExplicitIndentedContinuationAccepted(t *testing.T) {
	in := "RESULT: BLOCKED\nBLOCKER: dependency unavailable\nEVIDENCE: first line\n  second line"
	r, err := Parse(in)
	if err != nil {
		t.Fatalf("explicit continuation should be accepted: %v", err)
	}
	if got := r.Fields["EVIDENCE"]; got != "first line\nsecond line" {
		t.Fatalf("EVIDENCE = %q, want explicit two-line value", got)
	}
}

func TestParseBlankInteriorLineRejected(t *testing.T) {
	in := "RESULT: BLOCKED\nBLOCKER: dependency unavailable\n\nEVIDENCE: direct evidence"
	if _, err := Parse(in); err == nil {
		t.Fatal("blank interior line should be rejected")
	}
}

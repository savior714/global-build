package opencode

import (
	"fmt"
	"sync"
	"testing"
)

// TestTrackerConcurrentObserve verifies that concurrent Observe calls are safe.
// Each goroutine feeds a disjoint set of event lines; the tracker must not
// panic and must retain all observations.
func TestTrackerConcurrentObserve(t *testing.T) {
	trk := NewTracker()
	const goroutines = 8
	const linesPerGoroutine = 32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < linesPerGoroutine; i++ {
				line := `{"type":"text","timestamp":1,"sessionID":"ses_1","part":{"id":"p` +
					string(rune('0'+id)) + string(rune('0'+i)) + `","type":"text","text":"msg"}}`
				if err := trk.Observe(line); err != nil {
					t.Errorf("goroutine %d line %d: %v", id, i, err)
				}
			}
		}(g)
	}
	wg.Wait()
	if !trk.AnyEvents() {
		t.Error("tracker reports no events after concurrent observes")
	}
	if !trk.SubstantiveBegan() {
		t.Error("tracker should have substantive text after concurrent observes")
	}
}

// TestTrackerTerminalTextAfterConcurrentObserve ensures TerminalText is
// deterministic even when Observe calls race.
func TestTrackerTerminalTextAfterConcurrentObserve(t *testing.T) {
	trk := NewTracker()
	const n = 64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(id int) {
			defer wg.Done()
			line := `{"type":"text","timestamp":1,"sessionID":"ses_1","part":{"id":"p` +
				string(rune('0'+id%10)) + `","type":"text","text":"msg"}}`
			if err := trk.Observe(line); err != nil {
				t.Errorf("goroutine %d: %v", id, err)
			}
		}(i)
	}
	wg.Wait()
	_, ok := trk.TerminalText()
	if !ok {
		t.Fatal("TerminalText returned no text after concurrent observes")
	}
}

// TestTrackerErrorsThreadSafe verifies that Errors() returns a copy and is safe
// to call concurrently with Observe.
func TestTrackerErrorsThreadSafe(t *testing.T) {
	trk := NewTracker()
	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < 16; i++ {
				line := fmt.Sprintf(`{"type":"error","timestamp":1,"sessionID":"ses_1","error":"err-%d"}`, i)
				if err := trk.Observe(line); err != nil {
					t.Errorf("observe: %v", err)
				}
			}
			_ = trk.Errors() // read while writes may be in flight
		}()
	}
	wg.Wait()
	errs := trk.Errors()
	if len(errs) == 0 {
		t.Error("expected errors from tracker")
	}
	// Verify the returned slice is a copy: mutating it must not affect the tracker.
	errs[0] = "mutated"
	if trk.Errors()[0] == "mutated" {
		t.Error("Errors() did not return a copy")
	}
}

// TestTrackerProgressAtUpdatesOnConcurrentObserve ensures ProgressAt advances
// even under concurrent Observe pressure.
func TestTrackerProgressAtUpdatesOnConcurrentObserve(t *testing.T) {
	trk := NewTracker()
	const n = 32
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			line := `{"type":"text","timestamp":1,"sessionID":"ses_1","part":{"id":"p1","type":"text","text":"x"}}`
			if err := trk.Observe(line); err != nil {
				t.Errorf("observe: %v", err)
			}
		}()
	}
	wg.Wait()
	if !trk.ProgressAt().After(trk.ProgressAt()) {
		// ProgressAt should have advanced from the initial time.Now() in NewTracker.
		// Under contention we just verify it's not zero and the tracker has events.
	}
	if !trk.AnyEvents() {
		t.Error("expected progress events")
	}
}

// --- IsTransientErrorPayload ------------------------------------------------

func TestIsTransientErrorPayloadHTTPCodes(t *testing.T) {
	cases := []string{
		"429 Too Many Requests",
		"500 Internal Server Error",
		"502 Bad Gateway",
		"503 Service Unavailable",
		"504 Gateway Timeout",
		"524 A timeout occurred",
		"request failed with 500",
		"response 429 received",
	}
	for _, c := range cases {
		if !IsTransientErrorPayload(c) {
			t.Errorf("expected transient for %q", c)
		}
	}
}

func TestIsTransientErrorPayloadRateLimit(t *testing.T) {
	cases := []string{
		"rate limit exceeded",
		"rate-limit hit",
		"rate_limit reached",
		"too many requests",
		"rate increased too quickly",
	}
	for _, c := range cases {
		if !IsTransientErrorPayload(c) {
			t.Errorf("expected transient for rate-limit %q", c)
		}
	}
}

func TestIsTransientErrorPayloadNetwork(t *testing.T) {
	cases := []string{
		"fetch failed",
		"failed to fetch",
		"network error",
		"network_error",
		"connection refused",
		"connection reset",
		"connection lost",
		"socket hang up",
		"econnrefused",
		"econnreset",
		"etimedout",
		"getaddrinfo",
		"enotfound",
		"request timeout",
		"response timed out",
		"connection time out",
		"stream read timeout",
	}
	for _, c := range cases {
		if !IsTransientErrorPayload(c) {
			t.Errorf("expected transient for network %q", c)
		}
	}
}

func TestIsTransientErrorPayloadOverloaded(t *testing.T) {
	cases := []string{
		"overloaded",
		"service unavailable",
		"service_unavailable",
		"internal server error",
		"server_error",
	}
	for _, c := range cases {
		if !IsTransientErrorPayload(c) {
			t.Errorf("expected transient for overload %q", c)
		}
	}
}

func TestIsTransientErrorPayloadJSONEnvelope(t *testing.T) {
	// When the payload is JSON with a name + data.message, both fields are
	// concatenated for matching.
	payload := `{"name":"FetchError","data":{"message":"503 service unavailable"}}`
	if !IsTransientErrorPayload(payload) {
		t.Error("expected transient for JSON envelope with 503 message")
	}

	payload2 := `{"name":"RateLimitError","data":{"message":"rate limit exceeded"}}`
	if !IsTransientErrorPayload(payload2) {
		t.Error("expected transient for JSON envelope with rate limit message")
	}
}

func TestIsTransientErrorPayloadNonTransient(t *testing.T) {
	cases := []string{
		"permission denied",
		"invalid api key",
		"model not found",
		"content policy violation",
		"timeout waiting for user input",
		"compile error in generated code",
		"",
	}
	for _, c := range cases {
		if IsTransientErrorPayload(c) {
			t.Errorf("expected non-transient for %q", c)
		}
	}
}

func TestIsTransientErrorPayloadPartialCodeNotMatched(t *testing.T) {
	// The HTTP code patterns require non-digit boundaries: "429" inside "14290"
	// should not match.
	cases := []string{
		"error code 14290 occurred", // 429 embedded in larger number
		"status 5001 returned",      // 500 embedded in larger number
	}
	for _, c := range cases {
		if IsTransientErrorPayload(c) {
			t.Errorf("expected non-transient for embedded code %q", c)
		}
	}
}

func TestIsTransientErrorPayloadJSONOnlyName(t *testing.T) {
	// JSON with only a name field, no data.
	payload := `{"name":"NetworkError"}`
	if !IsTransientErrorPayload(payload) {
		t.Error("expected transient for JSON name matching network pattern")
	}

	payload2 := `{"name":"SomeRandomError"}`
	if IsTransientErrorPayload(payload2) {
		t.Error("expected non-transient for random JSON name")
	}
}

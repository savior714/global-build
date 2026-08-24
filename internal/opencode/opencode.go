// Package opencode executes the installed OpenCode CLI non-interactively and
// interprets its `run --format json` NDJSON event stream.
//
// Event contract (verified against the current installed CLI generation):
// each stdout line is {"type":..., "timestamp":..., "sessionID":..., ...data}
// with type one of step_start, step_finish, text, reasoning, tool_use, error.
// Substantive progress signals are: assistant text parts with time.end set,
// tool_use events (emitted on tool completion), and model step advancement.
// Spinner/reconnect/retry noise never appears in this stream.
package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// AgentName is the dedicated global agent id used for every BUILD attempt.
const AgentName = "global-build-worker"

// Part mirrors the subset of message-part fields the runner relies on.
type Part struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Text  string `json:"text"`
	Tool  string `json:"tool"`
	State *struct {
		Status string `json:"status"`
	} `json:"state"`
}

// Event is one NDJSON line of the structured stream.
type Event struct {
	Type      string          `json:"type"`
	Timestamp int64           `json:"timestamp"`
	SessionID string          `json:"sessionID"`
	Part      json.RawMessage `json:"part"`
	Error     json.RawMessage `json:"error"`
}

// Tracker classifies stream events into progress evidence and accumulates the
// terminal-response candidate. Safe for concurrent use: one goroutine feeds
// Observe while the runner watchdog reads ProgressAt/AnyEvents.
type Tracker struct {
	mu          sync.Mutex
	textOrder   []string        // part ids in first-seen order
	texts       map[string]Part // deduped by part id, last write wins
	errors      []string        // serialized error payloads in order
	lastProg    time.Time       // last meaningful-progress observation
	eventsSeen  int             // structured events parsed (any type)
	sawStepStart  bool
	sawStepFinish bool
	sawToolUse    bool
	substantive   bool // non-empty assistant text or any tool_use seen
}

func NewTracker() *Tracker {
	return &Tracker{texts: map[string]Part{}, lastProg: time.Now()}
}

// Observe consumes one raw stdout line. Blank lines are ignored. A line that
// is not valid JSON returns an error; callers treat that as a corrupted
// stream (fail closed).
func (t *Tracker) Observe(line string) error {
	if strings.TrimSpace(line) == "" {
		return nil
	}
	var ev Event
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return fmt.Errorf("invalid JSON event line: %w", err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.eventsSeen++
	if Progress(ev.Type) {
		t.lastProg = time.Now()
	}
	switch ev.Type {
	case "text":
		var p Part
		if err := json.Unmarshal(ev.Part, &p); err != nil {
			return fmt.Errorf("invalid text part: %w", err)
		}
		if _, seen := t.texts[p.ID]; !seen {
			t.textOrder = append(t.textOrder, p.ID)
		}
		t.texts[p.ID] = p
		if strings.TrimSpace(p.Text) != "" {
			t.substantive = true
		}
	case "tool_use":
		t.sawToolUse = true
		t.substantive = true // a tool call/result is substantive execution + potential mutation
	case "step_start":
		t.sawStepStart = true
	case "step_finish":
		t.sawStepFinish = true
	case "reasoning":
		// thinking text is not counted as substantive assistant output
	case "error":
		t.errors = append(t.errors, string(ev.Error))
	default:
		// Unknown structured types are noise by definition.
	}
	return nil
}

// Progress reports whether this event type counts as meaningful progress.
func Progress(evType string) bool {
	switch evType {
	case "text", "tool_use", "step_start", "step_finish":
		return true
	}
	return false
}

// TerminalText returns the last completed assistant text part deterministically.
func (t *Tracker) TerminalText() (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.textOrder) - 1; i >= 0; i-- {
		p := t.texts[t.textOrder[i]]
		if strings.TrimSpace(p.Text) != "" {
			return p.Text, true
		}
	}
	return "", false
}

// SubstantiveBegan reports whether substantive model execution has begun:
// non-empty assistant text was produced OR any tool call completed.
func (t *Tracker) SubstantiveBegan() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.substantive
}

// ToolEventOccurred reports whether any tool/mutation event has occurred.
func (t *Tracker) ToolEventOccurred() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sawToolUse
}

// Errors returns the serialized error payloads observed on the stream.
func (t *Tracker) Errors() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.errors))
	copy(out, t.errors)
	return out
}

// ProgressAt reports the time of the last meaningful-progress event
// (substantive text, tool call/result, step advancement, final result).
func (t *Tracker) ProgressAt() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastProg
}

// AnyEvents reports whether any structured event was observed at all.
func (t *Tracker) AnyEvents() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.eventsSeen > 0
}

// transientPatterns is a deliberately narrow, deterministic subset of the
// retryable-transport markers used by current upstream OpenCode itself
// (session/retry.ts RETRYABLE_MESSAGE_PATTERNS): HTTP 429/5xx transport codes,
// rate limiting / overload phrasing, and network/transport failure phrases.
var transientPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(^|[^0-9])(429|500|502|503|504|524)([^0-9]|$)`),
	regexp.MustCompile(`(?i)rate increased too quickly|rate limit|rate-limit|rate_limit|too many requests`),
	regexp.MustCompile(`(?i)overloaded|service unavailable|service_unavailable|internal server error|server_error`),
	regexp.MustCompile(`(?i)fetch failed|failed to fetch|network[-_ ]?error|upstream connect|connection refused|connection reset|connection lost|socket hang up|econnrefused|econnreset|etimedout|getaddrinfo|enotfound`),
	regexp.MustCompile(`(?i)\b(request|response|connection|network|stream|read) (timeout|timed out|time out)\b`),
}

// IsTransientErrorPayload reports whether an error event payload clearly
// indicates a transport/transient failure.
func IsTransientErrorPayload(payload string) bool {
	if payload == "" {
		return false
	}
	var probe struct {
		Name string `json:"name"`
		Data *struct {
			Message string `json:"message"`
		} `json:"data"`
	}
	hay := payload
	if err := json.Unmarshal([]byte(payload), &probe); err == nil {
		hay = probe.Name
		if probe.Data != nil {
			hay += "\n" + probe.Data.Message
		}
	}
	if strings.TrimSpace(hay) == "" {
		return false
	}
	for _, re := range transientPatterns {
		if re.MatchString(hay) {
			return true
		}
	}
	return false
}

// Attempt is the outcome of one child invocation.
type Attempt struct {
	Tracker       *Tracker
	StreamCorrupt bool  // at least one non-blank stdout line failed JSON parsing
	SpawnErr      error // process could not be started at all
	ExitErr       error // non-zero exit of a started process
	Cancelled     bool  // context cancelled while running (timeout/interrupt)
}

// Executable resolves the OpenCode CLI binary path: explicit env override
// first (absolute path or name looked up in PATH), then plain PATH lookup.
func Executable(envVar string) (string, error) {
	if envVar != "" {
		if _, err := os.Stat(envVar); err == nil {
			return envVar, nil
		} else if lp, lookErr := exec.LookPath(envVar); lookErr == nil {
			return lp, nil
		} else {
			return "", fmt.Errorf("opencode binary %q (from %s env var) not found", envVar, EnvBinVar)
		}
	}
	lp, err := exec.LookPath(DefaultBinName)
	if err != nil {
		return "", fmt.Errorf("opencode binary %q not found in PATH: %w", DefaultBinName, err)
	}
	return lp, nil
}

// Names used for configuration and lookup.
const (
	DefaultBinName = "opencode"
	EnvBinVar      = "GLOBAL_BUILD_OPENCODE_BIN"
)

// Invoke runs one non-interactive attempt:
//
//	<bin> run --format json --agent global-build-worker --dir <worktreeDir>
//
// with taskBody passed through child stdin. Stdout is consumed as NDJSON;
// child stderr is forwarded to stderrOut for operational diagnostics.
func Invoke(ctx context.Context, bin, worktreeDir, taskBody string, stderrOut io.Writer) (*Attempt, error) {
	cmd := exec.CommandContext(ctx, bin,
		"run",
		"--format", "json",
		"--agent", AgentName,
		"--dir", worktreeDir,
	)
	cmd.Stdin = strings.NewReader(taskBody)
	cmd.WaitDelay = 3 * time.Second // ensure pipes close after forced kill

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return &Attempt{SpawnErr: err}, nil
	}

	tracker := NewTracker()
	corrupt := false

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 0, 64*1024)
		scanner := newLargeScanner(stdout, &buf)
		for scanner.Scan() {
			line := scanner.Text()
			if err := tracker.Observe(line); err != nil {
				corrupt = true // keep draining; fail closed later
				fmt.Fprintf(stderrOut, "[global-build] malformed stdout line from opencode: %v\n", err)
			}
		}
	}()

	errCopyDone := make(chan struct{})
	go func() {
		defer close(errCopyDone)
		_, _ = io.Copy(stderrOut, stderr)
	}()

	startErr := cmd.Wait()
	<-readDone
	<-errCopyDone

	attempt := &Attempt{Tracker: tracker, StreamCorrupt: corrupt}
	if ctx.Err() != nil && startErr != nil {
		attempt.Cancelled = true
	}
	if startErr != nil {
		if ee, ok := startErr.(*exec.ExitError); ok {
			attempt.ExitErr = ee
		} else {
			attempt.SpawnErr = startErr
		}
	}
	return attempt, nil
}

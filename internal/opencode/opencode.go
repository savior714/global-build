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
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"global-build/internal/gitx"
)

//go:embed global-build-worker.md
var canonicalWorkerSource []byte

//go:embed global-build-explore.md
var canonicalExploreSource []byte

// EmbeddedWorkerSource returns the raw bytes of the repo-owned canonical worker
// definition. Exported only so cross-package tests can inspect it without
// reaching for the installed home-directory copy.
func EmbeddedWorkerSource() ([]byte, error) {
	return canonicalWorkerSource, nil
}

// EmbeddedExploreSource returns the raw bytes of the repo-owned canonical
// read-only discovery subagent definition. Exported only so cross-package tests
// can inspect it without reaching for the installed home-directory copy.
func EmbeddedExploreSource() ([]byte, error) {
	return canonicalExploreSource, nil
}

// EnvConfigContent is OpenCode's runtime inline configuration mechanism: when
// set in the environment of a spawned OpenCode process, its JSON value is merged
// over the on-disk configuration for that invocation only. It is NOT written to
// disk and does NOT alter the global agent definition.
const EnvConfigContent = "OPENCODE_CONFIG_CONTENT"

// AgentName is the dedicated global agent id used for every BUILD attempt.
const AgentName = "global-build-worker"

// ExploreAgentName is the built-in read-only discovery subagent admitted by the
// canonical worker contract. Deprecated: use GlobalBuildExploreAgentName.
const ExploreAgentName = "explore"

// GlobalBuildExploreAgentName is the dedicated repo-owned read-only discovery
// subagent injected by global-build for every BUILD invocation. It replaces the
// mutable external built-in `explore` agent so stale/custom invocation config
// cannot alter the delegated investigator contract.
const GlobalBuildExploreAgentName = "global-build-explore"

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
	mu            sync.Mutex
	textOrder     []string        // part ids in first-seen order
	texts         map[string]Part // deduped by part id, last write wins
	errors        []string        // serialized error payloads in order
	lastProg      time.Time       // last meaningful-progress observation
	eventsSeen    int             // structured events parsed (any type)
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

// parseEmbeddedWorker splits the repo-owned canonical worker Markdown into its
// YAML frontmatter (as a JSON RawMessage map) and its prompt body.
//
// The body is the literal Markdown text after the closing frontmatter
// delimiter, trimmed. This mirrors OpenCode's legacy Markdown agent loader
// semantics: "the file body becomes the agent's prompt" (verified against the
// installed OpenCode 1.18.25 generation). We do NOT reconstruct the body from
// prose; we use the literal worker body that ships in the repo.
func parseEmbeddedWorker() (frontmatter map[string]json.RawMessage, body string, err error) {
	text := string(canonicalWorkerSource)
	parts := strings.SplitN(text, "---", 3)
	if len(parts) < 3 {
		return nil, "", fmt.Errorf("embedded worker source lacks frontmatter delimiters")
	}
	// Parse YAML into a generic map so key casing is preserved exactly as in
	// the source (lowercase), then marshal to JSON for uniform merging.
	var rawMap map[string]interface{}
	if err := yaml.Unmarshal([]byte(parts[1]), &rawMap); err != nil {
		return nil, "", fmt.Errorf("embedded worker frontmatter is not valid YAML: %w", err)
	}
	out, err := json.Marshal(rawMap)
	if err != nil {
		return nil, "", fmt.Errorf("cannot marshal canonical worker frontmatter to JSON: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, "", fmt.Errorf("cannot re-parse canonical worker frontmatter: %w", err)
	}
	// Body is the literal worker prose after the closing '---'. Trim only the
	// surrounding whitespace, matching the loader's "file body becomes prompt"
	// behavior (leading/trailing blank lines carry no instruction).
	body = strings.TrimSpace(parts[2])
	return raw, body, nil
}

// CanonicalWorkerBody returns the repo-owned canonical worker instruction body
// (the Markdown text after the frontmatter). It is the exact text that
// BuildInlineConfig injects into agent.global-build-worker.prompt so the runtime
// never falls back to a separately installed stale worker file.
func CanonicalWorkerBody() (string, error) {
	_, body, err := parseEmbeddedWorker()
	if err != nil {
		return "", err
	}
	return body, nil
}

// applyExploreReadOnlyOverride owns the read-only discovery subagent tool
// surface for this single global-build invocation. The built-in Explore agent
// otherwise allows bash, webfetch, and websearch, and child sessions use the
// subagent's own permissions rather than inheriting the parent's narrower intent.
// Preserve all non-permission Explore metadata, but replace its permission block
// with a deny-by-default allow-list containing only dedicated local read/search
// tools. Also injects the canonical global-build-explore agent definition so
// stale/custom invocation config cannot replace or disable it.
func applyExploreReadOnlyOverride(agents map[string]json.RawMessage) error {
	// Inject the canonical global-build-explore agent definition. Parse its
	// frontmatter to extract mode/steps/permission, then set the prompt from the
	// literal body. This ensures the agent is always present with canonical
	// semantics regardless of any stale external config.
	exploreFM, explorePrompt, err := parseEmbeddedExplore()
	if err != nil {
		return fmt.Errorf("cannot load canonical explore source: %w", err)
	}

	explore := map[string]json.RawMessage{}
	if raw, ok := agents[GlobalBuildExploreAgentName]; ok {
		if err := json.Unmarshal(raw, &explore); err != nil {
			return fmt.Errorf("existing %s agent block is not a valid object: %w", GlobalBuildExploreAgentName, err)
		}
	}

	// Canonical fields always win.
	for k, v := range exploreFM {
		explore[k] = v
	}
	promptJSON, err := json.Marshal(explorePrompt)
	if err != nil {
		return fmt.Errorf("cannot encode canonical explore prompt: %w", err)
	}
	explore["prompt"] = promptJSON

	exploreRaw, err := json.Marshal(explore)
	if err != nil {
		return fmt.Errorf("cannot encode %s override: %w", GlobalBuildExploreAgentName, err)
	}
	agents[GlobalBuildExploreAgentName] = exploreRaw

	// Also override the legacy built-in `explore` agent if present, so stale
	// external config cannot weaken it either. Preserve non-permission metadata.
	if raw, ok := agents[ExploreAgentName]; ok {
		legacyExplore := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &legacyExplore); err != nil {
			return fmt.Errorf("existing %s agent block is not a valid object: %w", ExploreAgentName, err)
		}
		permissionRaw, err := json.Marshal(map[string]string{
			"*":    "deny",
			"glob": "allow",
			"grep": "allow",
			"list": "allow",
			"read": "allow",
		})
		if err != nil {
			return fmt.Errorf("cannot encode %s permission override: %w", ExploreAgentName, err)
		}
		legacyExplore["permission"] = permissionRaw
		legacyRaw, err := json.Marshal(legacyExplore)
		if err != nil {
			return fmt.Errorf("cannot encode %s override: %w", ExploreAgentName, err)
		}
		agents[ExploreAgentName] = legacyRaw
	}
	return nil
}

// parseEmbeddedExplore splits the repo-owned canonical explore Markdown into its
// YAML frontmatter (as a generic map) and its prompt body. Mirrors the same
// semantics as parseEmbeddedWorker but operates on the explore agent file.
func parseEmbeddedExplore() (map[string]json.RawMessage, string, error) {
	text := string(canonicalExploreSource)
	parts := strings.SplitN(text, "---", 3)
	if len(parts) < 3 {
		return nil, "", fmt.Errorf("embedded explore source lacks frontmatter delimiters")
	}
	var rawMap map[string]interface{}
	if err := yaml.Unmarshal([]byte(parts[1]), &rawMap); err != nil {
		return nil, "", fmt.Errorf("canonical explore frontmatter is not valid YAML: %w", err)
	}
	out, err := json.Marshal(rawMap)
	if err != nil {
		return nil, "", fmt.Errorf("cannot marshal canonical explore frontmatter to JSON: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, "", fmt.Errorf("cannot re-parse canonical explore frontmatter: %w", err)
	}
	return raw, strings.TrimSpace(parts[2]), nil
}

// BuildInlineConfig returns the merged OPENCODE_CONFIG_CONTENT JSON string for
// a single run. It grants agent global-build-worker external_directory access
// ONLY to the exact canonical worktree, while denying everything else, WITHOUT
// granting global external_directory: allow and WITHOUT weakening unrelated
// rules.
//
// The injected override is:
//
//	agent.global-build-worker.permission.external_directory = {
//	  "*":                     "deny",
//	  "<canonicalWorktree>/**": "allow",
//	}
//
// The canonical worker source (internal/opencode/global-build-worker.md) owns
// description, mode, the canonical permission entries, and the
// instruction body (prompt). Canonical-owned fields / sub-fields always
// override stale values for the SAME keys. Unrelated invocation-specific fields
// (e.g. a worker-scoped model/variant) are preserved because the canonical
// source does not own them. The primary worker is intentionally not given a
// steps budget: it terminates on its own rather than on a fixed step counter.
// NOTE: this is key-level override, not blanket
// permission isolation — OpenCode still merges configuration deeply, so only
// the canonical-owned keys are guaranteed to win.
//
// existingContent is the parent process's current OPENCODE_CONFIG_CONTENT (it
// may be empty). It is parsed as JSON; any malformed value fails closed so a
// broken user config is never silently discarded or replaced. All unrelated
// configuration (other agents, other permission keys, top-level fields) is
// preserved exactly. The canonical worktree identity is the same one the runner
// uses (gitx.CanonicalPath), so the allow rule matches exactly what the worker
// will observe.
func BuildInlineConfig(existingContent, worktreeDir string) (string, error) {
	wt := gitx.CanonicalPath(worktreeDir)

	root := map[string]json.RawMessage{}
	if s := strings.TrimSpace(existingContent); s != "" {
		if err := json.Unmarshal([]byte(s), &root); err != nil {
			return "", fmt.Errorf("existing OPENCODE_CONFIG_CONTENT is not valid JSON: %w", err)
		}
	}

	agents := map[string]json.RawMessage{}
	if raw, ok := root["agent"]; ok {
		if err := json.Unmarshal(raw, &agents); err != nil {
			return "", fmt.Errorf("existing agent block is not a valid object: %w", err)
		}
	}

	// Load the repo-owned canonical worker frontmatter AND body. Canonical
	// fields always win over stale external values; unrelated invocation-specific
	// fields (e.g. model/variant) are preserved because the canonical source does
	// not own them.
	canonicalFM, canonicalBody, err := parseEmbeddedWorker()
	if err != nil {
		return "", fmt.Errorf("cannot load canonical worker source: %w", err)
	}

	worker := map[string]json.RawMessage{}
	if raw, ok := agents[AgentName]; ok {
		if err := json.Unmarshal(raw, &worker); err != nil {
			return "", fmt.Errorf("existing %s agent block is not a valid object: %w", AgentName, err)
		}
	}

	// Merge canonical-owned fields into the worker block. Canonical source does
	// NOT own "model", so any invocation-specific model/variant is preserved.
	for k, v := range canonicalFM {
		if k == "permission" {
			// Merge permission sub-fields: canonical denies always win, but any
			// external sub-field not present in the canonical source is preserved.
			canonicalPerm := map[string]json.RawMessage{}
			if err := json.Unmarshal(v, &canonicalPerm); err != nil {
				return "", fmt.Errorf("canonical worker permission is not valid JSON: %w", err)
			}
			extPerm := map[string]json.RawMessage{}
			if raw, ok := worker["permission"]; ok {
				if err := json.Unmarshal(raw, &extPerm); err != nil {
					return "", fmt.Errorf("existing %s permission block is not a valid object: %w", AgentName, err)
				}
			}
			for pk, pv := range canonicalPerm {
				extPerm[pk] = pv
			}
			permRaw, err := json.Marshal(extPerm)
			if err != nil {
				return "", fmt.Errorf("cannot encode merged %s permission: %w", AgentName, err)
			}
			worker["permission"] = permRaw
		} else {
			// Top-level canonical-owned field overrides whatever was there.
			worker[k] = v
		}
	}

	// Model-directed termination: the primary worker must not run under a fixed
	// model-steering step budget from any source (canonical or stale invocation
	// config). A steps field would cap reasoning and force counter-based
	// termination, which is exactly what this runner removes from the hot path.
	// The worker terminates on its own when the prepared task is complete.
	delete(worker, "steps")

	// The canonical worker OWNS its instruction body. Inject it as the runtime
	// `prompt` so the spawned worker uses the repo-owned prose and NEVER falls
	// back to a separately installed stale worker file. This overrides any
	// stale prompt carried in existingContent (e.g. "STALE WORKER PROMPT").
	bodyJSON, err := json.Marshal(canonicalBody)
	if err != nil {
		return "", fmt.Errorf("cannot encode canonical worker body: %w", err)
	}
	worker["prompt"] = bodyJSON

	// Broad deny first, exact canonical-worktree allow last.
	extDir := map[string]string{
		"*":                 "deny",
		path.Join(wt, "**"): "allow",
	}
	extDirRaw, err := json.Marshal(extDir)
	if err != nil {
		return "", fmt.Errorf("cannot encode external_directory override: %w", err)
	}
	if permRaw, ok := worker["permission"]; ok {
		extPerm := map[string]json.RawMessage{}
		if err := json.Unmarshal(permRaw, &extPerm); err != nil {
			return "", fmt.Errorf("cannot re-parse merged %s permission: %w", AgentName, err)
		}
		extPerm["external_directory"] = extDirRaw
		permMarshaled, err := json.Marshal(extPerm)
		if err != nil {
			return "", fmt.Errorf("cannot encode external_directory override: %w", err)
		}
		worker["permission"] = permMarshaled
	} else {
		// Should not happen, but be defensive.
		permMarshaled, _ := json.Marshal(map[string]json.RawMessage{"external_directory": extDirRaw})
		worker["permission"] = permMarshaled
	}

	workerRaw, err := json.Marshal(worker)
	if err != nil {
		return "", fmt.Errorf("cannot encode %s override: %w", AgentName, err)
	}
	agents[AgentName] = workerRaw

	if err := applyExploreReadOnlyOverride(agents); err != nil {
		return "", err
	}

	agentsRaw, err := json.Marshal(agents)
	if err != nil {
		return "", fmt.Errorf("cannot encode agent override: %w", err)
	}
	root["agent"] = agentsRaw

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return "", fmt.Errorf("cannot encode merged config: %w", err)
	}
	return string(out), nil
}

// buildChildEnv returns the full environment for the child OpenCode process:
// the inherited parent environment with OPENCODE_CONFIG_CONTENT replaced by the
// run-specific merged config. The inheritance guarantees unrelated variables
// (PATH, GLOBAL_BUILD_OPENCODE_BIN, etc.) are carried through untouched.
func buildChildEnv(worktreeDir string) ([]string, error) {
	merged, err := BuildInlineConfig(os.Getenv(EnvConfigContent), worktreeDir)
	if err != nil {
		return nil, err
	}
	childEnv := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, EnvConfigContent+"=") {
			continue // drop any pre-existing value; we re-add the merged one
		}
		childEnv = append(childEnv, kv)
	}
	childEnv = append(childEnv, EnvConfigContent+"="+merged)
	return childEnv, nil
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

// syncedWriter wraps an io.Writer with a mutex so concurrent writers are safe.
// It is the single concurrency owner for diagnostic output during an OpenCode
// subprocess invocation: one goroutine feeds parse diagnostics while another
// drains child stderr. Callers must not write to the underlying writer directly.
type syncedWriter struct {
	mu  sync.Mutex
	w   io.Writer
}

func newSyncedWriter(w io.Writer) *syncedWriter {
	return &syncedWriter{w: w}
}

func (s *syncedWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// Invoke runs one non-interactive attempt:
//
//	<bin> run --format json --agent global-build-worker --dir <worktreeDir>
//
// with taskBody passed through child stdin. Stdout is consumed as NDJSON;
// child stderr is forwarded to stderrOut for operational diagnostics. If the
// primary reaches a terminal semantic outcome but renders malformed BUILD
// protocol text, Invoke may perform exactly one tool-less continuation of that
// exact session to re-render the already-reached outcome.
//
// Invoke creates its own Tracker internally; callers that need a live-progress
// watchdog to observe the exact same tracker while OpenCode is still running
// should use InvokeWithTracker instead.
func Invoke(ctx context.Context, bin, worktreeDir, taskBody string, stderrOut io.Writer) (*Attempt, error) {
	return InvokeWithTracker(ctx, bin, worktreeDir, taskBody, NewTracker(), stderrOut)
}

// InvokeWithTracker is the same as Invoke but accepts an externally-owned
// Tracker so the caller's watchdog can observe events in real-time while the
// child is still running. The tracker must be the same instance the runner's
// progress watchdog reads from; this is how the live-progress contract is
// enforced rather than observing only the post-invoke tracker snapshot.
func InvokeWithTracker(ctx context.Context, bin, worktreeDir, taskBody string, tracker *Tracker, stderrOut io.Writer) (*Attempt, error) {
	// Each BUILD invocation must grant the worker external_directory access to
	// ONLY its exact current disposable worktree. We inject that as a run-specific
	// runtime inline config (OPENCODE_CONFIG_CONTENT), merging over any config
	// already present in the parent environment. Malformed pre-existing config
	// fails closed: we never silently replace or discard it.
	childEnv, err := buildChildEnv(worktreeDir)
	if err != nil {
		return nil, fmt.Errorf("cannot build opencode child environment: %w", err)
	}

	cmd := exec.CommandContext(ctx, bin,
		"run",
		"--format", "json",
		"--agent", AgentName,
		"--dir", worktreeDir,
	)
	// Repository binding: the child's process working directory is pinned to
	// the exact owned disposable worktree, so it can never inherit the parent
	// shell's cwd (a potentially different repository) as its mutation root.
	// --dir remains the supported execution-directory option; cmd.Dir is the
	// deterministic backstop.
	cmd.Dir = worktreeDir
	cmd.Env = childEnv
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

	corrupt := false
	sessionID := ""
	sessionConflict := false

	synced := newSyncedWriter(stderrOut)

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 0, 64*1024)
		scanner := newLargeScanner(stdout, &buf)
		for scanner.Scan() {
			line := scanner.Text()
			if id, ok := sessionIDFromEventLine(line); ok {
				if sessionID == "" {
					sessionID = id
				} else if sessionID != id {
					sessionConflict = true
				}
			}
			if err := tracker.Observe(line); err != nil {
				corrupt = true // keep draining; fail closed later
				fmt.Fprintf(synced, "[global-build] malformed stdout line from opencode: %v\n", err)
			}
		}
	}()

	errCopyDone := make(chan struct{})
	go func() {
		defer close(errCopyDone)
		_, _ = io.Copy(synced, stderr)
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
	if sessionConflict {
		return attempt, nil
	}
	return maybeFinalizeMalformedTerminal(ctx, bin, worktreeDir, sessionID, attempt, synced)
}

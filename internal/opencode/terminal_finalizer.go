package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"global-build/internal/result"
)

// FinalizerAgentName is an invocation-local, tool-less agent used only to
// re-render a terminal outcome that the primary worker already reached but
// emitted in malformed BUILD protocol syntax.
const FinalizerAgentName = "global-build-finalizer"

const finalizerSystemPrompt = `You are the global-build terminal protocol finalizer.

You are continuing an existing session after the primary worker has already
finished its investigation, mutation, and PRIMARY PROOF. Your only job is to
re-render the terminal outcome already reached in that session into the exact
bare BUILD result protocol.

Hard rules:
- Do not investigate, call tools, mutate files, or make any new factual claim.
- Do not change the outcome or reinterpret the task.
- Use only facts and conclusions already present in the existing session.
- Output only the exact protocol lines. No Markdown, explanation, preface, or
  trailing prose. The first characters must be RESULT:.
- COMPLETE is valid only if the existing session already concluded PRIMARY
  PROOF passed; emit exactly RESULT: COMPLETE and PRIMARY_PROOF: PASS.
- CONTINUABLE and BLOCKED must preserve the already-established field values.
- If a valid protocol cannot be rendered without inventing or changing facts,
  output exactly UNREPAIRABLE. The runner will fail closed.`

const finalizerRequest = `Your immediately preceding terminal response was rejected only because it did not match the strict BUILD result syntax. Re-render the outcome you already reached using the protocol-only rules. Do not do any new work or change any fact.`

// sessionIDFromEventLine extracts the exact session identifier from one valid
// OpenCode NDJSON event. Invalid lines are left to Tracker.Observe so its
// existing stream-corruption behavior remains authoritative.
func sessionIDFromEventLine(line string) (string, bool) {
	var ev Event
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return "", false
	}
	id := strings.TrimSpace(ev.SessionID)
	return id, id != ""
}

// BuildFinalizerInlineConfig starts from the normal run-scoped configuration,
// then adds a dedicated finalizer whose entire tool surface is denied. The
// worker's invocation-specific model/variant are copied when present so the
// continuation does not silently switch model configuration.
func BuildFinalizerInlineConfig(existingContent, worktreeDir string) (string, error) {
	merged, err := BuildInlineConfig(existingContent, worktreeDir)
	if err != nil {
		return "", err
	}

	root := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(merged), &root); err != nil {
		return "", fmt.Errorf("cannot parse merged finalizer base config: %w", err)
	}
	agents := map[string]json.RawMessage{}
	if err := json.Unmarshal(root["agent"], &agents); err != nil {
		return "", fmt.Errorf("cannot parse merged agent block for finalizer: %w", err)
	}

	worker := map[string]json.RawMessage{}
	if raw, ok := agents[AgentName]; ok {
		if err := json.Unmarshal(raw, &worker); err != nil {
			return "", fmt.Errorf("cannot parse %s while building finalizer: %w", AgentName, err)
		}
	}

	finalizer := map[string]any{
		"description": "Tool-less same-session BUILD terminal protocol finalizer.",
		"mode":        "primary",
		"steps":       2,
		"prompt":      finalizerSystemPrompt,
		"permission": map[string]string{
			"*": "deny",
		},
	}
	for _, key := range []string{"model", "variant"} {
		if raw, ok := worker[key]; ok {
			var value any
			if err := json.Unmarshal(raw, &value); err != nil {
				return "", fmt.Errorf("cannot parse worker %s for finalizer: %w", key, err)
			}
			finalizer[key] = value
		}
	}
	finalizerRaw, err := json.Marshal(finalizer)
	if err != nil {
		return "", fmt.Errorf("cannot encode finalizer agent: %w", err)
	}
	agents[FinalizerAgentName] = finalizerRaw
	agentsRaw, err := json.Marshal(agents)
	if err != nil {
		return "", fmt.Errorf("cannot encode finalizer agent block: %w", err)
	}
	root["agent"] = agentsRaw

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return "", fmt.Errorf("cannot encode finalizer config: %w", err)
	}
	return string(out), nil
}

func buildFinalizerChildEnv(worktreeDir string) ([]string, error) {
	merged, err := BuildFinalizerInlineConfig(os.Getenv(EnvConfigContent), worktreeDir)
	if err != nil {
		return nil, err
	}
	childEnv := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, EnvConfigContent+"=") {
			continue
		}
		childEnv = append(childEnv, kv)
	}
	childEnv = append(childEnv, EnvConfigContent+"="+merged)
	return childEnv, nil
}

// maybeFinalizeMalformedTerminal performs at most one tool-less, exact-session
// continuation. It never runs for missing text, corrupted streams, process
// failures, valid protocol, missing session identity, or conflicting session
// identity. Failure to start the repair leaves the original malformed attempt
// authoritative; a started repair must itself satisfy the normal strict parser.
func maybeFinalizeMalformedTerminal(
	ctx context.Context,
	bin, worktreeDir, sessionID string,
	primary *Attempt,
	stderrOut io.Writer,
) (*Attempt, error) {
	if primary == nil || primary.Tracker == nil || primary.StreamCorrupt || primary.SpawnErr != nil || primary.ExitErr != nil || primary.Cancelled {
		return primary, nil
	}
	text, ok := primary.Tracker.TerminalText()
	if !ok {
		return primary, nil
	}
	if _, err := result.Parse(text); err == nil {
		return primary, nil
	}
	if strings.TrimSpace(sessionID) == "" {
		return primary, nil
	}

	repaired, err := invokeFinalizer(ctx, bin, worktreeDir, sessionID, stderrOut)
	if err != nil {
		fmt.Fprintf(stderrOut, "[global-build] protocol finalization unavailable; preserving original malformed result: %v\n", err)
		return primary, nil
	}
	return repaired, nil
}

func invokeFinalizer(ctx context.Context, bin, worktreeDir, sessionID string, stderrOut io.Writer) (*Attempt, error) {
	childEnv, err := buildFinalizerChildEnv(worktreeDir)
	if err != nil {
		return nil, fmt.Errorf("cannot build finalizer environment: %w", err)
	}

	cmd := exec.CommandContext(ctx, bin,
		"run",
		"--format", "json",
		"--agent", FinalizerAgentName,
		"--session", sessionID,
		"--dir", worktreeDir,
	)
	cmd.Dir = worktreeDir
	cmd.Env = childEnv
	cmd.Stdin = strings.NewReader(finalizerRequest)
	cmd.WaitDelay = 3 * time.Second

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
	synced := newSyncedWriter(stderrOut)

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 0, 64*1024)
		scanner := newLargeScanner(stdout, &buf)
		for scanner.Scan() {
			if err := tracker.Observe(scanner.Text()); err != nil {
				corrupt = true
				fmt.Fprintf(synced, "[global-build] malformed stdout line from protocol finalizer: %v\n", err)
			}
		}
	}()

	errCopyDone := make(chan struct{})
	go func() {
		defer close(errCopyDone)
		_, _ = io.Copy(synced, stderr)
	}()

	waitErr := cmd.Wait()
	<-readDone
	<-errCopyDone

	attempt := &Attempt{Tracker: tracker, StreamCorrupt: corrupt}
	if ctx.Err() != nil && waitErr != nil {
		attempt.Cancelled = true
	}
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			attempt.ExitErr = ee
		} else {
			attempt.SpawnErr = waitErr
		}
	}
	return attempt, nil
}

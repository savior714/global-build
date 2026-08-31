package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	runtimepolicy "global-build/internal/runtime"
)

// runtimeCommand is the stable JSON adapter for the durable runtime.  The
// runtime package remains the authority; this command only loads, applies one
// event, atomically saves, and prints the resulting state.
type runtimeCommand struct {
	Type         string                             `json:"type"`
	TaskID       string                             `json:"task_id"`
	Task         runtimepolicy.Task                 `json:"task"`
	Candidate    runtimepolicy.Candidate            `json:"candidate"`
	Admission    runtimepolicy.Admission            `json:"admission"`
	Approval     runtimepolicy.Approval             `json:"approval"`
	Freshness    runtimepolicy.FreshnessObservation `json:"freshness"`
	Continuation runtimepolicy.Continuation         `json:"continuation"`
	Blocker      runtimepolicy.Blocker              `json:"blocker"`
	RejectKind   runtimepolicy.RejectKind           `json:"reject_kind"`
	Note         string                             `json:"note"`
	Publication  runtimepolicy.Publication          `json:"publication"`
	Frontier     runtimepolicy.FrontierEvidence     `json:"frontier"`
}

// runRuntime executes `runtime --state <path> apply`, where apply consumes
// one JSON event from stdin. `snapshot` and `stop-check` are read-only.
func runRuntime(args []string) int {
	statePath, action, err := parseRuntimeArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "global-build runtime: %v\n", err)
		return 40
	}
	lock, err := runtimepolicy.AcquireFileLock(statePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "global-build runtime: %v\n", err)
		return 40
	}
	defer lock.Release()

	manager, err := runtimepolicy.LoadFile(statePath)
	if errors.Is(err, os.ErrNotExist) && action == "apply" {
		manager = runtimepolicy.New()
		err = nil
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "global-build runtime: cannot load state: %v\n", err)
		return 40
	}

	switch action {
	case "snapshot":
		return emitRuntimeJSON(manager.Snapshot())
	case "stop-check":
		decision := manager.StopDecision()
		return emitRuntimeJSON(decision)
	case "apply":
		var command runtimeCommand
		if err := json.NewDecoder(os.Stdin).Decode(&command); err != nil {
			fmt.Fprintf(os.Stderr, "global-build runtime: cannot decode event: %v\n", err)
			return 40
		}
		if err := applyRuntimeCommand(manager, command); err != nil {
			fmt.Fprintf(os.Stderr, "global-build runtime: %v\n", err)
			return 40
		}
		if err := manager.Save(statePath); err != nil {
			fmt.Fprintf(os.Stderr, "global-build runtime: cannot save state: %v\n", err)
			return 40
		}
		return emitRuntimeJSON(manager.Snapshot())
	default:
		fmt.Fprintf(os.Stderr, "global-build runtime: unsupported action %q\n", action)
		return 40
	}
}

func parseRuntimeArgs(args []string) (string, string, error) {
	var statePath, action string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--state":
			if statePath != "" || i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
				return "", "", errors.New("--state requires exactly one path argument")
			}
			statePath = args[i+1]
			i++
		case "apply", "snapshot", "stop-check":
			if action != "" {
				return "", "", errors.New("runtime action may be given exactly once")
			}
			action = args[i]
		default:
			return "", "", fmt.Errorf("unknown runtime argument %q", args[i])
		}
	}
	if statePath == "" {
		return "", "", errors.New("runtime requires --state <path>")
	}
	if action == "" {
		return "", "", errors.New("runtime requires one of apply, snapshot, or stop-check")
	}
	return statePath, action, nil
}

func applyRuntimeCommand(manager *runtimepolicy.Manager, command runtimeCommand) error {
	if strings.TrimSpace(command.Type) == "" {
		return errors.New("runtime event requires type")
	}
	switch command.Type {
	case "add_task":
		return manager.AddTask(command.Task)
	case "publish_brief":
		return manager.PublishBrief(command.TaskID)
	case "reconstruct":
		return manager.Reconstruct(command.Frontier)
	case "reconcile":
		manager.Reconcile()
		return nil
	case "candidate_ready":
		return manager.PrepareCandidate(command.TaskID, command.Candidate, command.Admission)
	case "queue_approval":
		return manager.QueueApproval(command.TaskID)
	case "approve":
		return manager.Approve(command.TaskID, command.Approval, command.Freshness)
	case "reject":
		return manager.Reject(command.TaskID, command.RejectKind, command.Note)
	case "published":
		return manager.RecordPublished(command.TaskID, command.Publication)
	case "continuable":
		return manager.RecordContinuation(command.TaskID, command.Continuation)
	case "blocked":
		return manager.RecordBlocked(command.TaskID, command.Blocker)
	default:
		return fmt.Errorf("unknown runtime event type %q", command.Type)
	}
}

func emitRuntimeJSON(value any) int {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintf(os.Stderr, "global-build runtime: cannot encode result: %v\n", err)
		return 40
	}
	return 0
}

package runner

import "fmt"

// ErrKind is the small explicit internal error classification for the runner.
// It intentionally stays narrow: malformed input, malformed model result,
// timeout, worktree identity mismatch, candidate validation failure, transient
// transport failure, and everything else generic.
type ErrKind int

const (
	ErrMalformedInput ErrKind = iota
	ErrMalformedResult
	ErrTimeout
	ErrWorktreeIdentityMismatch
	ErrCandidateValidation
	ErrTransientTransport
	ErrGeneric
)

// ErrorKindString maps an internal classification to the stable ERROR_KIND
// value printed on stdout for RUNNER_ERROR exits.
func (k ErrKind) ErrorKindString() string {
	switch k {
	case ErrMalformedInput:
		return "MALFORMED_INPUT"
	case ErrTimeout:
		return "TIMEOUT"
	case ErrWorktreeIdentityMismatch:
		return "WORKTREE_IDENTITY_MISMATCH"
	case ErrCandidateValidation:
		return "CANDIDATE_VALIDATION_FAILED"
	case ErrGeneric:
		return "RUNNER_ERROR"
	default:
		return "RUNNER_ERROR"
	}
}

// RunError carries a classification plus a deterministic human message.
type RunError struct {
	Kind ErrKind
	Msg  string
}

func (e *RunError) Error() string { return e.Msg }

func runErr(kind ErrKind, format string, args ...any) *RunError {
	return &RunError{Kind: kind, Msg: fmt.Sprintf(format, args...)}
}

// errTimedOut is the sentinel cause used to cancel a timed-out attempt.
var errWallClock = &timeoutSignal{name: "WALL_CLOCK"}
var errNoProgress = &timeoutSignal{name: "NO_PROGRESS"}

type timeoutSignal struct{ name string }

func (t *timeoutSignal) Error() string { return "timeout: " + t.name }

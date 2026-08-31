package runtime

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// FileLock serializes separate runtime command processes that share one state
// document. The lock is a control-plane sidecar and carries no task payload.
type FileLock struct {
	file *os.File
}

// AcquireFileLock takes a nonblocking exclusive lock on statePath + ".lock".
// A concurrent writer therefore fails closed instead of silently losing a
// state transition through last-writer-wins replacement.
func AcquireFileLock(statePath string) (*FileLock, error) {
	if strings.TrimSpace(statePath) == "" {
		return nil, errors.New("runtime: empty state path for lock")
	}
	file, err := os.OpenFile(statePath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("runtime: open state lock: %w", err)
	}
	if err := tryExclusiveLock(file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("runtime: state is already owned by another process: %w", err)
	}
	return &FileLock{file: file}, nil
}

// Release relinquishes the state lock. The sidecar pathname is retained as a
// harmless stable lock target; only the advisory lock ownership is ephemeral.
func (l *FileLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := releaseExclusiveLock(l.file)
	closeErr := l.file.Close()
	l.file = nil
	if err != nil {
		return err
	}
	return closeErr
}

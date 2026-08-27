package opencode

import (
	"bytes"
	"io"
	"sync"
	"testing"
)

// unsafeWriter is an io.Writer that panics on concurrent Write calls.
// It is used to force-detect races in tests via the race detector.
type unsafeWriter struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (w *unsafeWriter) Write(p []byte) (int, error) {
	// Intentionally NOT protected by a mutex — concurrent writes will race.
	return w.data.Write(p)
}

// TestConcurrentDiagnosticWritesAreSynced verifies that the diagnostic writer
// used during an OpenCode subprocess invocation serializes concurrent writes.
// The stdout-reader goroutine and the stderr-forwarder goroutine both write to
// the same diagnostic writer; without synchronization this races on any
// non-concurrency-safe underlying io.Writer.
func TestConcurrentDiagnosticWritesAreSynced(t *testing.T) {
	// The syncedWriter must be safe for concurrent use even when the wrapped
	// writer is not. We prove this by feeding it an unsafeWriter from two
	// goroutines simultaneously and confirming no race is detected.
	w := &unsafeWriter{}
	synced := newSyncedWriter(w)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if _, err := synced.Write([]byte("diagnostic-line\n")); err != nil {
				t.Errorf("synced write failed: %v", err)
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if _, err := io.WriteString(synced, "stderr-forward\n"); err != nil {
				t.Errorf("synced write failed: %v", err)
				return
			}
		}
	}()

	wg.Wait()

	got := w.data.String()
	wantLines := 200
	if n := bytes.Count([]byte(got), []byte("\n")); n != wantLines {
		t.Errorf("expected %d lines, got %d", wantLines, n)
	}
}

// TestSyncedWriterPassthrough verifies that a syncedWriter correctly forwards
// writes to the underlying writer and preserves content.
func TestSyncedWriterPassthrough(t *testing.T) {
	var buf bytes.Buffer
	synced := newSyncedWriter(&buf)

	if _, err := synced.Write([]byte("hello ")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := io.WriteString(synced, "world"); err != nil {
		t.Fatalf("WriteString: %v", err)
	}

	if got := buf.String(); got != "hello world" {
		t.Errorf("buf.String() = %q, want %q", got, "hello world")
	}
}

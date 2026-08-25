package ownership

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// joinPath derives the deterministic lease path: <cacheRoot>/<repoID>/<runID>.
func joinPath(cacheRoot, repoID, runID string) string {
	return filepath.Join(cacheRoot, repoID, runID)
}

func dirOf(p string) string { return filepath.Dir(p) }

func mkdirAll(dir string) error { return os.MkdirAll(dir, 0o755) }

func encodeJSON(f *os.File, v any) error {
	return json.NewEncoder(f).Encode(v)
}

func decodeJSON(f *os.File, v any) error {
	return json.NewDecoder(f).Decode(v)
}

// openExisting opens the lease path for read/write without creating it. It
// returns ok=false (and no error) when the pathname does not exist, so callers
// never manufacture missing liveness evidence.
func (l *Lease) openExisting() (*os.File, bool, error) {
	f, err := os.OpenFile(l.path, os.O_RDWR, 0o644)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return f, true, nil
}

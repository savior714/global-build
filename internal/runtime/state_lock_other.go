//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package runtime

import (
	"errors"
	"os"
)

func tryExclusiveLock(file *os.File) error {
	return errors.New("runtime: advisory state locking is unavailable on this platform")
}

func releaseExclusiveLock(file *os.File) error { return nil }

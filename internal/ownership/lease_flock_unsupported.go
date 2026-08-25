//go:build !unix

package ownership

import "errors"

// errFlockUnsupported means the current platform cannot provide safe
// nonblocking exclusive advisory locking without an inappropriate dependency.
// Per the Slice 2 contract, destructive apply must fail closed here rather than
// weaken the liveness proof.
var errFlockUnsupported = errors.New("ownership: advisory flock unavailable on this platform")

func flockAcquireExclusiveNB(fd uintptr) error { return errFlockUnsupported }

func flockRelease(fd uintptr) error { return errFlockUnsupported }

//go:build unix

package ownership

import "syscall"

// flockAcquireExclusiveNB takes a nonblocking exclusive advisory lock on the
// open file descriptor. If another process already holds it, it returns
// errLeaseHeld so callers can classify the candidate as uncertain. On platforms
// without safe nonblocking advisory locking this file is replaced by the
// !unix build, which fails closed for destructive apply.
func flockAcquireExclusiveNB(fd uintptr) error {
	if err := syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if err == syscall.EWOULDBLOCK {
			return errLeaseHeld
		}
		return err
	}
	return nil
}

func flockRelease(fd uintptr) error {
	return syscall.Flock(int(fd), syscall.LOCK_UN)
}

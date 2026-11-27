//go:build unix

package engine

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func verifySafeDirUnix(path string, fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	euid := uint32(os.Geteuid())
	if st.Uid != euid && st.Uid != 0 {
		return fmt.Errorf("%w: directory %q owned by uid %d (expected %d)", ErrUnsafeHomeDir, path, st.Uid, euid)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: directory %q is group/world writable (mode %04o)", ErrUnsafeHomeDir, path, fi.Mode().Perm())
	}
	return nil
}

func acquireWorktreeLock(lockFile *os.File) error {
	return unix.Flock(int(lockFile.Fd()), unix.LOCK_EX)
}

func releaseWorktreeLock(lockFile *os.File) {
	if lockFile != nil {
		_ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
	}
}

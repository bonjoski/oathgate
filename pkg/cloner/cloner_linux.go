//go:build linux

package cloner

import (
	"context"
	"os"

	"golang.org/x/sys/unix"
)

type linuxCloner struct{}

// New returns the Linux FICLONE ioctl CoW Cloner (with transparent copy fallback on ext4/tmpfs).
func New() Cloner {
	return &linuxCloner{}
}

func (c *linuxCloner) BackendName() string {
	return BackendLinuxFIClone
}

func (c *linuxCloner) CloneTree(ctx context.Context, srcRoot, dstRoot string, opts CloneOptions) (*CloneStats, error) {
	opts = normalizeOpts(opts)
	rep, err := ValidateTree(ctx, srcRoot, opts)
	if err != nil {
		return nil, err
	}
	return copyTreeSafe(ctx, srcRoot, dstRoot, opts, rep, func(dstFile, srcFile *os.File) bool {
		return unix.IoctlFileClone(int(dstFile.Fd()), int(srcFile.Fd())) == nil
	}, BackendLinuxFIClone)
}

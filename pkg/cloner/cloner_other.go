//go:build !darwin && !linux

package cloner

import "context"

type fallbackCloner struct{}

// New returns the portable os.OpenRoot safe copy cloner.
func New() Cloner {
	return &fallbackCloner{}
}

func (c *fallbackCloner) BackendName() string {
	return BackendCopyFallback
}

func (c *fallbackCloner) CloneTree(ctx context.Context, srcRoot, dstRoot string, opts CloneOptions) (*CloneStats, error) {
	opts = normalizeOpts(opts)
	rep, err := ValidateTree(ctx, srcRoot, opts)
	if err != nil {
		return nil, err
	}
	return copyTreeSafe(ctx, srcRoot, dstRoot, opts, rep, nil, "")
}

//go:build darwin

package cloner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

type darwinCloner struct{}

// New returns the platform-optimized CoW Cloner (APFS clonefile(2) on macOS).
func New() Cloner {
	return &darwinCloner{}
}

func (c *darwinCloner) BackendName() string {
	return BackendAPFSClonefile
}

func (c *darwinCloner) CloneTree(ctx context.Context, srcRoot, dstRoot string, opts CloneOptions) (*CloneStats, error) {
	opts = normalizeOpts(opts)
	rep, err := ValidateTree(ctx, srcRoot, opts)
	if err != nil {
		return nil, err
	}

	realSrc, err := filepath.EvalSymlinks(srcRoot)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dstRoot, 0o700); err != nil {
		return nil, err
	}
	realDst, err := filepath.EvalSymlinks(dstRoot)
	if err != nil {
		return nil, err
	}

	// If any skipped relative path (like .git/index.lock) is present inside a top-level dir,
	// clone those top-level dirs via safe tree copy while still using O(1) clonefile for all other top-level entries!
	topNeedsSelective := make(map[string]bool)
	for rel := range rep.skippedRelHits {
		top := rel
		if idx := firstSlash(rel); idx > 0 {
			top = rel[:idx]
		}
		topNeedsSelective[top] = true
	}

	entries, err := os.ReadDir(realSrc)
	if err != nil {
		return nil, err
	}

	usedAPFS := false
	usedFallback := false
	for _, e := range entries {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		name := e.Name()
		if opts.SkipTop[name] {
			continue
		}
		srcEntry := filepath.Join(realSrc, name)
		dstEntry := filepath.Join(realDst, name)

		if topNeedsSelective[name] {
			subOpts := opts
			subOpts.SkipTop = map[string]bool{}
			subOpts.SkipRel = rebaseSkipRel(opts.SkipRel, name)
			subRep, verr := ValidateTree(ctx, srcEntry, subOpts)
			if verr != nil {
				return nil, verr
			}
			if _, cerr := copyTreeSafe(ctx, srcEntry, dstEntry, subOpts, subRep, nil, ""); cerr != nil {
				return nil, cerr
			}
			usedFallback = true
			continue
		}

		// O(1) APFS clonefile(2) with CLONE_NOFOLLOW — never dereferences symlinks.
		err := unix.Clonefile(srcEntry, dstEntry, unix.CLONE_NOFOLLOW)
		if err == nil {
			usedAPFS = true
			continue
		}
		if errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) {
			// Cross-volume or non-APFS filesystem: fall back cleanly to OpenRoot copy.
			return copyTreeSafe(ctx, realSrc, realDst, opts, rep, nil, "")
		}
		return nil, fmt.Errorf("clonefile(%q -> %q): %w", name, dstEntry, err)
	}

	// Unconditionally strip setuid/setgid/sticky bits from any cloned entries via os.OpenRoot.
	if len(rep.specialBitPaths) > 0 {
		dRoot, err := os.OpenRoot(realDst)
		if err != nil {
			return nil, err
		}
		for _, rel := range rep.specialBitPaths {
			if fi, serr := dRoot.Lstat(rel); serr == nil && fi.Mode()&os.ModeSymlink == 0 {
				_ = dRoot.Chmod(rel, stripSpecialMode(fi.Mode()))
			}
		}
		_ = dRoot.Close()
	}

	backend := BackendAPFSClonefile
	if !usedAPFS && usedFallback {
		backend = BackendCopyFallback
	}
	return &CloneStats{
		Backend:        backend,
		EntriesCloned:  rep.entries,
		BytesLogical:   rep.bytesLogical,
		SetuidStripped: len(rep.specialBitPaths),
	}, nil
}

func firstSlash(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

func rebaseSkipRel(m map[string]bool, prefix string) map[string]bool {
	out := make(map[string]bool)
	p := prefix + "/"
	for k, v := range m {
		if v && len(k) > len(p) && k[:len(p)] == p {
			out[k[len(p):]] = true
		}
	}
	return out
}

package cloner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	BackendAPFSClonefile = "apfs_clonefile"
	BackendLinuxFIClone  = "linux_ficlone"
	BackendCopyFallback  = "copy_fallback"

	DefaultMaxDepth   = 64
	DefaultMaxEntries = 250_000
)

var (
	ErrEscapingSymlink    = errors.New("oathgate security violation: escaping symlink refused fail-closed")
	ErrSpecialFileBlocked = errors.New("oathgate security violation: special file (FIFO/socket/device) refused fail-closed")
	ErrMaxDepthExceeded   = errors.New("oathgate security violation: directory depth exceeds max_depth limit")
	ErrMaxEntriesExceeded = errors.New("oathgate security violation: workspace entry count exceeds max_entries limit")
	ErrInvalidRoot        = errors.New("oathgate security violation: invalid source or destination root")
)

// CloneOptions configures fail-closed workspace cloning boundaries.
type CloneOptions struct {
	MaxDepth   int
	MaxEntries int
	SkipTop    map[string]bool // Top-level names to skip (e.g. ".oathgate")
	SkipRel    map[string]bool // Relative slash paths to skip (e.g. ".git/index.lock")
}

// CloneStats reports cloning telemetry and backend used.
type CloneStats struct {
	Backend       string `json:"backend"`
	EntriesCloned int    `json:"entries_cloned"`
	BytesLogical  int64  `json:"bytes_logical"`
	SetuidStripped int   `json:"setuid_stripped"`
}

// Cloner abstracts OS Copy-on-Write tree cloning.
type Cloner interface {
	BackendName() string
	CloneTree(ctx context.Context, srcRoot, dstRoot string, opts CloneOptions) (*CloneStats, error)
}

// DefaultOptions returns hardened RFC-2026-11-25-62 cloning bounds.
func DefaultOptions() CloneOptions {
	return CloneOptions{
		MaxDepth:   DefaultMaxDepth,
		MaxEntries: DefaultMaxEntries,
		SkipTop: map[string]bool{
			".oathgate":      true,
			".oathgate.lock": true,
		},
		SkipRel: map[string]bool{
			".git/index.lock": true,
		},
	}
}

func normalizeOpts(opts CloneOptions) CloneOptions {
	def := DefaultOptions()
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = def.MaxDepth
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = def.MaxEntries
	}
	if opts.SkipTop == nil {
		opts.SkipTop = def.SkipTop
	}
	if opts.SkipRel == nil {
		opts.SkipRel = def.SkipRel
	}
	return opts
}

type validationReport struct {
	entries         int
	bytesLogical    int64
	specialBitPaths []string // relative slash paths carrying setuid/setgid/sticky
	skippedRelHits  map[string]bool
}

// ValidateTree inspects srcRoot via os.OpenRoot + Lstat without following symlinks,
// rejecting escaping symlinks, FIFOs/sockets/devices, and excessive depth/entry counts.
func ValidateTree(ctx context.Context, srcRoot string, opts CloneOptions) (*validationReport, error) {
	opts = normalizeOpts(opts)
	absSrc, err := filepath.Abs(srcRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRoot, err)
	}
	// Resolve real path of root itself (e.g. /var -> /private/var on macOS) for accurate containment checks.
	realSrc, err := filepath.EvalSymlinks(absSrc)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRoot, err)
	}
	fi, err := os.Stat(realSrc)
	if err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%w: %q is not a directory", ErrInvalidRoot, srcRoot)
	}

	root, err := os.OpenRoot(realSrc)
	if err != nil {
		return nil, fmt.Errorf("open source root: %w", err)
	}
	defer func() { _ = root.Close() }()

	rep := &validationReport{
		skippedRelHits: make(map[string]bool),
	}
	if err := walkValidate(ctx, root, realSrc, "", 0, opts, rep); err != nil {
		return nil, err
	}
	return rep, nil
}

func walkValidate(ctx context.Context, root *os.Root, realSrc, relDir string, depth int, opts CloneOptions, rep *validationReport) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if depth > opts.MaxDepth {
		return fmt.Errorf("%w (depth %d > %d at %q)", ErrMaxDepthExceeded, depth, opts.MaxDepth, relDir)
	}

	readPath := realSrc
	if relDir != "" {
		readPath = filepath.Join(realSrc, filepath.FromSlash(relDir))
	}
	entries, err := os.ReadDir(readPath)
	if err != nil {
		return fmt.Errorf("read dir %q: %w", relDir, err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if depth == 0 && opts.SkipTop[name] {
			continue
		}
		rel := name
		if relDir != "" {
			rel = relDir + "/" + name
		}
		if opts.SkipRel[rel] {
			rep.skippedRelHits[rel] = true
			continue
		}

		rep.entries++
		if rep.entries > opts.MaxEntries {
			return fmt.Errorf("%w (%d > %d)", ErrMaxEntriesExceeded, rep.entries, opts.MaxEntries)
		}

		fi, err := root.Lstat(rel)
		if err != nil {
			return fmt.Errorf("lstat %q: %w", rel, err)
		}
		mode := fi.Mode()

		// Block FIFOs, sockets, and block/char device nodes fail-closed.
		if mode&(os.ModeNamedPipe|os.ModeSocket|os.ModeDevice|os.ModeCharDevice) != 0 {
			return fmt.Errorf("%w: %s (mode %s)", ErrSpecialFileBlocked, rel, mode.String())
		}

		// Record any setuid/setgid/sticky bits so they are unconditionally stripped.
		if mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			rep.specialBitPaths = append(rep.specialBitPaths, rel)
		}

		if mode&os.ModeSymlink != 0 {
			target, err := root.Readlink(rel)
			if err != nil {
				return fmt.Errorf("readlink %q: %w", rel, err)
			}
			if !symlinkContainedInRoot(realSrc, rel, target) {
				return fmt.Errorf("%w: %s -> %s", ErrEscapingSymlink, rel, target)
			}
			continue
		}

		if mode.IsDir() {
			if err := walkValidate(ctx, root, realSrc, rel, depth+1, opts, rep); err != nil {
				return err
			}
			continue
		}

		if mode.IsRegular() {
			rep.bytesLogical += fi.Size()
		}
	}
	return nil
}

// symlinkContainedInRoot checks whether a symlink at rel inside realSrc resolves strictly within realSrc.
func symlinkContainedInRoot(realSrc, rel, target string) bool {
	if strings.TrimSpace(target) == "" || strings.ContainsRune(target, '\x00') {
		return false
	}
	var resolved string
	if filepath.IsAbs(target) {
		resolved = filepath.Clean(target)
	} else {
		parent := filepath.Dir(filepath.Join(realSrc, filepath.FromSlash(rel)))
		resolved = filepath.Clean(filepath.Join(parent, target))
	}
	// If the target exists on disk, also evaluate intermediate symlinks to defeat chained symlink escapes.
	if eval, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = eval
	}
	r, err := filepath.Rel(realSrc, resolved)
	if err != nil {
		return false
	}
	if r == ".." || strings.HasPrefix(r, ".."+string(os.PathSeparator)) || filepath.IsAbs(r) {
		return false
	}
	return true
}

// stripSpecialMode clears setuid, setgid, and sticky bits while keeping standard rwx permissions.
func stripSpecialMode(m os.FileMode) os.FileMode {
	perm := m.Perm()
	if perm == 0 {
		perm = 0o600
	}
	return perm &^ (os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
}

// copyTreeSafe performs an os.OpenRoot-confined recursive copy with optional per-file CoW hook.
func copyTreeSafe(
	ctx context.Context,
	srcRoot, dstRoot string,
	opts CloneOptions,
	rep *validationReport,
	fileCloneFn func(dstFile, srcFile *os.File) bool,
	cowBackend string,
) (*CloneStats, error) {
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

	sRoot, err := os.OpenRoot(realSrc)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sRoot.Close() }()

	dRoot, err := os.OpenRoot(realDst)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dRoot.Close() }()

	stats := &CloneStats{
		Backend:        BackendCopyFallback,
		EntriesCloned:  rep.entries,
		BytesLogical:   rep.bytesLogical,
		SetuidStripped: len(rep.specialBitPaths),
	}
	usedCoW := false

	var copyDir func(relDir string, depth int) error
	copyDir = func(relDir string, depth int) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		readPath := realSrc
		if relDir != "" {
			readPath = filepath.Join(realSrc, filepath.FromSlash(relDir))
		}
		entries, err := os.ReadDir(readPath)
		if err != nil {
			return err
		}
		for _, e := range entries {
			name := e.Name()
			if depth == 0 && opts.SkipTop[name] {
				continue
			}
			rel := name
			if relDir != "" {
				rel = relDir + "/" + name
			}
			if opts.SkipRel[rel] {
				continue
			}

			fi, err := sRoot.Lstat(rel)
			if err != nil {
				return err
			}
			mode := fi.Mode()

			if mode&os.ModeSymlink != 0 {
				target, err := sRoot.Readlink(rel)
				if err != nil {
					return err
				}
				if err := dRoot.Symlink(target, rel); err != nil {
					return err
				}
				continue
			}

			if mode.IsDir() {
				dirPerm := stripSpecialMode(mode) | 0o700
				if err := dRoot.Mkdir(rel, dirPerm); err != nil && !os.IsExist(err) {
					return err
				}
				if err := copyDir(rel, depth+1); err != nil {
					return err
				}
				continue
			}

			if mode.IsRegular() {
				sf, err := sRoot.Open(rel)
				if err != nil {
					return err
				}
				cleanPerm := stripSpecialMode(mode)
				df, err := dRoot.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, cleanPerm)
				if err != nil {
					_ = sf.Close()
					return err
				}
				cloned := false
				if fileCloneFn != nil {
					cloned = fileCloneFn(df, sf)
					if cloned {
						usedCoW = true
					}
				}
				if !cloned {
					if _, err := io.Copy(df, sf); err != nil {
						_ = df.Close()
						_ = sf.Close()
						return err
					}
				}
				_ = df.Chmod(cleanPerm)
				_ = sf.Close()
				if err := df.Close(); err != nil {
					return err
				}
			}
		}
		return nil
	}

	if err := copyDir("", 0); err != nil {
		return nil, err
	}
	if usedCoW && cowBackend != "" {
		stats.Backend = cowBackend
	}
	return stats, nil
}

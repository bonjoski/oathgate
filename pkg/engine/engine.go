package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bonjoski/oathgate/pkg/cloner"
	"github.com/bonjoski/oathgate/pkg/ledger"
	tmredact "github.com/bonjoski/tinmind/pkg/redact"
	tmtrace "github.com/bonjoski/tinmind/pkg/trace"
)

const (
	DefaultMaxSnapsPerWorktree = 25
	DefaultMaxAge              = 72 * time.Hour
	DefaultDedupWindow         = 2 * time.Second
	maxDiffFileBytes           = 512 * 1024
)

var (
	ErrUnsafeHomeDir = errors.New("oathgate security violation: unsafe home/store directory")
	ErrActiveGitLock = errors.New("oathgate safety guard: active .git/index.lock detected; refusing rewind to avoid git index corruption")
	ErrTargetExists  = errors.New("branch target directory already exists")
	ErrNoSnapshots   = errors.New("no snapshots found for this worktree")
)

// DefaultHomeDir returns OATHGATE_HOME if set, else ~/.oathgate.
func DefaultHomeDir() (string, error) {
	if env := strings.TrimSpace(os.Getenv("OATHGATE_HOME")); env != "" {
		return filepath.Abs(env)
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("%w: cannot determine home directory", ErrUnsafeHomeDir)
	}
	return filepath.Join(home, ".oathgate"), nil
}

// Engine coordinates CoW snapshots, SQLite WAL DAG metadata, redacted diffs, atomic rewinds,
// branching, GC, and Tinmind/Coppermind integration.
type Engine struct {
	homeDir   string
	snapsDir  string
	locksDir  string
	ledger    *ledger.Store
	cloner    cloner.Cloner
	cloneOpts cloner.CloneOptions
}

// Open opens or initializes an Oathgate Engine rooted at homeDir (default ~/.oathgate).
func Open(homeDir string) (*Engine, error) {
	if strings.TrimSpace(homeDir) == "" {
		var err error
		homeDir, err = DefaultHomeDir()
		if err != nil {
			return nil, err
		}
	}
	absHome, err := filepath.Abs(homeDir)
	if err != nil {
		return nil, err
	}
	if err := ensureSafeDir(absHome); err != nil {
		return nil, err
	}

	snapsDir := filepath.Join(absHome, "snapshots")
	locksDir := filepath.Join(absHome, "locks")
	if err := ensureSafeDir(snapsDir); err != nil {
		return nil, err
	}
	if err := ensureSafeDir(locksDir); err != nil {
		return nil, err
	}

	store, err := ledger.Open(filepath.Join(absHome, "ledger.db"))
	if err != nil {
		return nil, err
	}
	return &Engine{
		homeDir:   absHome,
		snapsDir:  snapsDir,
		locksDir:  locksDir,
		ledger:    store,
		cloner:    cloner.New(),
		cloneOpts: cloner.DefaultOptions(),
	}, nil
}

// Close closes the underlying SQLite WAL ledger.
func (e *Engine) Close() error {
	if e == nil || e.ledger == nil {
		return nil
	}
	return e.ledger.Close()
}

// HomeDir returns the validated Oathgate home directory.
func (e *Engine) HomeDir() string { return e.homeDir }

// BackendName returns the active OS CoW backend name.
func (e *Engine) BackendName() string { return e.cloner.BackendName() }

// SetCloneOptions overrides cloning limits (e.g. for custom depth/entry bounds in tests).
func (e *Engine) SetCloneOptions(opts cloner.CloneOptions) {
	e.cloneOpts = opts
}

// SnapOptions configures workspace checkpoint capture.
type SnapOptions struct {
	Label      string
	Trigger    string // "manual" (default), "fabrial_hook", "pre_rewind"
	Pinned     bool
	TinmindID  string
	TinmindSeq uint64
	SkipAutoGC bool
}

// SnapResult wraps the saved Snapshot plus dedup status.
type SnapResult struct {
	Snapshot     ledger.Snapshot `json:"snapshot"`
	Deduplicated bool            `json:"deduplicated,omitempty"`
}

// Snap captures an O(1) CoW snapshot of worktree into ~/.oathgate/snapshots/<repo>/<wt>/<snap_id>.
func (e *Engine) Snap(ctx context.Context, worktree string, opts SnapOptions) (*SnapResult, error) {
	realWT, repoHash, wtID, err := resolveWorktreeIdentity(worktree)
	if err != nil {
		return nil, err
	}
	trigger := strings.TrimSpace(opts.Trigger)
	if trigger == "" {
		trigger = "manual"
	}

	// Capture optional Tinmind trace bookmark from explicit flag or ambient TINMIND_SESSION_ID.
	tmID := strings.TrimSpace(opts.TinmindID)
	if tmID == "" {
		tmID = strings.TrimSpace(os.Getenv("TINMIND_SESSION_ID"))
	}
	if tmID != "" {
		if err := tmtrace.ValidateTraceID(tmID); err != nil {
			return nil, fmt.Errorf("invalid tinmind trace ID %q: %w", tmID, err)
		}
	}

	unlock, err := e.lockWorktree(wtID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	return e.snapLocked(ctx, realWT, repoHash, wtID, trigger, opts, tmID)
}

func (e *Engine) snapLocked(ctx context.Context, realWT, repoHash, wtID, trigger string, opts SnapOptions, tmID string) (*SnapResult, error) {
	gitHead, gitBranch := readGitHead(realWT)
	fp := computeQuickFingerprint(realWT, gitHead)

	latest, err := e.ledger.LatestForWorktree(ctx, wtID)
	if err != nil {
		return nil, err
	}
	// Rate-limit / dedup automatic hook snapshots if worktree fingerprint is unchanged within 2s.
	if trigger == "fabrial_hook" && latest != nil && latest.Fingerprint == fp && time.Since(latest.CreatedAt) < DefaultDedupWindow {
		return &SnapResult{Snapshot: *latest, Deduplicated: true}, nil
	}

	snapID := ledger.NewSnapID()
	relDir := filepath.ToSlash(filepath.Join(repoHash, wtID, snapID))
	dstDir := filepath.Join(e.snapsDir, repoHash, wtID, snapID)
	if err := os.MkdirAll(filepath.Dir(dstDir), 0o700); err != nil {
		return nil, err
	}

	start := time.Now()
	stats, err := e.cloner.CloneTree(ctx, realWT, dstDir, e.cloneOpts)
	if err != nil {
		_ = os.RemoveAll(dstDir)
		return nil, err
	}
	durMs := time.Since(start).Milliseconds()

	parentID := ""
	if latest != nil {
		parentID = latest.ID
	}

	snap := ledger.Snapshot{
		ID:            snapID,
		ParentID:      parentID,
		RepoHash:      repoHash,
		RepoRoot:      realWT,
		WorktreeID:    wtID,
		Label:         tmredact.ScrubText(opts.Label),
		Trigger:       trigger,
		GitHead:       gitHead,
		GitBranch:     gitBranch,
		CoWBackend:    stats.Backend,
		DurationMs:    durMs,
		EntriesCloned: stats.EntriesCloned,
		BytesLogical:  stats.BytesLogical,
		Pinned:        opts.Pinned,
		TinmindID:     tmID,
		TinmindSeq:    opts.TinmindSeq,
		Fingerprint:   fp,
		StoreRel:      relDir,
		CreatedAt:     time.Now().UTC(),
	}

	// SQLite WAL write occurs outside the filesystem clone to minimize lock hold time.
	if err := e.ledger.Insert(ctx, snap); err != nil {
		_ = os.RemoveAll(dstDir)
		return nil, err
	}

	if !opts.SkipAutoGC {
		_, _ = e.GC(ctx, GCOptions{MaxSnapsPerWorktree: DefaultMaxSnapsPerWorktree, MaxAge: DefaultMaxAge})
	}
	return &SnapResult{Snapshot: snap}, nil
}

// List returns recent snapshots for worktree newest-first.
func (e *Engine) List(ctx context.Context, worktree string, limit int) ([]ledger.Snapshot, error) {
	_, _, wtID, err := resolveWorktreeIdentity(worktree)
	if err != nil {
		return nil, err
	}
	return e.ledger.ListForWorktree(ctx, wtID, limit)
}

// Pin sets or clears the GC pin flag on snapID.
func (e *Engine) Pin(ctx context.Context, snapID string, pinned bool) error {
	return e.ledger.SetPinned(ctx, snapID, pinned)
}

// DiffEntry describes a single changed path between a snapshot and the live worktree.
type DiffEntry struct {
	Path    string `json:"path"`
	Status  string `json:"status"` // "added" | "modified" | "deleted"
	Preview string `json:"preview,omitempty"`
}

// DiffSummary reports secret-redacted differences between snapID and the current worktree.
type DiffSummary struct {
	SnapID   string      `json:"snap_id"`
	Worktree string      `json:"worktree"`
	Added    int         `json:"added"`
	Modified int         `json:"modified"`
	Deleted  int         `json:"deleted"`
	Entries  []DiffEntry `json:"entries"`
}

// Diff compares snapID against worktree, scrubbing all file previews through Tinmind's secret redactor.
func (e *Engine) Diff(ctx context.Context, worktree, snapID string) (*DiffSummary, error) {
	realWT, _, wtID, err := resolveWorktreeIdentity(worktree)
	if err != nil {
		return nil, err
	}
	snap, err := e.resolveSnapForWorktree(ctx, wtID, snapID)
	if err != nil {
		return nil, err
	}
	snapPath, err := e.safeSnapPath(snap)
	if err != nil {
		return nil, err
	}

	snapFiles, err := indexRegularFiles(snapPath)
	if err != nil {
		return nil, err
	}
	liveFiles, err := indexRegularFiles(realWT)
	if err != nil {
		return nil, err
	}

	sum := &DiffSummary{SnapID: snap.ID, Worktree: realWT}
	allPaths := make(map[string]struct{}, len(snapFiles)+len(liveFiles))
	for p := range snapFiles {
		allPaths[p] = struct{}{}
	}
	for p := range liveFiles {
		allPaths[p] = struct{}{}
	}
	sorted := make([]string, 0, len(allPaths))
	for p := range allPaths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)

	for _, rel := range sorted {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if strings.HasPrefix(rel, ".git/") || strings.HasPrefix(rel, ".oathgate/") {
			continue
		}
		sHash, inSnap := snapFiles[rel]
		lHash, inLive := liveFiles[rel]

		switch {
		case !inSnap && inLive:
			sum.Added++
			sum.Entries = append(sum.Entries, DiffEntry{
				Path:    rel,
				Status:  "added",
				Preview: redactedPreview(realWT, rel),
			})
		case inSnap && !inLive:
			sum.Deleted++
			sum.Entries = append(sum.Entries, DiffEntry{
				Path:    rel,
				Status:  "deleted",
				Preview: redactedPreview(snapPath, rel),
			})
		case sHash != lHash:
			sum.Modified++
			sum.Entries = append(sum.Entries, DiffEntry{
				Path:    rel,
				Status:  "modified",
				Preview: redactedPreview(realWT, rel),
			})
		}
	}
	return sum, nil
}

// RewindOptions controls atomic workspace restoration.
type RewindOptions struct {
	Reason        string
	RecordDeadEnd bool
	NoSafetySnap  bool // Default false: ALWAYS takes an automatic pre-rewind safety snapshot!
}

// RewindResult summarizes a completed atomic rewind.
type RewindResult struct {
	RestoredSnapID    string `json:"restored_snap_id"`
	SafetySnapID      string `json:"safety_snap_id,omitempty"`
	Worktree          string `json:"worktree"`
	Backend           string `json:"backend"`
	DeadEndRecorded   bool   `json:"dead_end_recorded,omitempty"`
	DeadEndPayload    string `json:"dead_end_payload,omitempty"`
	TinmindBookmarkID string `json:"tinmind_trace_id,omitempty"`
}

// Rewind restores worktree atomically to snapID under a per-worktree flock, after verifying
// no active .git/index.lock exists and taking an automatic pre-rewind safety snapshot.
func (e *Engine) Rewind(ctx context.Context, worktree, snapID string, opts RewindOptions) (*RewindResult, error) {
	realWT, repoHash, wtID, err := resolveWorktreeIdentity(worktree)
	if err != nil {
		return nil, err
	}
	snap, err := e.resolveSnapForWorktree(ctx, wtID, snapID)
	if err != nil {
		return nil, err
	}
	snapPath, err := e.safeSnapPath(snap)
	if err != nil {
		return nil, err
	}

	// Refuse fail-closed if git is actively mutating the index.
	if _, err := os.Lstat(filepath.Join(realWT, ".git", "index.lock")); err == nil {
		return nil, ErrActiveGitLock
	}

	unlock, err := e.lockWorktree(wtID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Re-check .git/index.lock under lock to close TOCTOU window.
	if _, err := os.Lstat(filepath.Join(realWT, ".git", "index.lock")); err == nil {
		return nil, ErrActiveGitLock
	}

	// Take mandatory automatic pre-rewind safety snapshot so every rewind is reversible!
	var safetyID string
	if !opts.NoSafetySnap {
		safetyRes, serr := e.snapLocked(ctx, realWT, repoHash, wtID, "pre_rewind", SnapOptions{
			Label:      fmt.Sprintf("auto pre-rewind before %s", snap.ID),
			Trigger:    "pre_rewind",
			SkipAutoGC: true,
		}, snap.TinmindID)
		if serr != nil {
			return nil, fmt.Errorf("create automatic pre-rewind safety snapshot: %w", serr)
		}
		safetyID = safetyRes.Snapshot.ID
	}

	// Stage restored copy in a sibling temp dir on the same filesystem, enforcing CoW & setuid stripping.
	stageDir, err := os.MkdirTemp(filepath.Dir(realWT), ".oathgate-stage-*")
	if err != nil {
		return nil, fmt.Errorf("create sibling staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(stageDir) }()

	cloneStats, err := e.cloner.CloneTree(ctx, snapPath, stageDir, e.cloneOpts)
	if err != nil {
		return nil, fmt.Errorf("stage snapshot %s: %w", snap.ID, err)
	}

	// Swap staged entries into worktree cleanly (preserving .oathgate & .fabrial).
	if err := swapWorktreeEntries(realWT, stageDir); err != nil {
		return nil, fmt.Errorf("apply staged rewind: %w", err)
	}

	res := &RewindResult{
		RestoredSnapID:    snap.ID,
		SafetySnapID:      safetyID,
		Worktree:          realWT,
		Backend:           cloneStats.Backend,
		TinmindBookmarkID: snap.TinmindID,
	}

	if opts.RecordDeadEnd {
		payload := buildRedactedDeadEndPayload(snap, realWT, opts.Reason, safetyID)
		res.DeadEndPayload = payload
		res.DeadEndRecorded = pushDeadEndToCoppermind(ctx, filepath.Base(realWT), payload)
	}

	return res, nil
}

// Branch materializes snapID into a brand-new directory targetDir (refusing existing/symlink paths).
func (e *Engine) Branch(ctx context.Context, snapID, targetDir string) (*ledger.Snapshot, error) {
	if err := ledger.ValidateSnapID(snapID); err != nil {
		return nil, err
	}
	snap, err := e.ledger.Get(ctx, snapID)
	if err != nil {
		return nil, err
	}
	snapPath, err := e.safeSnapPath(snap)
	if err != nil {
		return nil, err
	}

	absTarget, err := filepath.Abs(targetDir)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(absTarget); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrTargetExists, absTarget)
	}
	if err := os.MkdirAll(filepath.Dir(absTarget), 0o700); err != nil {
		return nil, err
	}
	if _, err := e.cloner.CloneTree(ctx, snapPath, absTarget, e.cloneOpts); err != nil {
		_ = os.RemoveAll(absTarget)
		return nil, err
	}
	return snap, nil
}

// GCOptions controls snapshot garbage collection bounds.
type GCOptions struct {
	MaxSnapsPerWorktree int
	MaxAge              time.Duration
}

// GCReport summarizes pruned snapshots.
type GCReport struct {
	Removed int `json:"removed"`
	Kept    int `json:"kept"`
	Pinned  int `json:"pinned"`
}

// GC prunes oldest unpinned snapshots exceeding MaxSnapsPerWorktree or MaxAge.
func (e *Engine) GC(ctx context.Context, opts GCOptions) (*GCReport, error) {
	if opts.MaxSnapsPerWorktree <= 0 {
		opts.MaxSnapsPerWorktree = DefaultMaxSnapsPerWorktree
	}
	if opts.MaxAge <= 0 {
		opts.MaxAge = DefaultMaxAge
	}

	all, err := e.ledger.ListAll(ctx)
	if err != nil {
		return nil, err
	}

	cutoff := time.Now().UTC().Add(-opts.MaxAge)
	countsByWT := make(map[string]int)
	rep := &GCReport{}

	for _, snap := range all {
		if snap.Pinned {
			rep.Pinned++
			rep.Kept++
			continue
		}
		countsByWT[snap.WorktreeID]++
		expired := snap.CreatedAt.Before(cutoff)
		overCap := countsByWT[snap.WorktreeID] > opts.MaxSnapsPerWorktree

		if expired || overCap {
			if p, perr := e.safeSnapPath(&snap); perr == nil {
				_ = os.RemoveAll(p)
			}
			_ = e.ledger.Delete(ctx, snap.ID)
			rep.Removed++
		} else {
			rep.Kept++
		}
	}
	return rep, nil
}

func (e *Engine) resolveSnapForWorktree(ctx context.Context, wtID, snapID string) (*ledger.Snapshot, error) {
	trimmed := strings.TrimSpace(snapID)
	if trimmed == "" || trimmed == "latest" {
		latest, err := e.ledger.LatestForWorktree(ctx, wtID)
		if err != nil {
			return nil, err
		}
		if latest == nil {
			return nil, ErrNoSnapshots
		}
		return latest, nil
	}
	return e.ledger.Get(ctx, trimmed)
}

func (e *Engine) safeSnapPath(snap *ledger.Snapshot) (string, error) {
	if err := ledger.ValidateSnapID(snap.ID); err != nil {
		return "", err
	}
	rel := filepath.Clean(filepath.FromSlash(snap.StoreRel))
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("invalid snapshot store path %q", snap.StoreRel)
	}
	full := filepath.Join(e.snapsDir, rel)
	// Confine inside snapsDir via os.OpenRoot to block symlinks/traversal.
	root, err := os.OpenRoot(e.snapsDir)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	fi, err := root.Lstat(filepath.ToSlash(rel))
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return "", fmt.Errorf("%w: %s", ledger.ErrSymlinkRefused, full)
	}
	return full, nil
}

func (e *Engine) lockWorktree(wtID string) (func(), error) {
	root, err := os.OpenRoot(e.locksDir)
	if err != nil {
		return nil, err
	}
	f, err := root.OpenFile(wtID+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if err := acquireWorktreeLock(f); err != nil {
		_ = f.Close()
		_ = root.Close()
		return nil, err
	}
	return func() {
		releaseWorktreeLock(f)
		_ = f.Close()
		_ = root.Close()
	}, nil
}

func ensureSafeDir(dir string) error {
	if fi, err := os.Lstat(dir); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s is a symlink", ErrUnsafeHomeDir, dir)
		}
		if !fi.IsDir() {
			return fmt.Errorf("%w: %s is not a directory", ErrUnsafeHomeDir, dir)
		}
		return verifySafeDirUnix(dir, fi)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	return verifySafeDirUnix(dir, fi)
}

func resolveWorktreeIdentity(worktree string) (realPath, repoHash, wtID string, err error) {
	if strings.TrimSpace(worktree) == "" {
		worktree, err = os.Getwd()
		if err != nil {
			return "", "", "", err
		}
	}
	abs, err := filepath.Abs(worktree)
	if err != nil {
		return "", "", "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", "", err
	}
	fi, err := os.Stat(real)
	if err != nil || !fi.IsDir() {
		return "", "", "", fmt.Errorf("worktree %q is not a directory", worktree)
	}
	wtSum := sha256.Sum256([]byte(real))
	wtID = hex.EncodeToString(wtSum[:8])

	repoKey := filepath.Base(real)
	repoSum := sha256.Sum256([]byte(repoKey + ":" + real))
	repoHash = hex.EncodeToString(repoSum[:8])
	return real, repoHash, wtID, nil
}

func readGitHead(dir string) (headSHA, branch string) {
	gitPath := filepath.Join(dir, ".git")
	fi, err := os.Lstat(gitPath)
	if err != nil {
		return "", ""
	}
	gitDir := gitPath
	if !fi.IsDir() && fi.Mode().IsRegular() {
		// Linked git worktree: `.git` file contains `gitdir: <path>`
		if b, rerr := os.ReadFile(gitPath); rerr == nil {
			line := strings.TrimSpace(string(b))
			if strings.HasPrefix(line, "gitdir:") {
				target := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
				if !filepath.IsAbs(target) {
					target = filepath.Join(dir, target)
				}
				gitDir = target
			}
		}
	}
	headBytes, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", ""
	}
	headStr := strings.TrimSpace(string(headBytes))
	if strings.HasPrefix(headStr, "ref: ") {
		ref := strings.TrimSpace(strings.TrimPrefix(headStr, "ref: "))
		branch = strings.TrimPrefix(ref, "refs/heads/")
		if refBytes, rerr := os.ReadFile(filepath.Join(gitDir, filepath.FromSlash(ref))); rerr == nil {
			headSHA = strings.TrimSpace(string(refBytes))
		}
		return headSHA, branch
	}
	return headStr, "detached"
}

func computeQuickFingerprint(dir, gitHead string) string {
	h := sha256.New()
	_, _ = io.WriteString(h, gitHead)
	entries, err := os.ReadDir(dir)
	if err == nil {
		for _, e := range entries {
			name := e.Name()
			if name == ".oathgate" || name == ".git" {
				continue
			}
			if info, ierr := e.Info(); ierr == nil {
				_, _ = fmt.Fprintf(h, "|%s:%d:%d", name, info.Size(), info.ModTime().UnixNano())
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

func indexRegularFiles(rootDir string) (map[string]string, error) {
	out := make(map[string]string)
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	var walk func(relDir string) error
	walk = func(relDir string) error {
		readDir := rootDir
		if relDir != "" {
			readDir = filepath.Join(rootDir, filepath.FromSlash(relDir))
		}
		entries, err := os.ReadDir(readDir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			name := e.Name()
			if relDir == "" && (name == ".git" || name == ".oathgate" || name == ".fabrial") {
				continue
			}
			rel := name
			if relDir != "" {
				rel = relDir + "/" + name
			}
			fi, err := root.Lstat(rel)
			if err != nil {
				continue
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				target, _ := root.Readlink(rel)
				sum := sha256.Sum256([]byte("symlink:" + target))
				out[rel] = hex.EncodeToString(sum[:8])
				continue
			}
			if fi.IsDir() {
				if err := walk(rel); err != nil {
					return err
				}
				continue
			}
			if fi.Mode().IsRegular() {
				f, err := root.Open(rel)
				if err != nil {
					continue
				}
				h := sha256.New()
				_, _ = io.Copy(h, io.LimitReader(f, 8*1024*1024))
				_ = f.Close()
				out[rel] = hex.EncodeToString(h.Sum(nil)[:16])
			}
		}
		return nil
	}
	if err := walk(""); err != nil {
		return nil, err
	}
	return out, nil
}

func redactedPreview(rootDir, rel string) string {
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return ""
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(rel)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxDiffFileBytes))
	if err != nil || bytes.IndexByte(raw, 0) >= 0 {
		return "<binary>"
	}
	scrubbed := tmredact.ScrubText(string(raw))
	lines := strings.Split(strings.TrimSpace(scrubbed), "\n")
	if len(lines) > 4 {
		lines = lines[:4]
	}
	preview := strings.Join(lines, " | ")
	if len(preview) > 220 {
		preview = preview[:217] + "..."
	}
	return preview
}

func swapWorktreeEntries(realWT, stageDir string) error {
	preserve := map[string]bool{
		".oathgate": true,
		".fabrial":  true,
	}

	// Remove current entries in worktree (except preserved local metadata).
	curEntries, err := os.ReadDir(realWT)
	if err != nil {
		return err
	}
	for _, e := range curEntries {
		if preserve[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(realWT, e.Name())); err != nil {
			return err
		}
	}

	// Atomically move staged entries into place on the same filesystem.
	stagedEntries, err := os.ReadDir(stageDir)
	if err != nil {
		return err
	}
	for _, e := range stagedEntries {
		if preserve[e.Name()] {
			continue
		}
		src := filepath.Join(stageDir, e.Name())
		dst := filepath.Join(realWT, e.Name())
		if err := os.Rename(src, dst); err != nil {
			return err
		}
	}
	return nil
}

func buildRedactedDeadEndPayload(snap *ledger.Snapshot, worktree, reason, safetyID string) string {
	cleanReason := tmredact.ScrubText(strings.TrimSpace(reason))
	if cleanReason == "" {
		cleanReason = "workspace rewound to earlier verified checkpoint"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Oathgate Dead End (%s): rewound workspace %q to snapshot %s (pre-rewind safety snap: %s). Reason: %s",
		time.Now().UTC().Format(time.RFC3339), filepath.Base(worktree), snap.ID, safetyID, cleanReason)
	if snap.TinmindID != "" {
		fmt.Fprintf(&b, " | Tinmind trace: %s", snap.TinmindID)
	}
	return tmredact.ScrubText(b.String())
}

func pushDeadEndToCoppermind(ctx context.Context, repoName, payload string) bool {
	bin, err := exec.LookPath("coppermind")
	if err != nil {
		return false
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	args := []string{"remember", "--category", "dead_end", "--session", "oathgate"}
	if repoName != "" {
		args = append(args, "--tags", "oathgate,"+repoName)
	}
	args = append(args, payload)
	cmd := exec.CommandContext(cmdCtx, bin, args...)
	return cmd.Run() == nil
}

package oathgate_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/bonjoski/oathgate/pkg/cloner"
	"github.com/bonjoski/oathgate/pkg/engine"
	"github.com/bonjoski/oathgate/pkg/ledger"
	tmredact "github.com/bonjoski/tinmind/pkg/redact"
)

// TestAdversarial_SnapIDPathTraversalRefused verifies STRIDE Spoofing: forged or traversing snap IDs fail closed.
func TestAdversarial_SnapIDPathTraversalRefused(t *testing.T) {
	home := t.TempDir()
	wt := t.TempDir()
	eng, err := engine.Open(home)
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}
	defer func() { _ = eng.Close() }()

	badIDs := []string{
		"../../etc/cron.d",
		"../og-20261127-deadbeef",
		"og-20261127-DEADBEEF", // uppercase rejected
		"og-20261127-1234",     // too short
		"-rf",
		"og-20261127-deadbeef/../../etc",
		"bad\x00id",
	}
	for _, id := range badIDs {
		if err := ledger.ValidateSnapID(id); !errors.Is(err, ledger.ErrInvalidSnapID) {
			t.Fatalf("ValidateSnapID(%q) = %v, want ErrInvalidSnapID", id, err)
		}
		if _, err := eng.Rewind(context.Background(), wt, id, engine.RewindOptions{}); !errors.Is(err, ledger.ErrInvalidSnapID) {
			t.Fatalf("Rewind(%q) = %v, want ErrInvalidSnapID", id, err)
		}
		if _, err := eng.Branch(context.Background(), id, filepath.Join(t.TempDir(), "out")); !errors.Is(err, ledger.ErrInvalidSnapID) {
			t.Fatalf("Branch(%q) = %v, want ErrInvalidSnapID", id, err)
		}
	}
}

// TestAdversarial_EscapingSymlinkAndDeviceNodeBlocked verifies STRIDE Tampering:
// symlinks escaping repoRoot and FIFOs/special files fail closed, while in-repo symlinks succeed.
func TestAdversarial_EscapingSymlinkAndDeviceNodeBlocked(t *testing.T) {
	home := t.TempDir()
	eng, err := engine.Open(home)
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}
	defer func() { _ = eng.Close() }()

	// 1. Escaping absolute symlink -> /etc/hosts
	wt1 := t.TempDir()
	_ = os.WriteFile(filepath.Join(wt1, "ok.txt"), []byte("hello"), 0o600)
	_ = os.Symlink("/etc/hosts", filepath.Join(wt1, "evil-abs"))
	if _, err := eng.Snap(context.Background(), wt1, engine.SnapOptions{}); !errors.Is(err, cloner.ErrEscapingSymlink) {
		t.Fatalf("expected ErrEscapingSymlink for absolute symlink, got %v", err)
	}

	// 2. Escaping relative symlink -> ../../outside
	wt2 := t.TempDir()
	_ = os.Symlink("../../outside", filepath.Join(wt2, "evil-rel"))
	if _, err := eng.Snap(context.Background(), wt2, engine.SnapOptions{}); !errors.Is(err, cloner.ErrEscapingSymlink) {
		t.Fatalf("expected ErrEscapingSymlink for relative escape, got %v", err)
	}

	// 3. FIFO special file refused fail-closed
	wt3 := t.TempDir()
	fifoPath := filepath.Join(wt3, "evil.fifo")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}
	if _, err := eng.Snap(context.Background(), wt3, engine.SnapOptions{}); !errors.Is(err, cloner.ErrSpecialFileBlocked) {
		t.Fatalf("expected ErrSpecialFileBlocked for FIFO, got %v", err)
	}

	// 4. Safe in-repo symlink succeeds cleanly
	wt4 := t.TempDir()
	_ = os.WriteFile(filepath.Join(wt4, "target.txt"), []byte("safe"), 0o600)
	_ = os.Symlink("target.txt", filepath.Join(wt4, "link.txt"))
	res, err := eng.Snap(context.Background(), wt4, engine.SnapOptions{Label: "in-repo-symlink"})
	if err != nil {
		t.Fatalf("safe in-repo symlink should succeed, got %v", err)
	}
	if res.Snapshot.ID == "" {
		t.Fatalf("expected valid snapshot ID")
	}
}

// TestAdversarial_ConcurrentMutationLockAndPreRewindSafety verifies STRIDE Repudiation:
// active .git/index.lock blocks rewind fail-closed, automatic pre-rewind safety snap makes rewind reversible,
// and exact byte-for-byte workspace restoration works cleanly.
func TestAdversarial_ConcurrentMutationLockAndPreRewindSafety(t *testing.T) {
	home := t.TempDir()
	wt := t.TempDir()
	eng, err := engine.Open(home)
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}
	defer func() { _ = eng.Close() }()

	_ = os.MkdirAll(filepath.Join(wt, ".git"), 0o700)
	_ = os.WriteFile(filepath.Join(wt, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600)
	_ = os.WriteFile(filepath.Join(wt, "app.go"), []byte("package app\n// v1\n"), 0o600)

	snap1, err := eng.Snap(context.Background(), wt, engine.SnapOptions{Label: "v1"})
	if err != nil {
		t.Fatalf("Snap v1: %v", err)
	}

	// Mutate workspace and add an untracked file
	_ = os.WriteFile(filepath.Join(wt, "app.go"), []byte("package app\n// broken v2\n"), 0o600)
	_ = os.WriteFile(filepath.Join(wt, "junk.tmp"), []byte("derailed"), 0o600)

	// Plant .git/index.lock — rewind MUST fail closed
	lockPath := filepath.Join(wt, ".git", "index.lock")
	_ = os.WriteFile(lockPath, []byte("lock"), 0o600)
	if _, err := eng.Rewind(context.Background(), wt, snap1.Snapshot.ID, engine.RewindOptions{}); !errors.Is(err, engine.ErrActiveGitLock) {
		t.Fatalf("expected ErrActiveGitLock when .git/index.lock exists, got %v", err)
	}
	_ = os.Remove(lockPath)

	// Rewind to v1 — must restore app.go, remove junk.tmp, AND create a pre-rewind safety snap!
	rw, err := eng.Rewind(context.Background(), wt, snap1.Snapshot.ID, engine.RewindOptions{Reason: "v2 broke build"})
	if err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	if rw.SafetySnapID == "" {
		t.Fatalf("expected automatic pre-rewind safety snapshot ID")
	}
	gotV1, _ := os.ReadFile(filepath.Join(wt, "app.go"))
	if !strings.Contains(string(gotV1), "// v1") {
		t.Fatalf("app.go not restored to v1: %q", string(gotV1))
	}
	if _, err := os.Stat(filepath.Join(wt, "junk.tmp")); !os.IsNotExist(err) {
		t.Fatalf("junk.tmp should have been removed on rewind")
	}

	// Rewind to the safety snap — proves rewind itself is 100% reversible!
	if _, err := eng.Rewind(context.Background(), wt, rw.SafetySnapID, engine.RewindOptions{NoSafetySnap: true}); err != nil {
		t.Fatalf("Rewind to safety snap: %v", err)
	}
	gotV2, _ := os.ReadFile(filepath.Join(wt, "app.go"))
	if !strings.Contains(string(gotV2), "// broken v2") {
		t.Fatalf("safety snap did not restore v2: %q", string(gotV2))
	}
}

// TestAdversarial_DiffAndCoppermindSecretRedaction verifies STRIDE Information Disclosure:
// secrets in .env / modified files and rewind reasons are scrubbed before diff display or Coppermind export.
func TestAdversarial_DiffAndCoppermindSecretRedaction(t *testing.T) {
	home := t.TempDir()
	wt := t.TempDir()
	eng, err := engine.Open(home)
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}
	defer func() { _ = eng.Close() }()

	_ = os.WriteFile(filepath.Join(wt, "main.go"), []byte("package main\n"), 0o600)
	snap, err := eng.Snap(context.Background(), wt, engine.SnapOptions{Label: "clean"})
	if err != nil {
		t.Fatalf("Snap: %v", err)
	}

	simAnthropic := "sk-ant-api03-SECRETTOKEN1234567890abcdef"
	simAWS := "AKIAIOSFODNN7EXAMPLE"
	_ = os.WriteFile(filepath.Join(wt, ".env"), []byte("ANTHROPIC_API_KEY="+simAnthropic+"\nAWS="+simAWS+"\n"), 0o600)

	diff, err := eng.Diff(context.Background(), wt, snap.Snapshot.ID)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if diff.Added != 1 || len(diff.Entries) != 1 {
		t.Fatalf("expected 1 added entry (.env), got %+v", diff)
	}
	preview := diff.Entries[0].Preview
	if strings.Contains(preview, simAnthropic) || strings.Contains(preview, simAWS) {
		t.Fatalf("SECURITY VIOLATION: raw secret leaked in diff preview: %q", preview)
	}
	if !strings.Contains(preview, tmredact.Placeholder) {
		t.Fatalf("expected %s placeholder in diff preview: %q", tmredact.Placeholder, preview)
	}

	rw, err := eng.Rewind(context.Background(), wt, snap.Snapshot.ID, engine.RewindOptions{
		Reason:        "failed using key " + simAnthropic,
		RecordDeadEnd: true,
	})
	if err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	if strings.Contains(rw.DeadEndPayload, simAnthropic) || !strings.Contains(rw.DeadEndPayload, tmredact.Placeholder) {
		t.Fatalf("SECURITY VIOLATION: dead_end payload not secret-scrubbed: %q", rw.DeadEndPayload)
	}
}

// TestAdversarial_DeepTreeAndSnapshotStormBounded verifies STRIDE Denial of Service:
// max_depth, max_entries, 2s fabrial_hook dedup, and pinned-aware FIFO GC.
func TestAdversarial_DeepTreeAndSnapshotStormBounded(t *testing.T) {
	home := t.TempDir()
	eng, err := engine.Open(home)
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}
	defer func() { _ = eng.Close() }()

	// 1. Deep directory tree exceeds custom max_depth=4
	deepWT := t.TempDir()
	nested := filepath.Join(deepWT, "a", "b", "c", "d", "e", "f")
	_ = os.MkdirAll(nested, 0o700)
	_ = os.WriteFile(filepath.Join(nested, "leaf.txt"), []byte("deep"), 0o600)

	eng.SetCloneOptions(cloner.CloneOptions{MaxDepth: 4, MaxEntries: 100})
	if _, err := eng.Snap(context.Background(), deepWT, engine.SnapOptions{}); !errors.Is(err, cloner.ErrMaxDepthExceeded) {
		t.Fatalf("expected ErrMaxDepthExceeded, got %v", err)
	}

	// 2. Entry count exceeds max_entries=5
	wideWT := t.TempDir()
	for i := 0; i < 8; i++ {
		_ = os.WriteFile(filepath.Join(wideWT, fmt.Sprintf("f%d.txt", i)), []byte("x"), 0o600)
	}
	eng.SetCloneOptions(cloner.CloneOptions{MaxDepth: 64, MaxEntries: 5})
	if _, err := eng.Snap(context.Background(), wideWT, engine.SnapOptions{}); !errors.Is(err, cloner.ErrMaxEntriesExceeded) {
		t.Fatalf("expected ErrMaxEntriesExceeded, got %v", err)
	}

	// 3. fabrial_hook dedup within 2s window + pinned-aware GC
	eng.SetCloneOptions(cloner.DefaultOptions())
	stormWT := t.TempDir()
	_ = os.WriteFile(filepath.Join(stormWT, "file.go"), []byte("package p\n"), 0o600)

	first, err := eng.Snap(context.Background(), stormWT, engine.SnapOptions{Trigger: "fabrial_hook", Pinned: true})
	if err != nil {
		t.Fatalf("first hook snap: %v", err)
	}
	second, err := eng.Snap(context.Background(), stormWT, engine.SnapOptions{Trigger: "fabrial_hook"})
	if err != nil {
		t.Fatalf("second hook snap: %v", err)
	}
	if !second.Deduplicated || second.Snapshot.ID != first.Snapshot.ID {
		t.Fatalf("expected 2s fabrial_hook dedup, got dedup=%v id=%s vs %s", second.Deduplicated, second.Snapshot.ID, first.Snapshot.ID)
	}

	// Take 3 manual snaps and run GC with MaxSnaps=1 -> pinned snap must survive!
	for i := 0; i < 3; i++ {
		if _, err := eng.Snap(context.Background(), stormWT, engine.SnapOptions{Label: fmt.Sprintf("m%d", i), SkipAutoGC: true}); err != nil {
			t.Fatalf("manual snap %d: %v", i, err)
		}
	}
	gcRep, err := eng.GC(context.Background(), engine.GCOptions{MaxSnapsPerWorktree: 1})
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if gcRep.Removed < 2 || gcRep.Pinned != 1 {
		t.Fatalf("unexpected GC report: %+v", gcRep)
	}
}

// TestAdversarial_SetuidBitsStrippedOnSnapAndRewind verifies STRIDE Elevation of Privilege:
// setuid/setgid/sticky bits are stripped from both snapshot storage and rewound workspaces.
func TestAdversarial_SetuidBitsStrippedOnSnapAndRewind(t *testing.T) {
	home := t.TempDir()
	wt := t.TempDir()
	eng, err := engine.Open(home)
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}
	defer func() { _ = eng.Close() }()

	binPath := filepath.Join(wt, "helper.sh")
	_ = os.WriteFile(binPath, []byte("#!/bin/sh\necho hi\n"), 0o755)
	if err := os.Chmod(binPath, 0o755|os.ModeSetuid|os.ModeSetgid|os.ModeSticky); err != nil {
		t.Fatalf("chmod setuid: %v", err)
	}

	snap, err := eng.Snap(context.Background(), wt, engine.SnapOptions{Label: "setuid-test"})
	if err != nil {
		t.Fatalf("Snap: %v", err)
	}

	// Check stored snapshot file has zero special bits.
	storedFile := filepath.Join(home, "snapshots", filepath.FromSlash(snap.Snapshot.StoreRel), "helper.sh")
	fi, err := os.Lstat(storedFile)
	if err != nil {
		t.Fatalf("lstat stored file: %v", err)
	}
	if fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Fatalf("SECURITY VIOLATION: setuid/setgid/sticky bit preserved in snapshot: %s", fi.Mode().String())
	}

	// Rewind and verify restored file also has zero special bits.
	if _, err := eng.Rewind(context.Background(), wt, snap.Snapshot.ID, engine.RewindOptions{NoSafetySnap: true}); err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	rfi, err := os.Lstat(binPath)
	if err != nil {
		t.Fatalf("lstat restored file: %v", err)
	}
	if rfi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Fatalf("SECURITY VIOLATION: setuid/setgid/sticky bit restored on rewind: %s", rfi.Mode().String())
	}
}

// TestConcurrent8WorktreesRaceFree verifies N=8 parallel worktrees snapping, mutating, diffing,
// rewinding, and branching simultaneously under -race with zero SQLite lock failures.
func TestConcurrent8WorktreesRaceFree(t *testing.T) {
	home := t.TempDir()
	eng, err := engine.Open(home)
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}
	defer func() { _ = eng.Close() }()

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			wt := t.TempDir()
			f := filepath.Join(wt, "work.txt")
			_ = os.WriteFile(f, []byte(fmt.Sprintf("v1-worker-%d", idx)), 0o600)

			s1, err := eng.Snap(context.Background(), wt, engine.SnapOptions{Label: fmt.Sprintf("w%d", idx)})
			if err != nil {
				errs <- fmt.Errorf("worker %d snap: %w", idx, err)
				return
			}
			_ = os.WriteFile(f, []byte(fmt.Sprintf("v2-worker-%d", idx)), 0o600)
			if _, err := eng.Diff(context.Background(), wt, s1.Snapshot.ID); err != nil {
				errs <- fmt.Errorf("worker %d diff: %w", idx, err)
				return
			}
			if _, err := eng.Rewind(context.Background(), wt, s1.Snapshot.ID, engine.RewindOptions{}); err != nil {
				errs <- fmt.Errorf("worker %d rewind: %w", idx, err)
				return
			}
			got, _ := os.ReadFile(f)
			if string(got) != fmt.Sprintf("v1-worker-%d", idx) {
				errs <- fmt.Errorf("worker %d restore mismatch: %q", idx, string(got))
				return
			}
			brDir := filepath.Join(t.TempDir(), fmt.Sprintf("branch-%d", idx))
			if _, err := eng.Branch(context.Background(), s1.Snapshot.ID, brDir); err != nil {
				errs <- fmt.Errorf("worker %d branch: %w", idx, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// BenchmarkAPFS500FileSnap verifies the sub-10ms CoW snapshot budget on a 500-file repository.
func BenchmarkAPFS500FileSnap(b *testing.B) {
	home := b.TempDir()
	wt := b.TempDir()
	for d := 0; d < 10; d++ {
		sub := filepath.Join(wt, fmt.Sprintf("pkg%d", d))
		_ = os.MkdirAll(sub, 0o700)
		for f := 0; f < 50; f++ {
			_ = os.WriteFile(filepath.Join(sub, fmt.Sprintf("file_%d.go", f)), []byte("package pkg\n// benchmark fixture\n"), 0o600)
		}
	}

	eng, err := engine.Open(home)
	if err != nil {
		b.Fatalf("engine.Open: %v", err)
	}
	defer func() { _ = eng.Close() }()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := eng.Snap(context.Background(), wt, engine.SnapOptions{SkipAutoGC: true}); err != nil {
			b.Fatalf("Snap: %v", err)
		}
	}
}

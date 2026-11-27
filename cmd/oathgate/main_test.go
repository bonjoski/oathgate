package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bonjoski/oathgate/pkg/engine"
)

func TestCLI_FullLifecycleWithTinmindBookmark(t *testing.T) {
	home := t.TempDir()
	wt := t.TempDir()
	_ = os.WriteFile(filepath.Join(wt, "hello.txt"), []byte("version-1\n"), 0o600)

	t.Setenv("TINMIND_SESSION_ID", "tm-20261127-a1b2c3d4")

	var stdout, stderr bytes.Buffer

	// 1. Snap with JSON output & auto-captured TINMIND_SESSION_ID.
	code := run([]string{"snap", "-m", "baseline", "--dir", wt, "--home", home, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("snap exit=%d err=%q", code, stderr.String())
	}
	var snapRes engine.SnapResult
	if err := json.Unmarshal(stdout.Bytes(), &snapRes); err != nil {
		t.Fatalf("unmarshal snap json: %v (%s)", err, stdout.String())
	}
	if snapRes.Snapshot.TinmindID != "tm-20261127-a1b2c3d4" {
		t.Fatalf("expected auto-captured tinmind bookmark, got %q", snapRes.Snapshot.TinmindID)
	}

	// 2. List.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"list", "--dir", wt, "--home", home}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), snapRes.Snapshot.ID) {
		t.Fatalf("list exit=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}

	// 3. Mutate & Diff.
	_ = os.WriteFile(filepath.Join(wt, "hello.txt"), []byte("version-2\n"), 0o600)
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"diff", "latest", "--dir", wt, "--home", home}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "~1 modified") {
		t.Fatalf("diff exit=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}

	// 4. Branch into parallel directory.
	brDir := filepath.Join(t.TempDir(), "try-b")
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"branch", snapRes.Snapshot.ID, brDir, "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("branch exit=%d err=%q", code, stderr.String())
	}
	brBody, _ := os.ReadFile(filepath.Join(brDir, "hello.txt"))
	if string(brBody) != "version-1\n" {
		t.Fatalf("branch content = %q, want version-1", string(brBody))
	}

	// 5. Rewind latest.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"rewind", "latest", "--dir", wt, "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("rewind exit=%d err=%q", code, stderr.String())
	}
	restored, _ := os.ReadFile(filepath.Join(wt, "hello.txt"))
	if string(restored) != "version-1\n" {
		t.Fatalf("rewound content = %q, want version-1", string(restored))
	}

	// 6. Pin, GC, Doctor, Version.
	for _, cmd := range [][]string{
		{"pin", snapRes.Snapshot.ID, "--home", home},
		{"gc", "--home", home, "--json"},
		{"doctor", "--home", home, "--json"},
		{"version"},
	} {
		stdout.Reset()
		stderr.Reset()
		if code := run(cmd, &stdout, &stderr); code != 0 {
			t.Fatalf("cmd %v exit=%d err=%q", cmd, code, stderr.String())
		}
	}
}

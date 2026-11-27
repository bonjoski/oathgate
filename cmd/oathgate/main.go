package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/bonjoski/oathgate/pkg/cloner"
	"github.com/bonjoski/oathgate/pkg/engine"
	"github.com/bonjoski/oathgate/pkg/ledger"
	"github.com/bonjoski/oathgate/pkg/version"
	"github.com/bonjoski/tinmind/pkg/ux"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printHelp(stdout)
		return ux.ExitOK
	}

	sub := args[0]
	rest := args[1:]
	switch sub {
	case "-h", "--help", "help":
		printHelp(stdout)
		return ux.ExitOK
	case "-v", "--version", "version":
		return cmdVersion(rest, stdout)
	case "snap", "checkpoint":
		return cmdSnap(rest, stdout, stderr)
	case "list", "ls", "log":
		return cmdList(rest, stdout, stderr)
	case "diff":
		return cmdDiff(rest, stdout, stderr)
	case "rewind", "restore":
		return cmdRewind(rest, stdout, stderr)
	case "branch", "fork":
		return cmdBranch(rest, stdout, stderr)
	case "pin":
		return cmdPin(rest, stdout, stderr)
	case "gc", "prune":
		return cmdGC(rest, stdout, stderr)
	case "doctor":
		return cmdDoctor(rest, stdout, stderr)
	default:
		return renderFF(stderr, &ux.FixForwardError{
			Tool:           "oathgate",
			Code:           "OATHGATE_UNKNOWN_COMMAND",
			Summary:        fmt.Sprintf("unknown command %q", sub),
			RemediationCmd: "oathgate --help",
			WhyOneLiner:    "Supported commands: snap, list, diff, rewind, branch, pin, gc, doctor, version.",
			ExitCode:       ux.ExitUsageErr,
		}, hasFlag(rest, "--json"), hasFlag(rest, "--why"))
	}
}

func printHelp(w io.Writer) {
	_, _ = fmt.Fprintf(w, `oathgate v%s — Instant Copy-on-Write time-travel checkpoints for coding agents

Usage:
  oathgate snap [-m <label>] [--pin] [--trace-id <id>]  Take an O(1) CoW snapshot of the current worktree
  oathgate list [--limit 25] [--json]                   List checkpoints for this worktree
  oathgate diff [<snap-id|latest>] [--json]             Show secret-redacted diff vs a checkpoint
  oathgate rewind [<snap-id|latest>] [--record-dead-end] Atomically rewind (auto-takes safety snap first)
  oathgate branch <snap-id> <target-dir>                Materialize a checkpoint into a parallel directory
  oathgate pin <snap-id> [--unpin]                      Exempt/unexempt a snapshot from FIFO GC
  oathgate gc [--max-snaps 25] [--max-hours 72]         Prune old unpinned checkpoints
  oathgate doctor [--json]                              Verify CoW backend, store permissions, and WAL DB
  oathgate version [--json]                             Print version

`, version.Version)
}

func cmdVersion(args []string, stdout io.Writer) int {
	if hasFlag(args, "--json") {
		_ = json.NewEncoder(stdout).Encode(map[string]string{
			"tool":    "oathgate",
			"version": version.Version,
		})
		return ux.ExitOK
	}
	_, _ = fmt.Fprintf(stdout, "oathgate v%s\n", version.Version)
	return ux.ExitOK
}

func cmdSnap(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("snap", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	label := fs.String("m", "", "snapshot label / message")
	pin := fs.Bool("pin", false, "pin snapshot to exempt from automatic GC")
	trigger := fs.String("trigger", "manual", "trigger source: manual or fabrial_hook")
	traceID := fs.String("trace-id", "", "optional Tinmind trace ID bookmark (auto-reads TINMIND_SESSION_ID)")
	worktree := fs.String("dir", "", "target worktree directory (default cwd)")
	home := fs.String("home", "", "override oathgate store directory")
	asJSON := fs.Bool("json", false, "output JSON")
	showWhy := fs.Bool("why", false, "show deep explanation on error")

	if err := fs.Parse(args); err != nil {
		return renderUsage(stderr, "OATHGATE_SNAP_FLAGS", err.Error(), "oathgate snap -m \"before refactor\"", *asJSON, *showWhy)
	}

	eng, err := engine.Open(*home)
	if err != nil {
		return renderErr(stderr, err, *asJSON, *showWhy)
	}
	defer func() { _ = eng.Close() }()

	res, err := eng.Snap(context.Background(), *worktree, engine.SnapOptions{
		Label:     *label,
		Trigger:   *trigger,
		Pinned:    *pin,
		TinmindID: *traceID,
	})
	if err != nil {
		return renderErr(stderr, err, *asJSON, *showWhy)
	}

	if *asJSON {
		_ = json.NewEncoder(stdout).Encode(res)
		return ux.ExitOK
	}
	s := res.Snapshot
	tag := ""
	if res.Deduplicated {
		tag = " (deduplicated)"
	}
	_, _ = fmt.Fprintf(stdout, "✓ oathgate: snapshot %s%s (%s, %dms, %d entries)\n", s.ID, tag, s.CoWBackend, s.DurationMs, s.EntriesCloned)
	if s.TinmindID != "" {
		_, _ = fmt.Fprintf(stdout, "  Trace:  tinmind replay %s\n", s.TinmindID)
	}
	return ux.ExitOK
}

func cmdList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	limit := fs.Int("limit", 25, "maximum snapshots to display")
	worktree := fs.String("dir", "", "target worktree directory")
	home := fs.String("home", "", "override oathgate store directory")
	asJSON := fs.Bool("json", false, "output JSON")
	showWhy := fs.Bool("why", false, "show deep explanation on error")

	if err := fs.Parse(args); err != nil {
		return renderUsage(stderr, "OATHGATE_LIST_FLAGS", err.Error(), "oathgate list", *asJSON, *showWhy)
	}

	eng, err := engine.Open(*home)
	if err != nil {
		return renderErr(stderr, err, *asJSON, *showWhy)
	}
	defer func() { _ = eng.Close() }()

	snaps, err := eng.List(context.Background(), *worktree, *limit)
	if err != nil {
		return renderErr(stderr, err, *asJSON, *showWhy)
	}
	if *asJSON {
		if snaps == nil {
			snaps = []ledger.Snapshot{}
		}
		_ = json.NewEncoder(stdout).Encode(snaps)
		return ux.ExitOK
	}
	if len(snaps) == 0 {
		_, _ = fmt.Fprintln(stdout, "No checkpoints recorded for this worktree. Run `oathgate snap -m \"baseline\"` to create one.")
		return ux.ExitOK
	}
	for _, s := range snaps {
		pinTag := " "
		if s.Pinned {
			pinTag = "*"
		}
		_, _ = fmt.Fprintf(stdout, "%s %-20s  %-14s  %4dms  %-11s  %s  %s\n",
			pinTag, s.ID, s.CoWBackend, s.DurationMs, s.Trigger, s.CreatedAt.Format(time.RFC3339), ux.SanitizeSingleLine(s.Label))
	}
	return ux.ExitOK
}

func cmdDiff(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	worktree := fs.String("dir", "", "target worktree directory")
	home := fs.String("home", "", "override oathgate store directory")
	asJSON := fs.Bool("json", false, "output JSON")
	showWhy := fs.Bool("why", false, "show deep explanation on error")

	snapArg, flagArgs := extractPositional(args)
	if err := fs.Parse(flagArgs); err != nil {
		return renderUsage(stderr, "OATHGATE_DIFF_FLAGS", err.Error(), "oathgate diff latest", *asJSON, *showWhy)
	}
	if snapArg == "" && fs.NArg() > 0 {
		snapArg = fs.Arg(0)
	}

	eng, err := engine.Open(*home)
	if err != nil {
		return renderErr(stderr, err, *asJSON, *showWhy)
	}
	defer func() { _ = eng.Close() }()

	diff, err := eng.Diff(context.Background(), *worktree, snapArg)
	if err != nil {
		return renderErr(stderr, err, *asJSON, *showWhy)
	}
	if *asJSON {
		_ = json.NewEncoder(stdout).Encode(diff)
		return ux.ExitOK
	}
	_, _ = fmt.Fprintf(stdout, "Diff vs %s: +%d added, ~%d modified, -%d deleted\n", diff.SnapID, diff.Added, diff.Modified, diff.Deleted)
	for _, e := range diff.Entries {
		sym := "~"
		switch e.Status {
		case "added":
			sym = "+"
		case "deleted":
			sym = "-"
		}
		_, _ = fmt.Fprintf(stdout, "  %s %s\n", sym, ux.SanitizeSingleLine(e.Path))
		if e.Preview != "" {
			_, _ = fmt.Fprintf(stdout, "      %s\n", ux.SanitizeSingleLine(e.Preview))
		}
	}
	return ux.ExitOK
}

func cmdRewind(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("rewind", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	reason := fs.String("reason", "", "why this path is being rewound")
	recordDeadEnd := fs.Bool("record-dead-end", false, "record sanitized dead_end lesson in coppermind")
	noSafety := fs.Bool("no-safety-snap", false, "skip automatic pre-rewind safety snapshot")
	worktree := fs.String("dir", "", "target worktree directory")
	home := fs.String("home", "", "override oathgate store directory")
	asJSON := fs.Bool("json", false, "output JSON")
	showWhy := fs.Bool("why", false, "show deep explanation on error")

	snapArg, flagArgs := extractPositional(args)
	if err := fs.Parse(flagArgs); err != nil {
		return renderUsage(stderr, "OATHGATE_REWIND_FLAGS", err.Error(), "oathgate rewind latest", *asJSON, *showWhy)
	}
	if snapArg == "" && fs.NArg() > 0 {
		snapArg = fs.Arg(0)
	}

	eng, err := engine.Open(*home)
	if err != nil {
		return renderErr(stderr, err, *asJSON, *showWhy)
	}
	defer func() { _ = eng.Close() }()

	res, err := eng.Rewind(context.Background(), *worktree, snapArg, engine.RewindOptions{
		Reason:        *reason,
		RecordDeadEnd: *recordDeadEnd,
		NoSafetySnap:  *noSafety,
	})
	if err != nil {
		return renderErr(stderr, err, *asJSON, *showWhy)
	}
	if *asJSON {
		_ = json.NewEncoder(stdout).Encode(res)
		return ux.ExitOK
	}
	_, _ = fmt.Fprintf(stdout, "✓ oathgate: rewound worktree to %s (%s)\n", res.RestoredSnapID, res.Backend)
	if res.SafetySnapID != "" {
		_, _ = fmt.Fprintf(stdout, "  Undo:   oathgate rewind %s\n", res.SafetySnapID)
	}
	if res.DeadEndRecorded {
		_, _ = fmt.Fprintln(stdout, "  Memory: recorded sanitized dead_end in coppermind")
	}
	return ux.ExitOK
}

func cmdBranch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("branch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	home := fs.String("home", "", "override oathgate store directory")
	asJSON := fs.Bool("json", false, "output JSON")
	showWhy := fs.Bool("why", false, "show deep explanation on error")

	var pos []string
	var flagArgs []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			flagArgs = append(flagArgs, args[i])
			if (args[i] == "--home" || args[i] == "-home") && i+1 < len(args) {
				i++
				flagArgs = append(flagArgs, args[i])
			}
		} else {
			pos = append(pos, args[i])
		}
	}
	if err := fs.Parse(flagArgs); err != nil {
		return renderUsage(stderr, "OATHGATE_BRANCH_FLAGS", err.Error(), "oathgate branch <snap-id> ../repo-try-b", *asJSON, *showWhy)
	}
	pos = append(pos, fs.Args()...)
	if len(pos) < 2 {
		return renderUsage(stderr, "OATHGATE_BRANCH_ARGS", "snapshot ID and target directory are required", "oathgate branch <snap-id> ../repo-try-b", *asJSON, *showWhy)
	}

	eng, err := engine.Open(*home)
	if err != nil {
		return renderErr(stderr, err, *asJSON, *showWhy)
	}
	defer func() { _ = eng.Close() }()

	snap, err := eng.Branch(context.Background(), pos[0], pos[1])
	if err != nil {
		return renderErr(stderr, err, *asJSON, *showWhy)
	}
	if *asJSON {
		_ = json.NewEncoder(stdout).Encode(map[string]any{
			"snapshot_id": snap.ID,
			"target_dir":  pos[1],
		})
		return ux.ExitOK
	}
	_, _ = fmt.Fprintf(stdout, "✓ oathgate: branched %s -> %s\n", snap.ID, pos[1])
	return ux.ExitOK
}

func cmdPin(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pin", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	unpin := fs.Bool("unpin", false, "remove GC pin")
	home := fs.String("home", "", "override oathgate store directory")
	asJSON := fs.Bool("json", false, "output JSON")
	showWhy := fs.Bool("why", false, "show deep explanation on error")

	snapArg, flagArgs := extractPositional(args)
	if err := fs.Parse(flagArgs); err != nil {
		return renderUsage(stderr, "OATHGATE_PIN_FLAGS", err.Error(), "oathgate pin <snap-id>", *asJSON, *showWhy)
	}
	if snapArg == "" && fs.NArg() > 0 {
		snapArg = fs.Arg(0)
	}
	if snapArg == "" {
		return renderUsage(stderr, "OATHGATE_PIN_ARGS", "snapshot ID is required", "oathgate list", *asJSON, *showWhy)
	}

	eng, err := engine.Open(*home)
	if err != nil {
		return renderErr(stderr, err, *asJSON, *showWhy)
	}
	defer func() { _ = eng.Close() }()

	if err := eng.Pin(context.Background(), snapArg, !*unpin); err != nil {
		return renderErr(stderr, err, *asJSON, *showWhy)
	}
	if *asJSON {
		_ = json.NewEncoder(stdout).Encode(map[string]any{"snap_id": snapArg, "pinned": !*unpin})
		return ux.ExitOK
	}
	action := "pinned"
	if *unpin {
		action = "unpinned"
	}
	_, _ = fmt.Fprintf(stdout, "✓ oathgate: %s %s\n", action, snapArg)
	return ux.ExitOK
}

func cmdGC(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	maxSnaps := fs.Int("max-snaps", engine.DefaultMaxSnapsPerWorktree, "max unpinned snapshots per worktree")
	maxHours := fs.Int("max-hours", 72, "max unpinned snapshot age in hours")
	home := fs.String("home", "", "override oathgate store directory")
	asJSON := fs.Bool("json", false, "output JSON")
	showWhy := fs.Bool("why", false, "show deep explanation on error")

	if err := fs.Parse(args); err != nil {
		return renderUsage(stderr, "OATHGATE_GC_FLAGS", err.Error(), "oathgate gc --max-snaps 25 --max-hours 72", *asJSON, *showWhy)
	}
	eng, err := engine.Open(*home)
	if err != nil {
		return renderErr(stderr, err, *asJSON, *showWhy)
	}
	defer func() { _ = eng.Close() }()

	rep, err := eng.GC(context.Background(), engine.GCOptions{
		MaxSnapsPerWorktree: *maxSnaps,
		MaxAge:              time.Duration(*maxHours) * time.Hour,
	})
	if err != nil {
		return renderErr(stderr, err, *asJSON, *showWhy)
	}
	if *asJSON {
		_ = json.NewEncoder(stdout).Encode(rep)
		return ux.ExitOK
	}
	_, _ = fmt.Fprintf(stdout, "✓ oathgate gc: removed %d, kept %d (%d pinned)\n", rep.Removed, rep.Kept, rep.Pinned)
	return ux.ExitOK
}

func cmdDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	home := fs.String("home", "", "override oathgate store directory")
	asJSON := fs.Bool("json", false, "output JSON")
	showWhy := fs.Bool("why", false, "show deep explanation on error")

	if err := fs.Parse(args); err != nil {
		return renderUsage(stderr, "OATHGATE_DOCTOR_FLAGS", err.Error(), "oathgate doctor", *asJSON, *showWhy)
	}
	eng, err := engine.Open(*home)
	if err != nil {
		return renderErr(stderr, err, *asJSON, *showWhy)
	}
	defer func() { _ = eng.Close() }()

	status := map[string]any{
		"tool":            "oathgate",
		"version":         version.Version,
		"status":          "ok",
		"home_dir":        eng.HomeDir(),
		"cow_backend":     eng.BackendName(),
		"max_depth":       cloner.DefaultMaxDepth,
		"max_entries":     cloner.DefaultMaxEntries,
		"network_sockets": 0,
	}
	if *asJSON {
		_ = json.NewEncoder(stdout).Encode(status)
		return ux.ExitOK
	}
	_, _ = fmt.Fprintf(stdout, "✓ oathgate v%s healthy | backend=%s | home=%s\n", version.Version, eng.BackendName(), eng.HomeDir())
	return ux.ExitOK
}

func extractPositional(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}

func renderUsage(w io.Writer, code, msg, fix string, asJSON, showWhy bool) int {
	return renderFF(w, &ux.FixForwardError{
		Tool:           "oathgate",
		Code:           code,
		Summary:        msg,
		RemediationCmd: fix,
		WhyOneLiner:    "Run `oathgate --help` to see all supported checkpoint commands.",
		ExitCode:       ux.ExitUsageErr,
	}, asJSON, showWhy)
}

func renderErr(w io.Writer, err error, asJSON, showWhy bool) int {
	ff := &ux.FixForwardError{
		Tool:           "oathgate",
		Code:           "OATHGATE_ERROR",
		Summary:        err.Error(),
		RemediationCmd: "oathgate doctor",
		WhyOneLiner:    "Inspect store permissions and snapshot IDs with `oathgate doctor` and `oathgate list`.",
		ExitCode:       ux.ExitGeneralErr,
	}
	switch {
	case errors.Is(err, cloner.ErrEscapingSymlink):
		ff.Code = "OATHGATE_ESCAPING_SYMLINK"
		ff.RemediationCmd = "find . -type l -ls"
		ff.WhyOneLiner = "Symlinks pointing outside the worktree are refused fail-closed to prevent host file exfiltration."
		ff.DeepWhy = "Oathgate validates every symlink with Lstat + containment checks before invoking APFS clonefile/FICLONE so untrusted agent trees cannot smuggle ~/.ssh or /etc into checkpoints."
		ff.ExitCode = ux.ExitSecurityErr
	case errors.Is(err, cloner.ErrSpecialFileBlocked):
		ff.Code = "OATHGATE_SPECIAL_FILE_BLOCKED"
		ff.RemediationCmd = "find . \\( -type p -o -type s \\)"
		ff.WhyOneLiner = "FIFOs, Unix sockets, and device nodes are refused fail-closed during snapshot capture."
		ff.ExitCode = ux.ExitSecurityErr
	case errors.Is(err, cloner.ErrMaxDepthExceeded), errors.Is(err, cloner.ErrMaxEntriesExceeded):
		ff.Code = "OATHGATE_TREE_BOUNDS_EXCEEDED"
		ff.RemediationCmd = "oathgate doctor"
		ff.WhyOneLiner = "Workspace exceeded safety limits (max_depth=64 or max_entries=250,000)."
		ff.ExitCode = ux.ExitPolicyDeny
	case errors.Is(err, ledger.ErrInvalidSnapID):
		ff.Code = "OATHGATE_INVALID_SNAP_ID"
		ff.RemediationCmd = "oathgate list"
		ff.WhyOneLiner = "Snapshot IDs must match og-YYYYMMDD-<8-hex> with zero path traversal characters."
		ff.ExitCode = ux.ExitPolicyDeny
	case errors.Is(err, ledger.ErrSnapNotFound), errors.Is(err, engine.ErrNoSnapshots):
		ff.Code = "OATHGATE_SNAP_NOT_FOUND"
		ff.RemediationCmd = "oathgate list"
		ff.WhyOneLiner = "No matching checkpoint exists for this worktree."
		ff.ExitCode = ux.ExitNotFound
	case errors.Is(err, engine.ErrActiveGitLock):
		ff.Code = "OATHGATE_ACTIVE_GIT_LOCK"
		ff.RemediationCmd = "ls -la .git/index.lock"
		ff.WhyOneLiner = "Rewind paused because .git/index.lock is active; wait for git to finish before rewinding."
		ff.ExitCode = ux.ExitPolicyDeny
	case errors.Is(err, engine.ErrUnsafeHomeDir), errors.Is(err, ledger.ErrSymlinkRefused):
		ff.Code = "OATHGATE_UNSAFE_STORE"
		ff.RemediationCmd = "chmod 0700 ~/.oathgate && oathgate doctor"
		ff.WhyOneLiner = "Oathgate store directories and ledger files must be regular, user-owned, non-symlink paths."
		ff.ExitCode = ux.ExitSecurityErr
	}
	return renderFF(w, ff, asJSON, showWhy)
}

func renderFF(w io.Writer, ff *ux.FixForwardError, asJSON, showWhy bool) int {
	_ = ff.Render(w, asJSON, showWhy)
	return ff.ExitCode
}

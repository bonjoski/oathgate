# `oathgate` AI Agent Guidelines

> **Repository:** `bonjoski/oathgate` (🟢 `PUBLIC_OSS` Safe Harbor)  
> **Scope:** `bonjoski/oathgate` (`.coppermind.yaml`)  
> **Parent Blueprint:** `spanreed/docs/plans/accepted/2026-11-25-oathgate-cow-time-travel-checkpoints-rfc.md` (`RFC-2026-11-25-62`)

## Architectural Invariants

1. **Zero Cloud / Zero Network:** Strictly local filesystem + SQLite WAL utility with zero network sockets.
2. **CoW Speed & Safe Fallback (`pkg/cloner`):** macOS `unix.Clonefile(..., unix.CLONE_NOFOLLOW)`, Linux `unix.IoctlFileClone`, verified `os.OpenRoot` copy fallback.
3. **Fail-Closed Confinement:** Reject escaping symlinks, FIFOs/sockets/devices, invalid snap IDs (`^og-[0-9]{8}-[0-9a-f]{8}$`), active `.git/index.lock` on rewind, and strip `setuid`/`setgid`/`sticky` bits.
4. **Portfolio Integration:** Reuse `github.com/bonjoski/tinmind/pkg/redact`, `pkg/trace`, and `pkg/ux`; never duplicate secret redaction or `UB-CLI-v2` formatting.
5. **Concurrency ($N=8$):** Per-worktree `flock` inside `~/.oathgate/locks/`, SQLite WAL (`busy_timeout=5000`) metadata writes strictly outside filesystem cloning, zero Touch ID stalls.
6. **Commit & Release Discipline:** Never `git commit` without explicit approval; sign all commits (`git commit -S`, GPG key `7BB5B44244E586B0`); bump `VERSION` + `pkg/version/version.go` and sync `bonjoski/homebrew-tap` on release.

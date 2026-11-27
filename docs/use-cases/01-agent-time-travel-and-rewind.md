# Use Case 01: Agent Time-Travel, Forensic Replay & Closed-Loop Memory

## Scenario

An autonomous coding agent is executing a risky multi-file refactor inside a worktree. You want instant, lossless rollback (preserving untracked generated code and local fixtures without cluttering `git` history), forensic visibility via `tinmind`, and automatic `dead_end` memory logging into `coppermind`.

## Workflow

### 1. Capture a Baseline Checkpoint Inside a Recorded Session

```bash
tinmind record -- sh -c '
  oathgate snap -m "pre-migration baseline" --pin
  claude "refactor storage layer to WAL"
'
```

Because `tinmind record` exports `TINMIND_SESSION_ID`, `oathgate snap` automatically bookmarks the active `tinmind` trace ID inside `~/.oathgate/ledger.db`.

### 2. Inspect Secret-Redacted Drift

```bash
oathgate list
oathgate diff latest
```

Every diff preview is scrubbed via `tinmind/pkg/redact` so `.env` credentials or API keys never appear in terminal logs.

### 3. Rewind & Teach `coppermind`

If the refactor breaks integration tests:

```bash
oathgate rewind latest \
  --reason "WAL migration broke concurrent reader transactions" \
  --record-dead-end
```

1. `oathgate` checks `.git/index.lock` (failing closed if git is busy), acquires a per-worktree `flock`, and captures a `pre_rewind` safety snapshot so you can even undo the rewind.
2. It stages and swaps the exact byte-for-byte baseline back into place (stripping any `setuid`/`setgid` bits).
3. It pushes a secret-redacted `dead_end` lesson linked to the `tinmind` trace ID into `coppermind`.

### 4. Fork an Alternative Attempt in Parallel

```bash
oathgate branch og-20261127-a1b2c3d4 ../repo-strategy-b
```

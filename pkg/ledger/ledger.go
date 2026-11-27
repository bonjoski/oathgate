package ledger

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	snapIDRe = regexp.MustCompile(`^og-[0-9]{8}-[0-9a-f]{8}$`)

	ErrInvalidSnapID   = errors.New("invalid snapshot ID (must match ^og-YYYYMMDD-<8-hex>$)")
	ErrSnapNotFound    = errors.New("snapshot not found")
	ErrSymlinkRefused  = errors.New("oathgate security violation: symlink refused fail-closed")
)

// Snapshot represents an immutable local workspace checkpoint in the SQLite WAL DAG.
type Snapshot struct {
	ID            string    `json:"id"`
	ParentID      string    `json:"parent_id,omitempty"`
	RepoHash      string    `json:"repo_hash"`
	RepoRoot      string    `json:"repo_root"`
	WorktreeID    string    `json:"worktree_id"`
	Label         string    `json:"label,omitempty"`
	Trigger       string    `json:"trigger"` // "manual" | "fabrial_hook" | "pre_rewind"
	GitHead       string    `json:"git_head,omitempty"`
	GitBranch     string    `json:"git_branch,omitempty"`
	CoWBackend    string    `json:"cow_backend"`
	DurationMs    int64     `json:"duration_ms"`
	EntriesCloned int       `json:"entries_cloned"`
	BytesLogical  int64     `json:"bytes_logical"`
	Pinned        bool      `json:"pinned"`
	TinmindID     string    `json:"tinmind_trace_id,omitempty"`
	TinmindSeq    uint64    `json:"tinmind_seq,omitempty"`
	Fingerprint   string    `json:"fingerprint,omitempty"`
	StoreRel      string    `json:"store_rel"`
	CreatedAt     time.Time `json:"created_at"`
}

// NewSnapID generates a cryptographically random snapshot ID (og-YYYYMMDD-<8-hex>).
func NewSnapID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("og-%s-%08x", time.Now().UTC().Format("20060102"), uint32(time.Now().UnixNano()))
	}
	return fmt.Sprintf("og-%s-%s", time.Now().UTC().Format("20060102"), hex.EncodeToString(b[:]))
}

// ValidateSnapID enforces the strict ^og-[0-9]{8}-[0-9a-f]{8}$ grammar fail-closed.
func ValidateSnapID(id string) error {
	if !snapIDRe.MatchString(id) {
		return fmt.Errorf("%w: %q", ErrInvalidSnapID, id)
	}
	return nil
}

// Store manages the local SQLite WAL checkpoint DAG.
type Store struct {
	db *sql.DB
}

// Open initializes (or opens) the SQLite WAL ledger at dbPath with strict non-symlink checks.
func Open(dbPath string) (*Store, error) {
	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(absPath); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: ledger db %q", ErrSymlinkRefused, absPath)
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o700); err != nil {
		return nil, err
	}

	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)", absPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite ledger: %w", err)
	}
	db.SetMaxOpenConns(1)

	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	_ = os.Chmod(absPath, 0o600)
	return &Store{db: db}, nil
}

// Close closes the SQLite connection.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func migrate(db *sql.DB) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS snapshots (
	id TEXT PRIMARY KEY,
	parent_id TEXT NOT NULL DEFAULT '',
	repo_hash TEXT NOT NULL,
	repo_root TEXT NOT NULL,
	worktree_id TEXT NOT NULL,
	label TEXT NOT NULL DEFAULT '',
	trigger TEXT NOT NULL,
	git_head TEXT NOT NULL DEFAULT '',
	git_branch TEXT NOT NULL DEFAULT '',
	cow_backend TEXT NOT NULL,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	entries_cloned INTEGER NOT NULL DEFAULT 0,
	bytes_logical INTEGER NOT NULL DEFAULT 0,
	pinned INTEGER NOT NULL DEFAULT 0,
	tinmind_trace_id TEXT NOT NULL DEFAULT '',
	tinmind_seq INTEGER NOT NULL DEFAULT 0,
	fingerprint TEXT NOT NULL DEFAULT '',
	store_rel TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_snapshots_wt_created ON snapshots(worktree_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_snapshots_created ON snapshots(created_at DESC);
`
	_, err := db.Exec(ddl)
	return err
}

// Insert records a new immutable Snapshot node in the DAG.
func (s *Store) Insert(ctx context.Context, snap Snapshot) error {
	if err := ValidateSnapID(snap.ID); err != nil {
		return err
	}
	if snap.CreatedAt.IsZero() {
		snap.CreatedAt = time.Now().UTC()
	}
	pinnedInt := 0
	if snap.Pinned {
		pinnedInt = 1
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO snapshots (
	id, parent_id, repo_hash, repo_root, worktree_id, label, trigger,
	git_head, git_branch, cow_backend, duration_ms, entries_cloned, bytes_logical,
	pinned, tinmind_trace_id, tinmind_seq, fingerprint, store_rel, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		snap.ID, snap.ParentID, snap.RepoHash, snap.RepoRoot, snap.WorktreeID, snap.Label, snap.Trigger,
		snap.GitHead, snap.GitBranch, snap.CoWBackend, snap.DurationMs, snap.EntriesCloned, snap.BytesLogical,
		pinnedInt, snap.TinmindID, snap.TinmindSeq, snap.Fingerprint, snap.StoreRel, snap.CreatedAt.UTC().Format(time.RFC3339Nano),
	)
	return err
}

// Get retrieves a snapshot by its validated ID.
func (s *Store) Get(ctx context.Context, snapID string) (*Snapshot, error) {
	if err := ValidateSnapID(snapID); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx, selectCols+` WHERE id = ?`, snapID)
	snap, err := scanSnap(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrSnapNotFound, snapID)
	}
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

// LatestForWorktree returns the most recent snapshot for worktreeID, or (nil, nil) if none exist.
func (s *Store) LatestForWorktree(ctx context.Context, worktreeID string) (*Snapshot, error) {
	row := s.db.QueryRowContext(ctx, selectCols+` WHERE worktree_id = ? ORDER BY created_at DESC LIMIT 1`, worktreeID)
	snap, err := scanSnap(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

// ListForWorktree lists snapshots for a specific worktree newest-first.
func (s *Store) ListForWorktree(ctx context.Context, worktreeID string, limit int) ([]Snapshot, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, selectCols+` WHERE worktree_id = ? ORDER BY created_at DESC LIMIT ?`, worktreeID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanSnaps(rows)
}

// ListAll returns all snapshots newest-first (for GC & diagnostics).
func (s *Store) ListAll(ctx context.Context) ([]Snapshot, error) {
	rows, err := s.db.QueryContext(ctx, selectCols+` ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanSnaps(rows)
}

// SetPinned toggles GC exemption on a snapshot.
func (s *Store) SetPinned(ctx context.Context, snapID string, pinned bool) error {
	if err := ValidateSnapID(snapID); err != nil {
		return err
	}
	val := 0
	if pinned {
		val = 1
	}
	res, err := s.db.ExecContext(ctx, `UPDATE snapshots SET pinned = ? WHERE id = ?`, val, snapID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrSnapNotFound, snapID)
	}
	return nil
}

// Delete removes a snapshot record from the DAG.
func (s *Store) Delete(ctx context.Context, snapID string) error {
	if err := ValidateSnapID(snapID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM snapshots WHERE id = ?`, snapID)
	return err
}

const selectCols = `SELECT
	id, parent_id, repo_hash, repo_root, worktree_id, label, trigger,
	git_head, git_branch, cow_backend, duration_ms, entries_cloned, bytes_logical,
	pinned, tinmind_trace_id, tinmind_seq, fingerprint, store_rel, created_at
FROM snapshots`

type scanner interface {
	Scan(dest ...any) error
}

func scanSnap(sc scanner) (Snapshot, error) {
	var snap Snapshot
	var pinnedInt int
	var createdStr string
	err := sc.Scan(
		&snap.ID, &snap.ParentID, &snap.RepoHash, &snap.RepoRoot, &snap.WorktreeID,
		&snap.Label, &snap.Trigger, &snap.GitHead, &snap.GitBranch, &snap.CoWBackend,
		&snap.DurationMs, &snap.EntriesCloned, &snap.BytesLogical, &pinnedInt,
		&snap.TinmindID, &snap.TinmindSeq, &snap.Fingerprint, &snap.StoreRel, &createdStr,
	)
	if err != nil {
		return Snapshot{}, err
	}
	snap.Pinned = pinnedInt != 0
	if ts, perr := time.Parse(time.RFC3339Nano, strings.TrimSpace(createdStr)); perr == nil {
		snap.CreatedAt = ts.UTC()
	}
	return snap, nil
}

func scanSnaps(rows *sql.Rows) ([]Snapshot, error) {
	var out []Snapshot
	for rows.Next() {
		snap, err := scanSnap(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, snap)
	}
	return out, rows.Err()
}

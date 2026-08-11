// Package state persists bowt's worktree registry in SQLite. The Store
// interface is unchanged from the earlier JSON version — proof of the seam:
// swapping the backing store touched no caller (worktree.go, main.go).
package state

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registered under the name "sqlite"
)

// Worktree is one registered worktree. The json tags still define the wire
// (agent-facing) shape; the DB columns are an internal detail.
type Worktree struct {
	Repo    string    `json:"repo"`
	Branch  string    `json:"branch"`
	Offset  int       `json:"offset"`
	Port    int       `json:"port"`
	Path    string    `json:"path"`
	Created time.Time `json:"created"`
}

// Store is the persistence seam — identical to the JSON era.
type Store interface {
	List(repo string) ([]Worktree, error)
	Get(repo, branch string) (Worktree, bool, error)
	Add(wt Worktree) error
	Remove(repo, branch string) error
	NextOffset(repo string) (int, error)
}

// ErrExists is returned by Add when a worktree is already registered.
var ErrExists = errors.New("worktree already registered")

// offset is a SQL keyword, so the column is offset_n; the struct field stays Offset.
const schema = `
CREATE TABLE IF NOT EXISTS worktrees (
	repo     TEXT    NOT NULL,
	branch   TEXT    NOT NULL,
	offset_n INTEGER NOT NULL,
	port     INTEGER NOT NULL,
	path     TEXT    NOT NULL,
	created  TEXT    NOT NULL,
	PRIMARY KEY (repo, branch)
);`

type sqlStore struct{ db *sql.DB }

// Open returns the default store at ~/.bowt/state.db, creating ~/.bowt if needed.
func Open() (Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".bowt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return OpenAt(filepath.Join(dir, "state.db"))
}

// OpenAt opens a store at an explicit path — used by tests with a temp file.
func OpenAt(path string) (Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// A single connection: SQLite serializes writes anyway, and this sidesteps
	// "database is locked" under the database/sql pool for a local single-user tool.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &sqlStore{db: db}, nil
}

func (s *sqlStore) List(repo string) ([]Worktree, error) {
	rows, err := s.db.Query(
		`SELECT repo, branch, offset_n, port, path, created FROM worktrees WHERE repo=? ORDER BY offset_n`, repo)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Worktree
	for rows.Next() {
		wt, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, wt)
	}
	return out, rows.Err()
}

func (s *sqlStore) Get(repo, branch string) (Worktree, bool, error) {
	row := s.db.QueryRow(
		`SELECT repo, branch, offset_n, port, path, created FROM worktrees WHERE repo=? AND branch=?`, repo, branch)
	wt, err := scanRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Worktree{}, false, nil
	}
	if err != nil {
		return Worktree{}, false, err
	}
	return wt, true, nil
}

func (s *sqlStore) Add(wt Worktree) error {
	// Friendly sentinel for the common case; the PRIMARY KEY is the real guard.
	if _, ok, err := s.Get(wt.Repo, wt.Branch); err != nil {
		return err
	} else if ok {
		return ErrExists
	}
	_, err := s.db.Exec(
		`INSERT INTO worktrees (repo, branch, offset_n, port, path, created) VALUES (?,?,?,?,?,?)`,
		wt.Repo, wt.Branch, wt.Offset, wt.Port, wt.Path, wt.Created.Format(time.RFC3339Nano))
	return err
}

func (s *sqlStore) Remove(repo, branch string) error {
	_, err := s.db.Exec(`DELETE FROM worktrees WHERE repo=? AND branch=?`, repo, branch)
	return err
}

func (s *sqlStore) NextOffset(repo string) (int, error) {
	var maxOff sql.NullInt64 // MAX over zero rows is NULL, not 0 — NullInt64 handles it
	if err := s.db.QueryRow(`SELECT MAX(offset_n) FROM worktrees WHERE repo=?`, repo).Scan(&maxOff); err != nil {
		return 0, err
	}
	if !maxOff.Valid {
		return 1, nil
	}
	return int(maxOff.Int64) + 1, nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows, so scanRow works for
// single-row and multi-row queries alike.
type scanner interface {
	Scan(dest ...any) error
}

func scanRow(sc scanner) (Worktree, error) {
	var wt Worktree
	var created string
	if err := sc.Scan(&wt.Repo, &wt.Branch, &wt.Offset, &wt.Port, &wt.Path, &created); err != nil {
		return Worktree{}, err
	}
	wt.Created, _ = time.Parse(time.RFC3339Nano, created)
	return wt, nil
}

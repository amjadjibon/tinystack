// Package sqlite persists the store in one SQLite database. Records are served
// from memory, and every commit is written to the database before it becomes
// visible, so a restart recovers exactly the committed state.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"

	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

// schemaVersion is the database's PRAGMA user_version. Open refuses other
// versions rather than guessing at their layout.
const schemaVersion = 1

// File is the database's name inside the storage directory.
const File = "state.db"

// Store is a memory store whose commits are written through to SQLite.
type Store struct {
	*memory.Store
	db   *sql.DB
	conn *sql.Conn
}

// Open loads the database in dir, creating both if needed. The database stays
// locked until Close, so a second TinyStack cannot share it.
func Open(ctx context.Context, dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	path := filepath.Join(dir, File)
	// Create the file up front so it is readable only by its owner: records
	// hold message bodies, object data, and function environments.
	// #nosec G304 -- the user chooses the storage directory in the configuration.
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}

	if closeErr := f.Close(); closeErr != nil {
		return nil, closeErr
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	s := &Store{db: db}
	data, err := s.open(ctx)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open %s: %w", path, err), s.close())
	}

	s.Store = memory.New(data, s.write)
	return s, nil
}

// open pins the one connection every write uses, locks the database, and
// loads its records.
func (s *Store) open(ctx context.Context) (map[string][]byte, error) {
	var err error
	s.conn, err = s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}

	// Exclusive locking holds the lock taken by BEGIN EXCLUSIVE until the
	// connection closes; busy_timeout 0 makes a second opener fail at once.
	for _, stmt := range []string{
		"PRAGMA busy_timeout = 0",
		"PRAGMA locking_mode = EXCLUSIVE",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
		"BEGIN EXCLUSIVE",
		"COMMIT",
	} {
		if _, err = s.conn.ExecContext(ctx, stmt); err != nil {
			return nil, fmt.Errorf("%s: %w", stmt, err)
		}
	}

	if migrateErr := s.migrate(ctx); migrateErr != nil {
		return nil, migrateErr
	}

	rows, err := s.conn.QueryContext(ctx, "SELECT key, value FROM records")
	if err != nil {
		return nil, err
	}

	defer func() { _ = rows.Close() }()
	data := map[string][]byte{}
	for rows.Next() {
		var key string
		var value []byte
		if scanErr := rows.Scan(&key, &value); scanErr != nil {
			return nil, scanErr
		}

		data[key] = value
	}

	return data, rows.Err()
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}

	switch version {
	case schemaVersion:
		return nil
	case 0:
		_, err := s.conn.ExecContext(ctx, fmt.Sprintf(`
			BEGIN;
			CREATE TABLE records (key TEXT PRIMARY KEY, value BLOB NOT NULL) WITHOUT ROWID;
			PRAGMA user_version = %d;
			COMMIT;`, schemaVersion))
		return err
	default:
		return fmt.Errorf("schema version %d is not supported (want %d)", version, schemaVersion)
	}
}

// write commits one transaction's changes. It ignores the request's context:
// once the records are staged, the commit must not stop halfway.
func (s *Store) write(puts map[string][]byte, deletes []string) error {
	ctx := context.Background()
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	defer func() { _ = tx.Rollback() }()
	for _, key := range deletes {
		if _, err = tx.ExecContext(ctx, "DELETE FROM records WHERE key = ?", key); err != nil {
			return err
		}
	}

	for key, value := range puts {
		// A nil slice would bind as NULL; an empty record is an empty blob.
		if value == nil {
			value = []byte{}
		}

		_, err = tx.ExecContext(
			ctx,
			"INSERT INTO records (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value",
			key,
			value,
		)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// Close releases the database. It waits for the running commit, and later
// updates fail instead of changing only memory.
func (s *Store) Close() error {
	var err error
	// An update with no changes holds the store's write lock without writing.
	_ = s.Update(context.Background(), func(storage.WriteTx) error {
		err = s.close()
		return nil
	})

	return err
}

func (s *Store) close() error {
	var err error
	if s.conn != nil {
		err = s.conn.Close()
	}

	return errors.Join(err, s.db.Close())
}

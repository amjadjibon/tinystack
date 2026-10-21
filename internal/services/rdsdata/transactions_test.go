package rdsdata

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/services/rds"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

type (
	lifecycleDriver struct{}
	lifecycleConn   struct{}
	lifecycleTx     struct{}
)

var rollbacks atomic.Int64

func (lifecycleDriver) Open(string) (driver.Conn, error) { return lifecycleConn{}, nil }
func (lifecycleConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("SQL is not used by lifecycle tests")
}

func (lifecycleConn) Close() error              { return nil }
func (lifecycleConn) Begin() (driver.Tx, error) { return lifecycleTx{}, nil }
func (lifecycleTx) Commit() error               { return nil }
func (lifecycleTx) Rollback() error             { rollbacks.Add(1); return nil }
func init()                                     { sql.Register("rdsdata-lifecycle-test", lifecycleDriver{}) }

func lifecycleTransaction(t *testing.T, created, used time.Time) *transaction {
	t.Helper()
	db, err := sql.Open("rdsdata-lifecycle-test", "")
	if err != nil {
		t.Fatal(err)
	}

	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}

	return &transaction{tx: tx, db: db, arn: "cluster", secret: "secret", created: created, used: used}
}

func TestTransactionLifetimeAndCleanup(t *testing.T) {
	s := New(rds.New(memory.New(nil, nil)))
	defer s.Stop()
	now := time.Now()
	s.mu.Lock()
	s.transactions["idle"] = lifecycleTransaction(t, now.Add(-time.Hour), now.Add(-idleTimeout))
	s.transactions["old"] = lifecycleTransaction(t, now.Add(-maxLifetime), now)
	s.transactions["live"] = lifecycleTransaction(t, now, now)
	s.mu.Unlock()
	before := rollbacks.Load()
	s.expire(t.Context())
	s.mu.Lock()
	if len(s.transactions) != 1 || s.transactions["live"] == nil {
		t.Fatal("expired transactions retained")
	}

	if _, err := s.getTransaction("live", "other-cluster", "secret"); err == nil {
		t.Fatal("cross-cluster transaction accepted")
	}

	s.mu.Unlock()
	if rollbacks.Load()-before != 2 {
		t.Fatal("expired SQL transactions were not rolled back")
	}

	s.CloseCluster("cluster")
	s.mu.Lock()
	if len(s.transactions) != 0 {
		t.Fatal("cluster stop retained transactions")
	}

	s.transactions["reset"] = lifecycleTransaction(t, now, now)
	s.mu.Unlock()
	s.Reset(context.Background())
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.transactions) != 0 {
		t.Fatal("reset retained transactions")
	}
}

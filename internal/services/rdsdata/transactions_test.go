package rdsdata

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/rdsdata"
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

type (
	testEngineDriver struct{}
	testEngineConn   struct{}
	testEngineTx     struct{}
	testEngineStmt   struct{}
	testEngineRows   struct {
		columns []string
		types   []string
		rows    [][]driver.Value
		idx     int
	}
)

var (
	testBlockStart chan struct{}
	testBlockEnd   chan struct{}
)

func (testEngineDriver) Open(string) (driver.Conn, error) { return &testEngineConn{}, nil }

func (testEngineConn) Prepare(string) (driver.Stmt, error) {
	return &testEngineStmt{}, nil
}

func (testEngineConn) Close() error              { return nil }
func (testEngineConn) Begin() (driver.Tx, error) { return &testEngineTx{}, nil }

func (testEngineConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return &testEngineTx{}, nil
}

func (testEngineConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}

func (testEngineConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "BLOCK") {
		if testBlockStart != nil {
			testBlockStart <- struct{}{}
		}

		if testBlockEnd != nil {
			<-testBlockEnd
		}
	}

	if strings.Contains(query, "LARGE") {
		largePayload := strings.Repeat("x", maxResponseBytes+100)
		return &testEngineRows{
			columns: []string{"payload"},
			types:   []string{"VARCHAR"},
			rows:    [][]driver.Value{{largePayload}},
		}, nil
	}

	return &testEngineRows{
		columns: []string{"val"},
		types:   []string{"VARCHAR"},
		rows:    [][]driver.Value{{"ok"}},
	}, nil
}

func (testEngineTx) Commit() error   { return nil }
func (testEngineTx) Rollback() error { return nil }

func (testEngineStmt) Close() error { return nil }

func (testEngineStmt) NumInput() int { return -1 }

func (testEngineStmt) Exec([]driver.Value) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}

func (testEngineStmt) Query([]driver.Value) (driver.Rows, error) {
	return &testEngineRows{}, nil
}

func (r *testEngineRows) Columns() []string { return r.columns }

func (r *testEngineRows) Close() error { return nil }

func (r *testEngineRows) ColumnTypeDatabaseTypeName(index int) string { return r.types[index] }

func (r *testEngineRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.rows) {
		return io.EOF
	}

	copy(dest, r.rows[r.idx])
	r.idx++
	return nil
}

func init() {
	sql.Register("rdsdata-lifecycle-test", lifecycleDriver{})
	sql.Register("rdsdata-test-engine", testEngineDriver{})
}

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

	return &transaction{
		tx:      tx,
		db:      db,
		arn:     "cluster",
		secret:  "secret",
		driver:  driverPostgres,
		created: created,
		used:    used,
	}
}

func testTransaction(t *testing.T, created, used time.Time) *transaction {
	t.Helper()
	db, err := sql.Open("rdsdata-test-engine", "")
	if err != nil {
		t.Fatal(err)
	}

	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}

	return &transaction{
		tx:      tx,
		db:      db,
		arn:     "cluster",
		secret:  "secret",
		driver:  driverPostgres,
		created: created,
		used:    used,
	}
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
	liveCount := len(s.transactions)
	hasLive := s.transactions["live"] != nil
	s.mu.Unlock()
	if liveCount != 1 || !hasLive {
		t.Fatal("expired transactions retained")
	}

	if _, err := s.getTransaction("live", "other-cluster", "secret"); err == nil {
		t.Fatal("cross-cluster transaction accepted")
	}

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

func TestTransactionMutexDoesNotBlockMapOperations(t *testing.T) {
	s := New(rds.New(memory.New(nil, nil)))
	defer s.Stop()

	now := time.Now()
	tx1 := lifecycleTransaction(t, now, now)
	tx2 := lifecycleTransaction(t, now, now)

	s.mu.Lock()
	s.transactions["tx1"] = tx1
	s.transactions["tx2"] = tx2
	s.mu.Unlock()

	tx1.mu.Lock()
	defer tx1.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := s.getTransaction("tx2", "cluster", "secret")
		if err != nil {
			t.Errorf("unexpected error getting tx2: %v", err)
		}

		s.mu.Lock()
		count := len(s.transactions)
		s.mu.Unlock()
		if count != 2 {
			t.Errorf("expected 2 transactions, got %d", count)
		}
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("map operations were blocked by transaction mutex")
	}
}

func TestExecuteStatementDifferentConnectionsDoNotSerialize(t *testing.T) {
	testBlockStart = make(chan struct{})
	testBlockEnd = make(chan struct{})
	defer func() {
		testBlockStart = nil
		testBlockEnd = nil
	}()

	s := New(rds.New(memory.New(nil, nil)))
	defer s.Stop()

	now := time.Now()
	tx1 := testTransaction(t, now, now)
	tx2 := testTransaction(t, now, now)

	s.mu.Lock()
	s.transactions["tx1"] = tx1
	s.transactions["tx2"] = tx2
	s.mu.Unlock()

	tx1Done := make(chan error, 1)
	go func() {
		_, err := s.ExecuteStatement(context.Background(), &api.ExecuteStatementRequest{
			TransactionId: new("tx1"),
			ResourceArn:   new("cluster"),
			SecretArn:     new("secret"),
			Sql:           new("SELECT BLOCK"),
		})
		tx1Done <- err
	}()

	select {
	case <-testBlockStart:
	case <-time.After(time.Second):
		t.Fatal("tx1 query did not start in time")
	}

	tx2Done := make(chan error, 1)
	go func() {
		_, err := s.ExecuteStatement(context.Background(), &api.ExecuteStatementRequest{
			TransactionId: new("tx2"),
			ResourceArn:   new("cluster"),
			SecretArn:     new("secret"),
			Sql:           new("SELECT SMALL"),
		})
		tx2Done <- err
	}()

	select {
	case err := <-tx2Done:
		if err != nil {
			t.Fatalf("tx2 failed: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("tx2 was serialized and blocked behind tx1")
	}

	close(testBlockEnd)
	select {
	case err := <-tx1Done:
		if err != nil {
			t.Fatalf("tx1 failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("tx1 did not finish")
	}
}

func TestExecuteStatementClosedTransactionWhileWaiting(t *testing.T) {
	s := New(rds.New(memory.New(nil, nil)))
	defer s.Stop()

	now := time.Now()
	tx1 := testTransaction(t, now, now)

	s.mu.Lock()
	s.transactions["tx1"] = tx1
	s.mu.Unlock()

	tx1.mu.Lock()
	errCh := make(chan error, 1)
	go func() {
		_, err := s.ExecuteStatement(context.Background(), &api.ExecuteStatementRequest{
			TransactionId: new("tx1"),
			ResourceArn:   new("cluster"),
			SecretArn:     new("secret"),
			Sql:           new("SELECT SMALL"),
		})
		errCh <- err
	}()

	time.Sleep(30 * time.Millisecond)
	tx1.closed = true
	tx1.mu.Unlock()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error for closed transaction")
		}

		var awsErr *awserror.Error
		if !errors.As(err, &awsErr) || awsErr.Code != "TransactionNotFoundException" ||
			awsErr.Message != "Transaction not found" {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("statement did not return in time")
	}
}

func TestExecuteStatementResponseSizeLimit(t *testing.T) {
	s := New(rds.New(memory.New(nil, nil)))
	defer s.Stop()

	now := time.Now()
	s.mu.Lock()
	s.transactions["tx"] = testTransaction(t, now, now)
	s.mu.Unlock()

	// 1. Standard records mode exceeding 1 MiB returns BadRequestException
	_, err := s.ExecuteStatement(context.Background(), &api.ExecuteStatementRequest{
		TransactionId: new("tx"),
		ResourceArn:   new("cluster"),
		SecretArn:     new("secret"),
		Sql:           new("SELECT LARGE"),
	})
	if err == nil {
		t.Fatal("expected size limit error for large records")
	}

	var awsErr *awserror.Error
	if !errors.As(err, &awsErr) || awsErr.Code != "BadRequestException" ||
		awsErr.Message != "Database returned more than the allowed response size limit" {
		t.Fatalf("unexpected error for large records: %v", err)
	}

	// 2. JSON formattedRecords mode exceeding 1 MiB returns BadRequestException
	_, err = s.ExecuteStatement(context.Background(), &api.ExecuteStatementRequest{
		TransactionId:   new("tx"),
		ResourceArn:     new("cluster"),
		SecretArn:       new("secret"),
		Sql:             new("SELECT LARGE"),
		FormatRecordsAs: new("JSON"),
	})
	if err == nil {
		t.Fatal("expected size limit error for large JSON records")
	}

	if !errors.As(err, &awsErr) || awsErr.Code != "BadRequestException" ||
		awsErr.Message != "Database returned more than the allowed response size limit" {
		t.Fatalf("unexpected error for large JSON records: %v", err)
	}

	// 3. Small query within limit succeeds
	out, err := s.ExecuteStatement(context.Background(), &api.ExecuteStatementRequest{
		TransactionId: new("tx"),
		ResourceArn:   new("cluster"),
		SecretArn:     new("secret"),
		Sql:           new("SELECT SMALL"),
	})
	if err != nil {
		t.Fatalf("unexpected error for small query: %v", err)
	}

	if len(out.Records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(out.Records))
	}
}

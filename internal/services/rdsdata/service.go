// Package rdsdata executes SQL against the optional Aurora cluster engines.
package rdsdata

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq" // Register the PostgreSQL database/sql driver.

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/rdsdata"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/rds"
)

const (
	driverMySQL        = "mysql"
	driverPostgres     = "postgres"
	hintJSON           = "JSON"
	hintDate           = "DATE"
	hintTime           = "TIME"
	hintTimestamp      = "TIMESTAMP"
	hintDecimal        = "DECIMAL"
	typeBigInt         = "BIGINT"
	typeBoolean        = "BOOLEAN"
	typeBytea          = "BYTEA"
	badRequest         = "BadRequestException"
	transactionMissing = "TransactionNotFoundException"
	idleTimeout        = 3 * time.Minute
	maxLifetime        = 24 * time.Hour
)

var _ api.API = (*Service)(nil)

type transaction struct {
	tx       *sql.Tx
	db       *sql.DB
	cancel   context.CancelFunc
	created  time.Time
	used     time.Time
	arn      string
	secret   string
	database string
}

type Service struct {
	clusters     *rds.Service
	caller       *catalog.Caller
	bg           *background.Group
	transactions map[string]*transaction
	mu           sync.Mutex
}

func New(clusters *rds.Service) *Service {
	s := &Service{clusters: clusters, bg: background.New(), transactions: map[string]*transaction{}}
	clusters.UseDataCloser(s.CloseCluster)
	s.bg.Every(time.Second, s.expire)
	return s
}

func (s *Service) UseCaller(c *catalog.Caller) { s.caller = c }

func (s *Service) Stop() {
	s.bg.Stop()
	s.Reset(context.Background())
}

func (s *Service) Reset(_ context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, tx := range s.transactions {
		s.closeTransaction(id, tx)
	}
}

// CloseCluster rolls back open transactions when its engine stops.
func (s *Service) CloseCluster(arn string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, tx := range s.transactions {
		if tx.arn == arn {
			s.closeTransaction(id, tx)
		}
	}
}

func (s *Service) closeTransaction(id string, tx *transaction) {
	_ = tx.tx.Rollback()
	if tx.cancel != nil {
		tx.cancel()
	}

	_ = tx.db.Close()
	delete(s.transactions, id)
}

func (s *Service) expire(_ context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, tx := range s.transactions {
		if now.Sub(tx.used) >= idleTimeout || now.Sub(tx.created) >= maxLifetime {
			s.closeTransaction(id, tx)
		}
	}
}

func (s *Service) connection(ctx context.Context, arn, secret, database string) (*sql.DB, string, error) {
	spec, port, err := s.clusters.DataEngine(ctx, arn)
	if err != nil {
		return nil, "", err
	}

	if s.caller == nil {
		return nil, "", awserror.New("ForbiddenException", "Secret is unavailable")
	}

	out, err := s.caller.Call(ctx, "secretsmanager", "GetSecretValue", map[string]any{"SecretId": secret})
	if err != nil {
		return nil, "", awserror.New("ForbiddenException", "Secret is unavailable")
	}

	value, _ := out["SecretString"].(string)
	var credentials struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}

	if json.Unmarshal([]byte(value), &credentials) != nil || credentials.Username == "" || credentials.Password == "" ||
		subtle.ConstantTimeCompare([]byte(credentials.Username), []byte(spec.Username)) != 1 ||
		subtle.ConstantTimeCompare([]byte(credentials.Password), []byte(spec.Password)) != 1 {
		return nil, "", awserror.New("ForbiddenException", "Secret credentials do not match the cluster")
	}

	if port == 0 {
		return nil, "", awserror.New(
			"InvalidResourceStateException",
			"Cluster has no running engine; configure the Docker RDS runtime",
		)
	}

	if database == "" {
		database = spec.Database
	}

	address := net.JoinHostPort("127.0.0.1", strconv.FormatInt(port, 10))
	driver, dsn := driverPostgres, ""
	if spec.Engine == rds.EnginePostgres {
		u := &url.URL{
			Scheme: driverPostgres,
			User:   url.UserPassword(spec.Username, spec.Password),
			Host:   address,
			Path:   "/" + database,
		}

		q := u.Query()
		q.Set("sslmode", "disable")
		q.Set("connect_timeout", "10")
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		driver = driverMySQL
		cfg := mysql.NewConfig()
		cfg.User, cfg.Passwd, cfg.Net, cfg.Addr, cfg.DBName = spec.Username, spec.Password, "tcp", address, database
		cfg.Timeout = 10 * time.Second
		dsn = cfg.FormatDSN()
	}

	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, "", awserror.New(badRequest, err.Error())
	}

	db.SetMaxOpenConns(1)
	return db, driver, nil
}

func (s *Service) getTransaction(id, arn, secret string) (*transaction, error) {
	tx, ok := s.transactions[id]
	if !ok || tx.arn != arn || tx.secret != secret {
		return nil, awserror.New(transactionMissing, "Transaction not found")
	}

	now := time.Now()
	if now.Sub(tx.used) >= idleTimeout || now.Sub(tx.created) >= maxLifetime {
		s.closeTransaction(id, tx)
		return nil, awserror.New(transactionMissing, "Transaction has expired")
	}

	tx.used = now
	return tx, nil
}

//nolint:contextcheck // Transactions use the service lifetime across HTTP requests, bounded to 24 hours.
func (s *Service) BeginTransaction(
	ctx context.Context,
	in *api.BeginTransactionRequest,
) (*api.BeginTransactionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, driver, err := s.connection(
		ctx,
		common.Value(in.ResourceArn),
		common.Value(in.SecretArn),
		common.Value(in.Database),
	)
	if err != nil {
		return nil, err
	}

	if err = db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, awserror.New(badRequest, err.Error())
	}

	// The transaction outlives this HTTP request. Its lifetime belongs to the service.
	txCtx, txCancel := context.WithTimeout(s.bg.Context(), maxLifetime)
	tx, err := db.BeginTx(txCtx, nil)
	if err != nil {
		txCancel()
		_ = db.Close()
		return nil, awserror.New(badRequest, err.Error())
	}

	if err := setSchema(ctx, tx, driver, common.Value(in.Schema)); err != nil {
		_ = tx.Rollback()
		txCancel()
		_ = db.Close()
		return nil, err
	}

	id, now := common.RandomID(32), time.Now()
	s.transactions[id] = &transaction{
		cancel:   txCancel,
		tx:       tx,
		db:       db,
		arn:      common.Value(in.ResourceArn),
		secret:   common.Value(in.SecretArn),
		database: common.Value(in.Database),
		created:  now,
		used:     now,
	}

	return &api.BeginTransactionResponse{TransactionId: new(id)}, nil
}

func (s *Service) finish(ctx context.Context, arn, secret, id string, commit bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, _, err := s.connection(ctx, arn, secret, "")
	if err != nil {
		return err
	}

	_ = db.Close()
	tx, err := s.getTransaction(id, arn, secret)
	if err != nil {
		return err
	}

	defer s.closeTransaction(id, tx)
	if commit {
		err = tx.tx.Commit()
	} else {
		err = tx.tx.Rollback()
	}

	if err != nil {
		return awserror.New(badRequest, err.Error())
	}

	return nil
}

func (s *Service) CommitTransaction(
	ctx context.Context,
	in *api.CommitTransactionRequest,
) (*api.CommitTransactionResponse, error) {
	err := s.finish(ctx, common.Value(in.ResourceArn), common.Value(in.SecretArn), common.Value(in.TransactionId), true)
	return &api.CommitTransactionResponse{TransactionStatus: new("Transaction Committed")}, err
}

func (s *Service) RollbackTransaction(
	ctx context.Context,
	in *api.RollbackTransactionRequest,
) (*api.RollbackTransactionResponse, error) {
	err := s.finish(
		ctx,
		common.Value(in.ResourceArn),
		common.Value(in.SecretArn),
		common.Value(in.TransactionId),
		false,
	)
	return &api.RollbackTransactionResponse{TransactionStatus: new("Transaction Rolled Back")}, err
}

// sqlError preserves the database's diagnostic text in the modeled AWS error.
func sqlError(err error) error { return awserror.New(badRequest, fmt.Sprint(err)) }

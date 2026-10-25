package redshiftdata

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/redshiftdata"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) execute(
	ctx context.Context,
	in *api.ExecuteStatementInput,
	sqls []string,
	batch bool,
) (*record, error) {
	if common.Value(in.WithEvent) {
		return nil, invalid("WithEvent=true is unsupported; no EventBridge events are emitted")
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if validationErr := s.validateTarget(ctx, in); validationErr != nil {
		return nil, validationErr
	}

	substituted := make([]string, len(sqls))
	for i, sql := range sqls {
		substituted[i], err = substitute(sql, in.Parameters)
		if err != nil {
			return nil, err
		}
	}

	fingerprint, err := json.Marshal(struct {
		Input *api.ExecuteStatementInput
		SQLs  []string
		Batch bool
	}{in, sqls, batch})
	if err != nil {
		return nil, err
	}

	digest := sha256.Sum256([]byte(common.Value(in.ClientToken)))
	tokenKey := scope.Key(serviceName, "token", fmt.Sprintf("%x", digest))
	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if in.ClientToken != nil {
			old, ok, e := storage.Read[replay](tx, tokenKey)
			if e != nil {
				return e
			}

			if ok && time.Now().Before(old.Expires) {
				if old.Fingerprint != string(fingerprint) {
					return invalid("ClientToken was already used with different input")
				}

				r, e = read(tx, scope, old.ID)
				return e
			}
		}

		fixtures, e := storage.Records[Fixture](
			tx,
			(request.Scope{AccountID: scope.AccountID}).Prefix(serviceName, "fixture"),
		)
		if e != nil {
			return e
		}

		now := time.Now().UTC()
		stamp := float64(now.UnixMilli()) / 1000
		id := uuid.NewV4().String()
		r = &record{
			DescribeStatementResponse: api.DescribeStatementResponse{
				Id:                new(id),
				CreatedAt:         new(stamp),
				UpdatedAt:         new(stamp),
				Status:            new(statusSubmitted),
				Database:          in.Database,
				ClusterIdentifier: in.ClusterIdentifier,
				WorkgroupName:     in.WorkgroupName,
				DbUser:            in.DbUser,
				SecretArn:         in.SecretArn,
				QueryString:       in.Sql,
				QueryParameters:   in.Parameters,
				ResultFormat:      common.OrElse(in.ResultFormat, new(formatJSON)),
			},
			Name:    in.StatementName,
			SQLs:    sqls,
			Batch:   batch,
			Due:     now.Add(150 * time.Millisecond),
			Expires: now.Add(24 * time.Hour),
		}

		for i, sql := range substituted {
			f := Fixture{
				Error: new("No fixture matches this SQL; register an answer at /_tinystack/redshift-data/fixtures"),
			}

			for _, candidate := range fixtures {
				if candidate.matches(
					sql,
					common.Value(in.ClusterIdentifier),
					common.Value(in.WorkgroupName),
					common.Value(in.Database),
				) {
					f = *candidate
					break
				}
			}

			r.Answers = append(r.Answers, f)
			if batch {
				r.SubStatements = append(
					r.SubStatements,
					api.SubStatementData{
						Id:          new(subID(id, i)),
						QueryString: new(sqls[i]),
						Status:      new(statusSubmitted),
						CreatedAt:   new(stamp),
						UpdatedAt:   new(stamp),
					},
				)
			}
		}

		if e := save(tx, scope, r); e != nil {
			return e
		}

		if e := storage.Write(
			tx,
			indexPrefix+scope.AccountID+"/"+scope.Region+"/"+id,
			work{Scope: scope, ID: id},
		); e != nil {
			return e
		}

		if in.ClientToken != nil {
			return storage.Write(
				tx,
				tokenKey,
				replay{Fingerprint: string(fingerprint), ID: id, Expires: now.Add(8 * time.Hour)},
			)
		}

		return nil
	})
	return r, err
}

func (s *Service) ExecuteStatement(
	ctx context.Context,
	in *api.ExecuteStatementInput,
) (*api.ExecuteStatementOutput, error) {
	r, err := s.execute(ctx, in, []string{common.Value(in.Sql)}, false)
	if err != nil {
		return nil, err
	}

	return &api.ExecuteStatementOutput{
		Id:                r.Id,
		CreatedAt:         r.CreatedAt,
		ClusterIdentifier: r.ClusterIdentifier,
		WorkgroupName:     r.WorkgroupName,
		Database:          r.Database,
		DbUser:            r.DbUser,
		SecretArn:         r.SecretArn,
	}, nil
}

func (s *Service) BatchExecuteStatement(
	ctx context.Context,
	in *api.BatchExecuteStatementInput,
) (*api.BatchExecuteStatementOutput, error) {
	r, err := s.execute(
		ctx,
		&api.ExecuteStatementInput{
			ClusterIdentifier: in.ClusterIdentifier,
			WorkgroupName:     in.WorkgroupName,
			Database:          in.Database,
			DbUser:            in.DbUser,
			SecretArn:         in.SecretArn,
			Parameters:        in.Parameters,
			StatementName:     in.StatementName,
			ClientToken:       in.ClientToken,
			ResultFormat:      in.ResultFormat,
			WithEvent:         in.WithEvent,
		},
		in.Sqls,
		true,
	)
	if err != nil {
		return nil, err
	}

	return &api.BatchExecuteStatementOutput{
		Id:                r.Id,
		CreatedAt:         r.CreatedAt,
		ClusterIdentifier: r.ClusterIdentifier,
		WorkgroupName:     r.WorkgroupName,
		Database:          r.Database,
		DbUser:            r.DbUser,
		SecretArn:         r.SecretArn,
	}, nil
}

func (s *Service) DescribeStatement(
	ctx context.Context,
	in *api.DescribeStatementRequest,
) (*api.DescribeStatementResponse, error) {
	parent, child := splitID(common.Value(in.Id))
	r, err := s.get(ctx, new(parent))
	if err != nil {
		return nil, err
	}

	if child != "" {
		for _, sub := range r.SubStatements {
			if common.Value(sub.Id) == common.Value(in.Id) {
				return &api.DescribeStatementResponse{
					Id:                sub.Id,
					Status:            sub.Status,
					Error:             sub.Error,
					CreatedAt:         sub.CreatedAt,
					UpdatedAt:         sub.UpdatedAt,
					QueryString:       sub.QueryString,
					HasResultSet:      sub.HasResultSet,
					ResultRows:        sub.ResultRows,
					ResultFormat:      r.ResultFormat,
					Database:          r.Database,
					WorkgroupName:     r.WorkgroupName,
					ClusterIdentifier: r.ClusterIdentifier,
				}, nil
			}
		}

		return nil, missing()
	}

	return &r.DescribeStatementResponse, nil
}

func splitID(id string) (parent, child string) {
	parent, child, _ = strings.Cut(id, ":")
	return parent, child
}

func (s *Service) CancelStatement(
	ctx context.Context,
	in *api.CancelStatementRequest,
) (*api.CancelStatementResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	success := false
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, common.Value(in.Id))
		if e != nil {
			return e
		}

		if common.Value(r.Status) != statusSubmitted && common.Value(r.Status) != statusStarted {
			return invalid("Statement can only be cancelled before it completes")
		}

		r.Status = new(statusAborted)
		r.UpdatedAt = new(float64(time.Now().UTC().UnixMilli()) / 1000)
		for i := range r.SubStatements {
			r.SubStatements[i].Status = new(statusAborted)
			r.SubStatements[i].UpdatedAt = r.UpdatedAt
		}

		success = true
		return save(tx, scope, r)
	})
	return &api.CancelStatementResponse{Status: new(success)}, err
}

func (s *Service) ListStatements(
	ctx context.Context,
	in *api.ListStatementsRequest,
) (*api.ListStatementsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	rows := []*record{}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, e := storage.Records[record](tx, scope.Prefix(serviceName, kindStatement))
		if e != nil {
			return e
		}

		for _, r := range all {
			if !time.Now().Before(r.Expires) {
				continue
			}

			if in.Status != nil && *in.Status != "ALL" && common.Value(r.Status) != *in.Status {
				continue
			}

			if in.StatementName != nil && !strings.HasPrefix(common.Value(r.Name), *in.StatementName) {
				continue
			}

			if in.Database != nil && common.Value(r.Database) != *in.Database {
				continue
			}

			if in.ClusterIdentifier != nil && common.Value(r.ClusterIdentifier) != *in.ClusterIdentifier {
				continue
			}

			if in.WorkgroupName != nil && common.Value(r.WorkgroupName) != *in.WorkgroupName {
				continue
			}

			rows = append(rows, r)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	rows, token, err := common.Page(
		rows,
		func(r *record) string { return common.Value(r.Id) },
		in.NextToken,
		int(common.Value(common.OrElse(in.MaxResults, new(int64(100))))),
		invalid(invalidTokenMessage),
	)
	if err != nil {
		return nil, err
	}

	out := &api.ListStatementsResponse{NextToken: token, Statements: []api.StatementData{}}

	for _, r := range rows {
		out.Statements = append(
			out.Statements,
			api.StatementData{
				Id:               r.Id,
				StatementName:    r.Name,
				Status:           r.Status,
				CreatedAt:        r.CreatedAt,
				UpdatedAt:        r.UpdatedAt,
				IsBatchStatement: new(r.Batch),
				QueryString:      r.QueryString,
				QueryStrings:     r.SQLs,
				QueryParameters:  r.QueryParameters,
				ResultFormat:     r.ResultFormat,
				SecretArn:        r.SecretArn,
			},
		)
	}

	return out, nil
}

package rdsdata

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/rdsdata"
	"github.com/amjadjibon/tinystack/internal/common"
)

type executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func setSchema(ctx context.Context, q executor, driver, schema string) error {
	if schema == "" {
		return nil
	}

	query := `SET search_path TO "` + strings.ReplaceAll(schema, `"`, `""`) + `"`
	if driver == driverMySQL {
		query = "USE `" + strings.ReplaceAll(schema, "`", "``") + "`"
	}

	if _, err := q.ExecContext(ctx, query); err != nil {
		return sqlError(err)
	}

	return nil
}

func (s *Service) ExecuteStatement(
	ctx context.Context,
	in *api.ExecuteStatementRequest,
) (*api.ExecuteStatementResponse, error) {
	return s.execute(ctx, in)
}

func (s *Service) execute(ctx context.Context, in *api.ExecuteStatementRequest) (*api.ExecuteStatementResponse, error) {
	format := common.Value(in.FormatRecordsAs)
	if format != "" && format != "NONE" && format != hintJSON {
		return nil, awserror.New(badRequest, "Unsupported formatRecordsAs")
	}

	if strings.TrimSpace(common.Value(in.Sql)) == "" {
		return nil, awserror.New(badRequest, "SQL is required")
	}

	var (
		q      executor
		driver string
	)
	if in.TransactionId != nil {
		tx, txErr := s.getTransaction(*in.TransactionId, common.Value(in.ResourceArn), common.Value(in.SecretArn))
		if txErr != nil {
			return nil, txErr
		}

		tx.mu.Lock()
		defer tx.mu.Unlock()
		if tx.closed {
			return nil, awserror.New(transactionMissing, "Transaction not found")
		}

		if in.Database != nil && *in.Database != tx.database {
			return nil, awserror.New(badRequest, "Database must match the transaction database")
		}

		driver = tx.driver
		q = tx.tx
		defer func() {
			s.mu.Lock()
			tx.used = time.Now()
			s.mu.Unlock()
		}()
	} else {
		db, d, err := s.connection(
			ctx,
			common.Value(in.ResourceArn),
			common.Value(in.SecretArn),
			common.Value(in.Database),
		)
		if err != nil {
			return nil, err
		}

		defer func() { _ = db.Close() }()
		conn, connErr := db.Conn(ctx)
		if connErr != nil {
			return nil, sqlError(connErr)
		}

		defer func() { _ = conn.Close() }()
		driver = d
		q = conn
	}

	query, args, err := bind(common.Value(in.Sql), driver, in.Parameters)
	if err != nil {
		return nil, err
	}

	if schemaErr := setSchema(ctx, q, driver, common.Value(in.Schema)); schemaErr != nil {
		return nil, schemaErr
	}

	out := &api.ExecuteStatementResponse{NumberOfRecordsUpdated: new(int64(0))}
	words := sqlWords(query)
	returnsRows := false
	if len(words) > 0 {
		switch words[0] {
		case "SELECT", "SHOW", "EXPLAIN", "VALUES", "TABLE":
			returnsRows = true
		case "WITH":
			returnsRows = slices.Contains(words, "SELECT") && !slices.Contains(words, "INSERT") &&
				!slices.Contains(words, "UPDATE") &&
				!slices.Contains(words, "DELETE")
		}
	}

	returnsRows = returnsRows || slices.Contains(words, "RETURNING")
	if !returnsRows {
		result, resultErr := q.ExecContext(ctx, query, args...)
		if resultErr != nil {
			return nil, sqlError(resultErr)
		}

		count, countErr := result.RowsAffected()
		if countErr != nil {
			return nil, sqlError(countErr)
		}

		out.NumberOfRecordsUpdated = new(count)
		if driver == driverMySQL {
			id, idErr := result.LastInsertId()
			if idErr == nil && id > 0 {
				out.GeneratedFields = []api.Field{{LongValue: new(id)}}
			}
		}

		return out, nil
	}

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, sqlError(err)
	}

	defer func() { _ = rows.Close() }()
	columns, err := rows.ColumnTypes()
	if err != nil {
		return nil, sqlError(err)
	}

	if common.Value(in.IncludeResultMetadata) && format != hintJSON {
		for _, column := range columns {
			out.ColumnMetadata = append(out.ColumnMetadata, metadata(column))
		}
	}

	var formatted []map[string]any
	for rows.Next() {
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}

		if err := rows.Scan(targets...); err != nil {
			return nil, sqlError(err)
		}

		fields := make([]api.Field, len(columns))
		object := map[string]any{}
		for i, value := range values {
			field, err := resultField(value, columns[i].DatabaseTypeName())
			if err != nil {
				return nil, err
			}

			fields[i] = field
			if format == hintJSON {
				if _, exists := object[columns[i].Name()]; exists {
					return nil, awserror.New(badRequest, "JSON result column labels must be unique")
				}

				object[columns[i].Name()] = jsonField(field)
			}
		}

		if format == hintJSON {
			formatted = append(formatted, object)
		} else {
			out.Records = append(out.Records, fields)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, sqlError(err)
	}

	if format == hintJSON {
		if formatted == nil {
			formatted = []map[string]any{}
		}

		data, err := json.Marshal(formatted)
		if err != nil {
			return nil, sqlError(err)
		}

		// Enforce the 1 MiB response size limit by measuring the JSON-encoded string length.
		if len(data) > maxResponseBytes {
			return nil, awserror.New(badRequest, "Database returned more than the allowed response size limit")
		}

		out.FormattedRecords = new(string(data))
	} else if len(out.Records) > 0 {
		// Enforce the 1 MiB response size limit by measuring the JSON-encoded size of the returned records.
		data, err := json.Marshal(out.Records)
		if err != nil {
			return nil, sqlError(err)
		}

		if len(data) > maxResponseBytes {
			return nil, awserror.New(badRequest, "Database returned more than the allowed response size limit")
		}
	}

	return out, nil
}

func jsonField(f api.Field) any {
	switch {
	case f.StringValue != nil:
		return *f.StringValue
	case f.LongValue != nil:
		return *f.LongValue
	case f.DoubleValue != nil:
		return *f.DoubleValue
	case f.BooleanValue != nil:
		return *f.BooleanValue
	case f.BlobValue != nil:
		return base64.StdEncoding.EncodeToString(*f.BlobValue)
	default:
		return nil
	}
}

func (s *Service) BatchExecuteStatement(
	ctx context.Context,
	in *api.BatchExecuteStatementRequest,
) (*api.BatchExecuteStatementResponse, error) {
	sets := in.ParameterSets
	if len(sets) == 0 {
		sets = [][]api.SqlParameter{nil}
	}

	out := &api.BatchExecuteStatementResponse{}
	for _, parameters := range sets {
		result, err := s.execute(
			ctx,
			&api.ExecuteStatementRequest{
				ResourceArn:   in.ResourceArn,
				SecretArn:     in.SecretArn,
				Database:      in.Database,
				Schema:        in.Schema,
				Sql:           in.Sql,
				Parameters:    parameters,
				TransactionId: in.TransactionId,
			},
		)
		if err != nil {
			return nil, err
		}

		out.UpdateResults = append(out.UpdateResults, api.UpdateResult{GeneratedFields: result.GeneratedFields})
	}

	return out, nil
}

package athena

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/athena"
	glueapi "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const maxQuery = 262144

func accountOf(ctx context.Context) string {
	scope, _ := request.RegionalScope(ctx)
	return scope.AccountID
}

// execution is a query execution's record; Text marks a DDL statement's
// result, which Athena writes as text without a header row.
type execution struct {
	Execution api.QueryExecution
	Text      bool
	Enforced  bool
}

func loadExecution(tx storage.ReadTx, scope request.Scope, id string) (*execution, error) {
	e, ok, err := storage.Read[execution](tx, scope.Key(service, kindQuery, id))
	if err != nil || !ok {
		return nil, common.ErrorsOr(err, invalid("QueryExecution %s was not found", id))
	}

	return e, nil
}

// resultConfiguration is where a query's results go: the workgroup's when it
// enforces its configuration or the request names none.
func resultConfiguration(w *workGroup, in *api.ResultConfiguration) (*api.ResultConfiguration, error) {
	c := w.WorkGroup.Configuration
	group := common.OrElse(c.ResultConfiguration, &api.ResultConfiguration{})
	out := common.Clone(group)
	if !common.Value(c.EnforceWorkGroupConfiguration) && in != nil {
		out.OutputLocation = common.OrElse(in.OutputLocation, group.OutputLocation)
		out.EncryptionConfiguration = common.OrElse(in.EncryptionConfiguration, group.EncryptionConfiguration)
		out.ExpectedBucketOwner = common.OrElse(in.ExpectedBucketOwner, group.ExpectedBucketOwner)
	}

	if common.Value(out.OutputLocation) == "" {
		return nil, invalid("No output location provided. An output location is required either through the " +
			"Workgroup result configuration setting or as an API input.")
	}

	return out, nil
}

func (s *Service) StartQueryExecution(
	ctx context.Context,
	in *api.StartQueryExecutionInput,
) (*api.StartQueryExecutionOutput, error) {
	query := common.Value(in.QueryString)
	if query == "" || len(query) > maxQuery {
		return nil, invalid("QueryString must be 1 to %d characters long.", maxQuery)
	}

	qc := common.OrElse(common.Clone(in.QueryExecutionContext), &api.QueryExecutionContext{})
	qc.Database = new(strings.ToLower(common.Value(common.OrElse(qc.Database, new(defaultDB)))))
	if qc.Catalog != nil {
		if err := checkCatalog(qc.Catalog); err != nil {
			return nil, err
		}
	}

	group := common.Value(common.OrElse(in.WorkGroup, new(primary)))
	submitted := s.now()
	var id string
	var e *execution
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		w, err := loadWorkGroup(tx, scope, group)
		if err != nil {
			return err
		}

		if common.Value(w.WorkGroup.State) == "DISABLED" {
			return invalid("WorkGroup %s is disabled.", group)
		}

		rc, err := resultConfiguration(w, in.ResultConfiguration)
		if err != nil {
			return err
		}

		id, err = idempotent(tx, scope, in.ClientRequestToken, func(id string) error {
			e = &execution{
				Enforced: common.Value(w.WorkGroup.Configuration.EnforceWorkGroupConfiguration),
				Execution: api.QueryExecution{
					QueryExecutionId:      new(id),
					Query:                 in.QueryString,
					StatementType:         new(statementDML),
					ResultConfiguration:   rc,
					QueryExecutionContext: qc,
					WorkGroup:             new(group),
					ExecutionParameters:   in.ExecutionParameters,
					EngineVersion:         w.WorkGroup.Configuration.EngineVersion,
					Status: &api.QueryExecutionStatus{
						State:              new("RUNNING"),
						SubmissionDateTime: new(common.Unix(submitted)),
					},
				},
			}

			return storage.Write(tx, scope.Key(service, kindQuery, id), e)
		})
		return err
	})
	if err != nil {
		return nil, err
	}

	if e != nil {
		s.finish(ctx, e, submitted)
	}

	return &api.StartQueryExecutionOutput{QueryExecutionId: new(id)}, nil
}

// finish runs an execution's statement, writes its results to its output
// location, and records how it ended.
func (s *Service) finish(ctx context.Context, e *execution, submitted time.Time) {
	x := &e.Execution
	ctx = context.WithValue(
		ctx,
		writeContextKey{},
		writeContext{id: *x.QueryExecutionId, output: *x.ResultConfiguration.OutputLocation, enforced: e.Enforced},
	)
	rs, kind, scanned, err := s.execute(ctx, *x.QueryExecutionContext.Database, *x.Query, x.ExecutionParameters)
	x.StatementType, e.Text = new(kind), kind != statementDML
	if first := firstWords(mustLex(*x.Query), 1); first != "" {
		x.SubstatementType = new(strings.ToUpper(first))
	}

	if err == nil {
		x.ResultConfiguration.OutputLocation = new(strings.TrimSuffix(*x.ResultConfiguration.OutputLocation, "/") +
			"/" + *x.QueryExecutionId + map[bool]string{true: ".txt", false: ".csv"}[e.Text])
		err = s.writeResult(ctx, *x.ResultConfiguration.OutputLocation, rs, e.Text)
	}

	done := s.now()
	x.Status.CompletionDateTime = new(common.Unix(done))
	elapsed := done.Sub(submitted).Milliseconds()
	x.Statistics = &api.QueryExecutionStatistics{
		EngineExecutionTimeInMillis: new(elapsed), DataScannedInBytes: new(scanned),
		TotalExecutionTimeInMillis: new(elapsed), QueryQueueTimeInMillis: new(int64(0)),
		QueryPlanningTimeInMillis: new(int64(0)), ServiceProcessingTimeInMillis: new(int64(0)),
	}
	x.Status.State = new("SUCCEEDED")
	if err != nil {
		x.Status.State, x.Status.StateChangeReason = new("FAILED"), new(err.Error())
		x.Status.AthenaError = &api.AthenaError{
			ErrorCategory: new(int64(2)), ErrorType: new(int64(1100)), Retryable: new(false),
			ErrorMessage: new(err.Error()),
		}
	}

	// The execution is recorded even when the request that started it ends.
	_ = s.update(context.WithoutCancel(ctx), func(tx storage.WriteTx, scope request.Scope) error {
		if err == nil {
			if writeErr := storage.Write(tx, scope.Key(service, kindResult, *x.QueryExecutionId), rs); writeErr != nil {
				return writeErr
			}
		}

		return storage.Write(tx, scope.Key(service, kindQuery, *x.QueryExecutionId), e)
	})
}

func mustLex(query string) []token {
	tokens, _ := lex(query)
	return tokens
}

// execute runs one statement in database, returning its result, its
// statement type, and the bytes it read.
func (s *Service) execute(ctx context.Context, database, query string, params []string) (
	rs *resultSet, kind string, scanned int64, err error,
) {
	tokens, err := lex(query)
	if err != nil {
		return nil, statementDML, 0, fmt.Errorf("SYNTAX_ERROR: %w", err)
	}

	t := solid(tokens)
	for len(t) > 0 && t[len(t)-1].text == ";" {
		t = t[:len(t)-1]
	}

	if slices.ContainsFunc(t, func(tok token) bool { return tok.text == ";" }) {
		return nil, statementDML, 0, errors.New("SYNTAX_ERROR: Only one sql statement is allowed")
	}

	if len(t) == 0 {
		return nil, statementDML, 0, errors.New("SYNTAX_ERROR: the query is empty")
	}

	end := slices.IndexFunc(tokens, func(tok token) bool { return tok.text == ";" })
	if end >= 0 {
		tokens = tokens[:end]
	}

	if isCTAS(t) {
		rs, scanned, err := s.ctas(ctx, database, t, params)
		return rs, statementDML, scanned, err
	}
	if t[0].is("insert") {
		rs, scanned, err := s.insertInto(ctx, database, t, params)
		return rs, statementDML, scanned, err
	}
	first := strings.ToLower(t[0].text)
	switch first {
	case "select", "with", "values", "(":
		rs, scanned, queryErr := s.query(ctx, database, tokens, params)
		return rs, statementDML, scanned, queryErr
	case "create", "drop", "alter", "msck", "show", "describe", "desc":
		rs, kind, ddlErr := s.ddl(ctx, database, tokens)
		return rs, kind, 0, ddlErr
	}

	return nil, statementDML, 0, fmt.Errorf("NOT_SUPPORTED: TinyStack runs SELECT queries and catalog DDL, not %s",
		strings.ToUpper(first))
}

// query runs a SELECT on the tables it names, loaded into an in-memory
// SQLite database with an attached database per Glue database.
func (s *Service) query(ctx context.Context, database string, tokens []token, params []string) (
	*resultSet, int64, error,
) {
	text, err := rewrite(tokens, params)
	if err != nil {
		return nil, 0, fmt.Errorf("SYNTAX_ERROR: %w", err)
	}

	db, err := sql.Open(driverName, "file::memory:")
	if err != nil {
		return nil, 0, err
	}

	defer func() { _ = db.Close() }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, 0, err
	}

	defer func() { _ = conn.Close() }()
	attached := map[string]bool{}
	attach := func(name string) error {
		if attached[name] {
			return nil
		}

		attached[name] = true
		_, attachErr := conn.ExecContext(ctx, "ATTACH DATABASE ':memory:' AS "+quote(name))
		return attachErr
	}
	if attachErr := attach(database); attachErr != nil {
		return nil, 0, attachErr
	}

	var scanned int64
	loaded := map[tableRef]bool{}
	for _, ref := range references(tokens, database) {
		if loaded[ref] {
			continue
		}

		loaded[ref] = true
		t, getErr := s.catalog.GetTable(ctx, &glueapi.GetTableRequest{DatabaseName: &ref.database, Name: &ref.table})
		if notFoundErr(getErr) {
			continue
		}

		if getErr != nil {
			return nil, 0, getErr
		}

		if attachErr := attach(ref.database); attachErr != nil {
			return nil, 0, attachErr
		}

		n, loadErr := s.load(ctx, conn, t.Table)
		if loadErr != nil {
			return nil, 0, loadErr
		}

		scanned += n
	}

	rows, err := conn.QueryContext(ctx, text)
	if err != nil {
		return nil, 0, engineError(err)
	}

	defer func() { _ = rows.Close() }()
	rs, err := collect(rows)
	if err != nil {
		return nil, 0, engineError(err)
	}

	return rs, scanned, nil
}

// engineError words SQLite's errors as Athena's.
func engineError(err error) error {
	msg := err.Error()
	if i := strings.Index(msg, "no such table: "); i >= 0 {
		return fmt.Errorf("TABLE_NOT_FOUND: Table '%s' does not exist", strings.TrimSuffix(msg[i+15:], " (1)"))
	}

	return fmt.Errorf("SYNTAX_ERROR: %s", strings.TrimPrefix(msg, "SQL logic error: "))
}

// writeResult writes a result to S3 as Athena does: CSV with a header row
// and every value quoted (NULL as an empty field), or, for DDL, lines of
// tab-separated values.
func (s *Service) writeResult(ctx context.Context, uri string, rs *resultSet, text bool) error {
	bucket, key, err := location(uri)
	if err != nil {
		return err
	}

	key = strings.TrimSuffix(key, "/")
	var b bytes.Buffer
	if text {
		for _, row := range rs.Rows {
			fields := make([]string, len(row))
			for i, v := range row {
				fields[i] = common.Value(v)
			}

			b.WriteString(strings.Join(fields, "\t") + "\n")
		}
	} else {
		header := make([]*string, len(rs.Columns))
		for i, c := range rs.Columns {
			header[i] = new(c.Name)
		}

		for _, row := range append([][]*string{header}, rs.Rows...) {
			fields := make([]string, len(row))
			for i, v := range row {
				if v != nil {
					fields[i] = `"` + strings.ReplaceAll(*v, `"`, `""`) + `"`
				}
			}

			b.WriteString(strings.Join(fields, ",") + "\n")
		}
	}

	contentType := map[bool]string{true: "text/plain", false: "text/csv"}[text]
	if _, err = s.objects.PutObject(ctx, &s3api.PutObjectRequest{
		Bucket: &bucket, Key: &key, Body: b.Bytes(), ContentType: &contentType,
	}); err != nil {
		return fmt.Errorf("unable to write the query results to %s: %w", uri, err)
	}

	return nil
}

func (s *Service) GetQueryExecution(
	ctx context.Context,
	in *api.GetQueryExecutionInput,
) (*api.GetQueryExecutionOutput, error) {
	out := &api.GetQueryExecutionOutput{}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		e, err := loadExecution(tx, scope, common.Value(in.QueryExecutionId))
		if err == nil {
			out.QueryExecution = &e.Execution
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) BatchGetQueryExecution(
	ctx context.Context,
	in *api.BatchGetQueryExecutionInput,
) (*api.BatchGetQueryExecutionOutput, error) {
	if len(in.QueryExecutionIds) == 0 || len(in.QueryExecutionIds) > 50 {
		return nil, invalid("QueryExecutionIds must have 1 to 50 IDs.")
	}

	out := &api.BatchGetQueryExecutionOutput{
		QueryExecutions: []api.QueryExecution{}, UnprocessedQueryExecutionIds: []api.UnprocessedQueryExecutionId{},
	}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		for _, id := range in.QueryExecutionIds {
			e, found, err := storage.Read[execution](tx, scope.Key(service, kindQuery, id))
			switch {
			case err != nil:
				return err
			case found:
				out.QueryExecutions = append(out.QueryExecutions, e.Execution)
			default:
				out.UnprocessedQueryExecutionIds = append(out.UnprocessedQueryExecutionIds,
					api.UnprocessedQueryExecutionId{
						QueryExecutionId: new(id), ErrorCode: new("INVALID_INPUT"),
						ErrorMessage: new("QueryExecution not found."),
					})
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) ListQueryExecutions(
	ctx context.Context,
	in *api.ListQueryExecutionsInput,
) (*api.ListQueryExecutionsOutput, error) {
	group := common.Value(common.OrElse(in.WorkGroup, new(primary)))
	var all []*execution
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		if _, err := loadWorkGroup(tx, scope, group); err != nil {
			return err
		}

		executions, err := storage.Records[execution](tx, scope.Prefix(service, kindQuery))
		for _, e := range executions {
			if common.Value(e.Execution.WorkGroup) == group {
				all = append(all, e)
			}
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	// The newest come first.
	slices.SortStableFunc(all, func(a, b *execution) int {
		return int(common.Value(b.Execution.Status.SubmissionDateTime)*1e6 -
			common.Value(a.Execution.Status.SubmissionDateTime)*1e6)
	})
	ids := make([]string, len(all))
	for i, e := range all {
		ids[i] = fmt.Sprintf("%08d %s", i, *e.Execution.QueryExecutionId)
	}

	selected, next, err := page(ids, func(id string) string { return id }, in.NextToken, in.MaxResults, 50, 50)
	if err != nil {
		return nil, err
	}

	out := &api.ListQueryExecutionsOutput{QueryExecutionIds: []string{}, NextToken: next}
	for _, id := range selected {
		_, execID, _ := strings.Cut(id, " ")
		out.QueryExecutionIds = append(out.QueryExecutionIds, execID)
	}

	return out, nil
}

func (s *Service) StopQueryExecution(
	ctx context.Context,
	in *api.StopQueryExecutionInput,
) (*api.StopQueryExecutionOutput, error) {
	// Queries finish before StartQueryExecution returns, so there is nothing
	// left to stop.
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		_, err := loadExecution(tx, scope, common.Value(in.QueryExecutionId))
		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.StopQueryExecutionOutput{}, nil
}

func (s *Service) GetQueryResults(
	ctx context.Context,
	in *api.GetQueryResultsInput,
) (*api.GetQueryResultsOutput, error) {
	var e *execution
	rs := &resultSet{}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		if e, err = loadExecution(tx, scope, common.Value(in.QueryExecutionId)); err != nil {
			return err
		}

		if state := common.Value(e.Execution.Status.State); state != "SUCCEEDED" {
			return invalid("Query did not finish successfully. Final query state: %s", state)
		}

		stored, _, err := storage.Read[resultSet](tx, scope.Key(service, kindResult, *in.QueryExecutionId))
		if stored != nil {
			rs = stored
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	rows := rs.Rows
	if !e.Text {
		header := make([]*string, len(rs.Columns))
		for i, c := range rs.Columns {
			header[i] = new(c.Name)
		}

		rows = append([][]*string{header}, rows...)
	}

	index := make([]string, len(rows))
	for i := range rows {
		index[i] = fmt.Sprintf("%010d", i)
	}

	selected, next, err := page(index, func(id string) string { return id }, in.NextToken, in.MaxResults, 1000, 1000)
	if err != nil {
		return nil, err
	}

	out := &api.GetQueryResultsOutput{UpdateCount: new(rs.UpdateCount), NextToken: next, ResultSet: &api.ResultSet{
		Rows: []api.Row{}, ResultSetMetadata: &api.ResultSetMetadata{ColumnInfo: []api.ColumnInfo{}},
	}}
	for _, c := range rs.Columns {
		out.ResultSet.ResultSetMetadata.ColumnInfo = append(out.ResultSet.ResultSetMetadata.ColumnInfo,
			api.ColumnInfo{
				CatalogName: new("hive"), SchemaName: new(""), TableName: new(""), Name: new(c.Name),
				Label: new(c.Name), Type: new(c.Type), Precision: new(int64(0)), Scale: new(int64(0)),
				Nullable: new("UNKNOWN"), CaseSensitive: new(c.Type == typeVarchar),
			})
	}

	for _, i := range selected {
		n, _ := strconv.Atoi(i)
		row := api.Row{Data: []api.Datum{}}
		for _, v := range rows[n] {
			row.Data = append(row.Data, api.Datum{VarCharValue: v})
		}

		out.ResultSet.Rows = append(out.ResultSet.Rows, row)
	}

	return out, nil
}

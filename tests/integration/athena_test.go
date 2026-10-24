package integration_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	attypes "github.com/aws/aws-sdk-go-v2/service/athena/types"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func athenaClient(s *httptest.Server) *athena.Client {
	return athena.New(athena.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

func glueClient(s *httptest.Server) *glue.Client {
	return glue.New(glue.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

// athenaRun starts a query in the lake database and returns its execution.
func athenaRun(t *testing.T, c *athena.Client, in *athena.StartQueryExecutionInput) *attypes.QueryExecution {
	t.Helper()
	ctx := context.Background()
	if in.QueryExecutionContext == nil {
		in.QueryExecutionContext = &attypes.QueryExecutionContext{Database: aws.String("lake")}
	}

	started, err := c.StartQueryExecution(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.GetQueryExecution(ctx, &athena.GetQueryExecutionInput{QueryExecutionId: started.QueryExecutionId})
	if err != nil {
		t.Fatal(err)
	}

	return got.QueryExecution
}

// athenaRows runs a query that must succeed and returns its rows, header
// first for a SELECT, with NULL as "NULL".
func athenaRows(t *testing.T, c *athena.Client, query string, params ...string) [][]string {
	t.Helper()
	in := &athena.StartQueryExecutionInput{
		QueryString: aws.String(query), ExecutionParameters: params,
		ResultConfiguration: &attypes.ResultConfiguration{OutputLocation: aws.String("s3://results/athena/")},
	}
	e := athenaRun(t, c, in)
	if e.Status.State != attypes.QueryExecutionStateSucceeded {
		t.Fatalf("%s: %s %s", query, e.Status.State, aws.ToString(e.Status.StateChangeReason))
	}

	out, err := c.GetQueryResults(context.Background(), &athena.GetQueryResultsInput{
		QueryExecutionId: e.QueryExecutionId,
	})
	if err != nil {
		t.Fatal(err)
	}

	var rows [][]string
	for _, r := range out.ResultSet.Rows {
		var row []string
		for _, d := range r.Data {
			row = append(row, aws.ToString(aws.String(func() string {
				if d.VarCharValue == nil {
					return "NULL"
				}

				return *d.VarCharValue
			}())))
		}

		rows = append(rows, row)
	}

	return rows
}

func joined(rows [][]string) string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = strings.Join(r, ",")
	}

	return strings.Join(out, "|")
}

func TestAthenaQueriesGlueTables(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	objects := s3Client(s, "test", "us-east-1")
	for _, bucket := range []string{"lake", "results"} {
		if _, err := objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Fatal(err)
		}
	}

	var zipped bytes.Buffer
	w := gzip.NewWriter(&zipped)
	_, _ = w.Write([]byte("id,item,amount\n3,plum,1\n"))
	_ = w.Close()
	for key, body := range map[string][]byte{
		"sales/region=eu/part-0.csv":    []byte("id,item,amount\n1,apple,2.5\n2,pear,4\n"),
		"sales/region=us/part-0.csv.gz": zipped.Bytes(),
		"sales/_SUCCESS":                nil,
		"events/a.json": []byte(`{"id":1,"Kind":"click","meta":{"x":1}}` + "\n" +
			`{"id":2,"kind":"view"}` + "\n"),
		"cols/data.parquet": []byte("PAR1"),
	} {
		if _, err := objects.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String("lake"), Key: aws.String(key), Body: bytes.NewReader(body),
		}); err != nil {
			t.Fatal(err)
		}
	}

	c := athenaClient(s)
	for _, ddl := range []string{
		"CREATE DATABASE IF NOT EXISTS lake COMMENT 'the lake'",
		`CREATE EXTERNAL TABLE sales (id int, item string, amount double COMMENT 'euros')
PARTITIONED BY (region string)
ROW FORMAT DELIMITED FIELDS TERMINATED BY ','
LOCATION 's3://lake/sales/'
TBLPROPERTIES ('skip.header.line.count'='1')`,
		"MSCK REPAIR TABLE sales",
		`CREATE EXTERNAL TABLE IF NOT EXISTS lake.events (id bigint, kind string, meta struct<x:int>)
ROW FORMAT SERDE 'org.openx.data.jsonserde.JsonSerDe' LOCATION 's3://lake/events/'`,
		"CREATE EXTERNAL TABLE cols (a int) STORED AS PARQUET LOCATION 's3://lake/cols/'",
	} {
		athenaRows(t, c, ddl)
	}

	rows := athenaRows(
		t,
		c,
		"SELECT region, count(*) AS n, sum(amount) AS total FROM sales GROUP BY region ORDER BY region",
	)
	if got := joined(rows); got != "region,n,total|eu,2,6.5|us,1,1.0" {
		t.Fatalf("grouped = %s", got)
	}

	rows = athenaRows(
		t,
		c,
		`SELECT item FROM awsdatacatalog.lake."sales" WHERE amount > ? AND region = 'eu' ORDER BY item`,
		"2",
	)
	if got := joined(rows); got != "item|apple|pear" {
		t.Fatalf("parameters = %s", got)
	}

	rows = athenaRows(t, c, "SELECT id, kind, json_extract_scalar(meta, '$.x') AS x FROM events ORDER BY id")
	if got := joined(rows); got != "id,kind,x|1,click,1|2,view,NULL" {
		t.Fatalf("JSON = %s", got)
	}

	rows = athenaRows(
		t,
		c,
		"SELECT count(*) AS n FROM sales s JOIN events e ON s.id = e.id WHERE regexp_like(s.item, '^a')",
	)
	if got := joined(rows); got != "n|1" {
		t.Fatalf("join = %s", got)
	}

	rows = athenaRows(t, c, "SHOW TABLES IN lake")
	if got := joined(rows); got != "cols|events|sales" {
		t.Fatalf("SHOW TABLES = %s", got)
	}

	rows = athenaRows(t, c, "SHOW PARTITIONS sales")
	if got := joined(rows); got != "region=eu|region=us" {
		t.Fatalf("SHOW PARTITIONS = %s", got)
	}

	rows = athenaRows(t, c, "DESCRIBE sales")
	if len(rows) != 4 || rows[2][0] != "amount\tdouble\teuros" {
		t.Fatalf("DESCRIBE = %q", rows)
	}

	// The results are in S3 as Athena writes them.
	e := athenaRun(t, c, &athena.StartQueryExecutionInput{
		QueryString:         aws.String("SELECT item, amount FROM sales WHERE id = 1"),
		ResultConfiguration: &attypes.ResultConfiguration{OutputLocation: aws.String("s3://results/athena")},
	})
	if loc := aws.ToString(
		e.ResultConfiguration.OutputLocation,
	); loc != "s3://results/athena/"+aws.ToString(e.QueryExecutionId)+".csv" ||
		e.StatementType != attypes.StatementTypeDml ||
		aws.ToInt64(e.Statistics.DataScannedInBytes) == 0 {
		t.Fatalf("execution = %+v", e)
	}

	obj, err := objects.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("results"), Key: aws.String("athena/" + aws.ToString(e.QueryExecutionId) + ".csv"),
	})
	if err != nil {
		t.Fatal(err)
	}

	body, _ := io.ReadAll(obj.Body)
	if string(body) != "\"item\",\"amount\"\n\"apple\",\"2.5\"\n" {
		t.Fatalf("result object = %q", body)
	}

	// Failures are FAILED executions with Athena's reasons.
	for query, reason := range map[string]string{
		"SELECT * FROM missing":               "TABLE_NOT_FOUND",
		"INSERT INTO sales VALUES (1)":        "TYPE_MISMATCH",
		"SELECT * FROM cols":                  "HIVE_CURSOR_ERROR",
		"SELECT 1; SELECT 2":                  "Only one sql statement",
		"DROP DATABASE lake":                  "not empty",
		"SELECT nosuchfunction(1) FROM sales": "SYNTAX_ERROR",
	} {
		e = athenaRun(t, c, &athena.StartQueryExecutionInput{
			QueryString:         aws.String(query),
			ResultConfiguration: &attypes.ResultConfiguration{OutputLocation: aws.String("s3://results/")},
		})
		if e.Status.State != attypes.QueryExecutionStateFailed ||
			!strings.Contains(aws.ToString(e.Status.StateChangeReason), reason) {
			t.Fatalf("%s: %s %s", query, e.Status.State, aws.ToString(e.Status.StateChangeReason))
		}
	}

	if _, err = c.GetQueryResults(
		ctx,
		&athena.GetQueryResultsInput{QueryExecutionId: e.QueryExecutionId},
	); errorCode(
		err,
	) != "InvalidRequestException" {
		t.Fatalf("results of a failed query: %v", err)
	}

	if _, err = c.StartQueryExecution(
		ctx,
		&athena.StartQueryExecutionInput{QueryString: aws.String("SELECT 1")},
	); errorCode(
		err,
	) != "InvalidRequestException" {
		t.Fatalf("no output location: %v", err)
	}

	// A workgroup's output location serves its queries.
	if _, err = c.CreateWorkGroup(ctx, &athena.CreateWorkGroupInput{
		Name: aws.String("analysts"), Configuration: &attypes.WorkGroupConfiguration{
			ResultConfiguration: &attypes.ResultConfiguration{OutputLocation: aws.String("s3://results/analysts/")},
		},
		Tags: []attypes.Tag{{Key: aws.String("team"), Value: aws.String("data")}},
	}); err != nil {
		t.Fatal(err)
	}

	e = athenaRun(t, c, &athena.StartQueryExecutionInput{
		QueryString: aws.String("SELECT 1 + 1 AS two"), WorkGroup: aws.String("analysts"),
	})
	if e.Status.State != attypes.QueryExecutionStateSucceeded ||
		!strings.HasPrefix(aws.ToString(e.ResultConfiguration.OutputLocation), "s3://results/analysts/") {
		t.Fatalf("workgroup query = %+v %s", e.Status, aws.ToString(e.Status.StateChangeReason))
	}

	listed, err := c.ListQueryExecutions(ctx, &athena.ListQueryExecutionsInput{WorkGroup: aws.String("analysts")})
	if err != nil || len(listed.QueryExecutionIds) != 1 ||
		listed.QueryExecutionIds[0] != aws.ToString(e.QueryExecutionId) {
		t.Fatalf("%+v %v", listed, err)
	}

	named, err := c.CreateNamedQuery(ctx, &athena.CreateNamedQueryInput{
		Name: aws.String(
			"totals",
		),
		Database:           aws.String("lake"),
		QueryString:        aws.String("SELECT sum(amount) FROM sales"),
		WorkGroup:          aws.String("analysts"),
		ClientRequestToken: aws.String("totals-token-0000000000000000000000"),
	})
	if err != nil {
		t.Fatal(err)
	}

	again, err := c.CreateNamedQuery(ctx, &athena.CreateNamedQueryInput{
		Name: aws.String(
			"totals",
		),
		Database:           aws.String("lake"),
		QueryString:        aws.String("SELECT sum(amount) FROM sales"),
		WorkGroup:          aws.String("analysts"),
		ClientRequestToken: aws.String("totals-token-0000000000000000000000"),
	})
	if err != nil || aws.ToString(again.NamedQueryId) != aws.ToString(named.NamedQueryId) {
		t.Fatalf("idempotent named query: %v %v", again, err)
	}

	groups, err := c.ListWorkGroups(ctx, &athena.ListWorkGroupsInput{})
	if err != nil || len(groups.WorkGroups) != 2 {
		t.Fatalf("%+v %v", groups, err)
	}

	// Athena's catalog operations and Glue read the same tables.
	meta, err := c.GetTableMetadata(ctx, &athena.GetTableMetadataInput{
		CatalogName: aws.String("AwsDataCatalog"), DatabaseName: aws.String("lake"), TableName: aws.String("sales"),
	})
	if err != nil || len(meta.TableMetadata.Columns) != 3 || len(meta.TableMetadata.PartitionKeys) != 1 ||
		meta.TableMetadata.Parameters["location"] != "s3://lake/sales/" {
		t.Fatalf("%+v %v", meta, err)
	}

	partitions, err := glueClient(s).GetPartitions(ctx, &glue.GetPartitionsInput{
		DatabaseName: aws.String("lake"), TableName: aws.String("sales"),
	})
	if err != nil || len(partitions.Partitions) != 2 {
		t.Fatalf("%+v %v", partitions, err)
	}

	athenaRows(t, c, "ALTER TABLE sales DROP PARTITION (region = 'us')")
	rows = athenaRows(t, c, "SELECT count(*) AS n FROM sales")
	if got := joined(rows); got != "n|2" {
		t.Fatalf("after dropping a partition = %s", got)
	}

	athenaRows(t, c, "DROP TABLE IF EXISTS cols")
	athenaRows(t, c, "DROP DATABASE lake CASCADE")
	if _, err = glueClient(
		s,
	).GetDatabase(ctx, &glue.GetDatabaseInput{Name: aws.String("lake")}); errorCode(
		err,
	) != "EntityNotFoundException" {
		t.Fatalf("dropped database: %v", err)
	}
}

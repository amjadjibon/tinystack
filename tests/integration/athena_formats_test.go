package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	attypes "github.com/aws/aws-sdk-go-v2/service/athena/types"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	gluetypes "github.com/aws/aws-sdk-go-v2/service/glue/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress"
	"github.com/parquet-go/parquet-go/compress/gzip"
	"github.com/parquet-go/parquet-go/compress/snappy"
	"github.com/parquet-go/parquet-go/compress/uncompressed"
	"github.com/parquet-go/parquet-go/compress/zstd"
)

func athenafmtRun(t *testing.T, c *athena.Client, query string) *attypes.QueryExecution {
	t.Helper()
	return athenaRun(
		t,
		c,
		&athena.StartQueryExecutionInput{
			QueryString:         new(query),
			ResultConfiguration: &attypes.ResultConfiguration{OutputLocation: new("s3://results/athena/")},
		},
	)
}

func athenafmtFail(t *testing.T, c *athena.Client, query, reason string) {
	t.Helper()
	e := athenafmtRun(t, c, query)
	if e.Status.State != attypes.QueryExecutionStateFailed ||
		!strings.Contains(aws.ToString(e.Status.StateChangeReason), reason) {
		t.Fatalf("%s: %s %s", query, e.Status.State, aws.ToString(e.Status.StateChangeReason))
	}
}

func TestAthenaFormatsAndWrites(t *testing.T) {
	srv := server(t)
	ctx := context.Background()
	s3c := s3Client(srv, "test", "us-east-1")
	c := athenaClient(srv)
	for _, name := range []string{"lake", "results"} {
		if _, err := s3c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new(name)}); err != nil {
			t.Fatal(err)
		}
	}

	athenaRows(t, c, "CREATE DATABASE lake")
	type record struct {
		Name    *string `parquet:"name,optional"`
		Values  []int32 `parquet:"vals,list"`
		ID      int64   `parquet:"id"`
		Enabled bool    `parquet:"enabled"`
	}

	codecs := []compress.Codec{&uncompressed.Codec{}, &snappy.Codec{}, &gzip.Codec{}, &zstd.Codec{}}
	var totalBytes int64
	for i, codec := range codecs {
		var body bytes.Buffer
		if err := parquet.Write(
			&body,
			[]record{{ID: int64(i + 1), Name: new("name"), Enabled: true, Values: []int32{1, 2}}, {ID: int64(i + 10)}},
			parquet.Compression(codec),
		); err != nil {
			t.Fatal(err)
		}

		totalBytes += int64(body.Len())
		if _, err := s3c.PutObject(
			ctx,
			&s3.PutObjectInput{
				Bucket: new("lake"),
				Key:    new(fmt.Sprintf("source/part=p%d/data.parquet", i)),
				Body:   bytes.NewReader(body.Bytes()),
			},
		); err != nil {
			t.Fatal(err)
		}
	}

	athenaRows(
		t,
		c,
		`CREATE EXTERNAL TABLE source (id bigint,name string,enabled boolean,vals array<int>) PARTITIONED BY (part string)
 STORED AS PARQUET LOCATION 's3://lake/source/' TBLPROPERTIES ('projection.enabled'='true')`,
	)
	if got := joined(athenaRows(t, c, "SELECT count(*) AS n FROM source")); got != "n|8" {
		t.Fatal(got)
	}

	e := athenafmtRun(t, c, "SELECT id,name,enabled,vals,part FROM source ORDER BY id")
	if e.Status.State != attypes.QueryExecutionStateSucceeded ||
		aws.ToInt64(e.Statistics.DataScannedInBytes) != totalBytes {
		t.Fatalf("%+v", e)
	}

	for _, format := range []string{"PARQUET", "JSON", "TEXTFILE"} {
		name := strings.ToLower(format)
		athenaRows(
			t,
			c,
			fmt.Sprintf(
				"CREATE TABLE copy_%s WITH (format='%s',external_location='s3://lake/copy_%s/',"+
					"write_compression='GZIP') AS SELECT id,name FROM source",
				name,
				format,
				name,
			),
		)
		if got := joined(athenaRows(t, c, "SELECT count(*) AS n FROM copy_"+name)); got != "n|8" {
			t.Fatal(got)
		}

		athenaRows(t, c, "INSERT INTO copy_"+name+" (id,name) VALUES (100,'added'),(101,NULL)")
		if got := joined(athenaRows(t, c, "SELECT count(*) AS n FROM copy_"+name)); got != "n|10" {
			t.Fatal(got)
		}
	}

	athenaRows(t, c, "CREATE TABLE nested_copy AS SELECT id,vals FROM source")
	nested, err := glueClient(
		srv,
	).GetTable(ctx, &glue.GetTableInput{DatabaseName: new("lake"), Name: new("nested_copy")})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(nested.Table.StorageDescriptor.Columns[1].Type) != "array<int>" {
		t.Fatalf("nested column: %+v", nested.Table.StorageDescriptor.Columns[1])
	}
	if got := joined(athenaRows(t, c, "SELECT vals FROM nested_copy WHERE id=1")); got != "vals|[1,2]" {
		t.Fatal(got)
	}
	empty := athenafmtRun(t, c, "CREATE TABLE empty AS SELECT id,name FROM source WITH NO DATA")
	if empty.Status.State != attypes.QueryExecutionStateSucceeded ||
		aws.ToInt64(empty.Statistics.DataScannedInBytes) != 0 {
		t.Fatalf("empty CTAS: %+v", empty)
	}
	if got := joined(athenaRows(t, c, "SELECT count(*) AS n FROM empty")); got != "n|0" {
		t.Fatal(got)
	}

	athenaRows(
		t,
		c,
		"CREATE TABLE partitioned WITH (partitioned_by=ARRAY['part'],write_compression='SNAPPY') "+
			"AS SELECT id,name,part FROM source",
	)
	athenaRows(t, c, "INSERT INTO partitioned SELECT 200,'new','newpart'")
	athenaRows(t, c, "INSERT INTO partitioned VALUES (201,'more','newpart')")
	if got := joined(athenaRows(t, c, "SELECT count(*) AS n FROM partitioned WHERE part='newpart'")); got != "n|2" {
		t.Fatal(got)
	}

	e = athenafmtRun(t, c, "INSERT INTO partitioned VALUES (202,'count','newpart')")
	result, err := c.GetQueryResults(ctx, &athena.GetQueryResultsInput{QueryExecutionId: e.QueryExecutionId})
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToInt64(result.UpdateCount) != 1 {
		t.Fatal(result)
	}

	athenafmtFail(
		t,
		c,
		"CREATE TABLE occupied WITH (external_location='s3://lake/source/') AS SELECT 1 AS id",
		"HIVE_PATH_ALREADY_EXISTS",
	)
	athenafmtFail(t, c, "CREATE TABLE bucketed WITH (bucket_count=2) AS SELECT 1 AS id", "NOT_SUPPORTED")
	athenafmtFail(
		t,
		c,
		"CREATE TABLE wrong_order WITH (partitioned_by=ARRAY['part']) AS SELECT part,id FROM source",
		"HIVE_COLUMN_ORDER_MISMATCH",
	)
	for _, format := range []string{"ORC", "AVRO", "ICEBERG"} {
		athenafmtFail(t, c, "CREATE TABLE bad_"+format+" WITH (format='"+format+"') AS SELECT 1 AS id", "NOT_SUPPORTED")
	}

	for _, statement := range []string{
		"UPDATE source SET id=1", "DELETE FROM source", "MERGE INTO source USING source ON true",
		"OPTIMIZE source", "VACUUM source",
	} {
		athenafmtFail(t, c, statement, "NOT_SUPPORTED")
	}

	athenaRows(t, c, "CREATE EXTERNAL TABLE orc_table (id int) STORED AS ORC LOCATION 's3://lake/orc/'")
	athenafmtFail(t, c, "SELECT * FROM orc_table", "NOT_SUPPORTED")
}

func TestAthenaFormatsPersistentRestart(t *testing.T) {
	dir := t.TempDir()
	srv, stop := persistent(t, dir, &fakeRuntime{})
	objects := s3Client(srv, "test", "us-east-1")
	for _, bucket := range []string{"lake", "results"} {
		if _, err := objects.CreateBucket(
			context.Background(),
			&s3.CreateBucketInput{Bucket: new(bucket)},
		); err != nil {
			t.Fatal(err)
		}
	}

	c := athenaClient(srv)
	athenaRows(t, c, "CREATE DATABASE lake")
	athenaRows(t, c, "CREATE TABLE durable AS SELECT 42 AS id, 'saved' AS name")
	athenaRows(t, c, "INSERT INTO durable VALUES (43,'appended')")
	stop()
	srv, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	c = athenaClient(srv)
	if got := joined(
		athenaRows(t, c, "SELECT id,name FROM durable ORDER BY id"),
	); got != "id,name|42,saved|43,appended" {
		t.Fatal(got)
	}
}

func TestAthenaFormatsUnsupportedTables(t *testing.T) {
	srv := server(t)
	ctx := context.Background()
	objects := s3Client(srv, "test", "us-east-1")
	c := athenaClient(srv)
	for _, bucket := range []string{"lake", "results"} {
		if _, err := objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new(bucket)}); err != nil {
			t.Fatal(err)
		}
	}

	athenaRows(t, c, "CREATE DATABASE lake")
	athenaRows(
		t,
		c,
		`CREATE EXTERNAL TABLE avro_table (id int) ROW FORMAT SERDE 'org.apache.hadoop.hive.serde2.avro.AvroSerDe'
 LOCATION 's3://lake/avro/'`,
	)
	athenafmtFail(t, c, "SELECT * FROM avro_table", "NOT_SUPPORTED")
	gluec := glueClient(srv)
	_, err := gluec.CreateTable(
		ctx,
		&glue.CreateTableInput{
			DatabaseName: new("lake"),
			TableInput: &gluetypes.TableInput{
				Name:       new("iceberg_table"),
				TableType:  new("EXTERNAL_TABLE"),
				Parameters: map[string]string{"table_type": "ICEBERG"},
				StorageDescriptor: &gluetypes.StorageDescriptor{
					Location:    new("s3://lake/iceberg/"),
					InputFormat: new("org.apache.hadoop.hive.ql.io.parquet.MapredParquetInputFormat"),
					Columns:     []gluetypes.Column{{Name: new("id"), Type: new("int")}},
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	athenafmtFail(t, c, "SELECT * FROM iceberg_table", "NOT_SUPPORTED")
	athenafmtFail(t, c, "INSERT INTO iceberg_table VALUES (1)", "NOT_SUPPORTED")
}

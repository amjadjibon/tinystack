package integration_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/rdsdata"
	datatypes "github.com/aws/aws-sdk-go-v2/service/rdsdata/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	rdsapi "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	dataapi "github.com/amjadjibon/tinystack/internal/aws/generated/rdsdata"
	secretapi "github.com/amjadjibon/tinystack/internal/aws/generated/secretsmanager"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/gateway"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	rdssvc "github.com/amjadjibon/tinystack/internal/services/rds"
	rdsdocker "github.com/amjadjibon/tinystack/internal/services/rds/docker"
	datasvc "github.com/amjadjibon/tinystack/internal/services/rdsdata"
	secretsvc "github.com/amjadjibon/tinystack/internal/services/secretsmanager"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

// Discovery is intentionally empty: a test may clean up only its own resources.
type isolatedRDSRuntime struct{ *rdsdocker.Runtime }

func (isolatedRDSRuntime) Resources(context.Context) ([]*rdssvc.Spec, error) { return nil, nil }

func auroraDockerServer(t *testing.T) *httptest.Server {
	t.Helper()
	store := memory.New(nil, nil)
	clusters := rdssvc.New(store)
	data := datasvc.New(clusters)
	secrets := secretsvc.New(store, nil, nil, nil)
	services := []catalog.Service{rdsapi.Descriptor(clusters), dataapi.Descriptor(data), secretapi.Descriptor(secrets)}
	caller := catalog.NewCaller(services...)
	clusters.UseCaller(caller)
	data.UseCaller(caller)
	if err := clusters.UseRuntime(isolatedRDSRuntime{rdsdocker.New()}); err != nil {
		t.Fatal(err)
	}

	h, err := gateway.New(config.Default(), slog.New(slog.NewTextHandler(io.Discard, nil)), services...)
	if err != nil {
		t.Fatal(err)
	}

	s := httptest.NewServer(h)
	t.Cleanup(func() { data.Stop(); clusters.Reset(context.Background()); clusters.Stop(); secrets.Stop(); s.Close() })
	return s
}

func TestRDSDataDocker(t *testing.T) {
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1 for live Aurora engines")
	}

	checkCtx, checkCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err := dockercli.Run(checkCtx, nil, "info")
	checkCancel()
	if err != nil {
		t.Skipf("Docker daemon unavailable: %v", err)
	}

	for _, engine := range []string{"aurora-postgresql", "aurora-mysql"} {
		t.Run(engine, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			s := auroraDockerServer(t)
			c := rdsClient(s, "test", "us-east-1")
			data := rdsDataClient(s)
			id := fmt.Sprintf("data-%d", time.Now().UnixNano())
			in := auroraInput(id, engine)
			in.MasterUserPassword = nil
			in.ManageMasterUserPassword = new(true)
			_, err := c.CreateDBCluster(ctx, in)
			if err != nil {
				t.Fatal(err)
			}

			cluster := waitAurora(t, ctx, c, id)
			if aws.ToString(cluster.Endpoint) != "127.0.0.1" || aws.ToString(cluster.ReaderEndpoint) != "127.0.0.1" ||
				aws.ToInt32(cluster.Port) == 0 {
				t.Fatal(cluster)
			}

			_, err = c.CreateDBInstance(
				ctx,
				&rds.CreateDBInstanceInput{
					DBInstanceIdentifier: new(id + "-member"),
					DBClusterIdentifier:  new(id),
					Engine:               new(engine),
					DBInstanceClass:      new("db.serverless"),
				},
			)
			if err != nil {
				t.Fatal(err)
			}

			arn, secret := cluster.DBClusterArn, cluster.MasterUserSecret.SecretArn
			execute := func(
				query string, parameters []datatypes.SqlParameter, tx *string, metadata bool,
				format datatypes.RecordsFormatType,
			) *rdsdata.ExecuteStatementOutput {
				t.Helper()
				out, statementErr := data.ExecuteStatement(
					ctx,
					&rdsdata.ExecuteStatementInput{
						ResourceArn:           arn,
						SecretArn:             secret,
						Database:              new("testdb"),
						Sql:                   new(query),
						Parameters:            parameters,
						TransactionId:         tx,
						IncludeResultMetadata: metadata,
						FormatRecordsAs:       format,
					},
				)
				if statementErr != nil {
					t.Fatalf("%s: %v", query, statementErr)
				}

				return out
			}

			execute(
				"CREATE TABLE items (id INTEGER PRIMARY KEY, label VARCHAR(100), amount DECIMAL(10,2), enabled BOOLEAN)",
				nil,
				nil,
				false,
				"",
			)
			params := func(id int64, label string) []datatypes.SqlParameter {
				return []datatypes.SqlParameter{
					{Name: new("id"), Value: &datatypes.FieldMemberLongValue{Value: id}},
					{Name: new("label"), Value: &datatypes.FieldMemberStringValue{Value: label}},
					{
						Name:     new("amount"),
						Value:    &datatypes.FieldMemberStringValue{Value: "12.50"},
						TypeHint: datatypes.TypeHintDecimal,
					},
					{Name: new("enabled"), Value: &datatypes.FieldMemberBooleanValue{Value: true}},
				}
			}

			insert := "INSERT INTO items VALUES (:id,:label,:amount,:enabled)"
			out := execute(insert, params(1, "first"), nil, false, "")
			if out.NumberOfRecordsUpdated != 1 {
				t.Fatal(out)
			}

			out = execute("SELECT id,label,amount,enabled FROM items ORDER BY id", nil, nil, true, "")
			if len(out.Records) != 1 || len(out.ColumnMetadata) != 4 {
				t.Fatalf("records: %+v", out)
			}

			if value, ok := out.Records[0][0].(*datatypes.FieldMemberLongValue); !ok || value.Value != 1 {
				t.Fatal(out.Records)
			}

			out = execute("SELECT id,label FROM items ORDER BY id", nil, nil, true, datatypes.RecordsFormatTypeJson)
			if !strings.Contains(aws.ToString(out.FormattedRecords), `"label":"first"`) {
				t.Fatal(out)
			}

			_, err = data.BatchExecuteStatement(
				ctx,
				&rdsdata.BatchExecuteStatementInput{
					ResourceArn:   arn,
					SecretArn:     secret,
					Database:      new("testdb"),
					Sql:           new(insert),
					ParameterSets: [][]datatypes.SqlParameter{params(2, "second"), params(3, "third")},
				},
			)
			if err != nil {
				t.Fatal(err)
			}

			for _, commit := range []bool{true, false} {
				begin, beginErr := data.BeginTransaction(
					ctx,
					&rdsdata.BeginTransactionInput{ResourceArn: arn, SecretArn: secret, Database: new("testdb")},
				)
				if beginErr != nil {
					t.Fatal(beginErr)
				}

				key := int64(4)
				if !commit {
					key = 5
				}

				execute(insert, params(key, "transaction"), begin.TransactionId, false, "")
				if commit {
					_, err = data.CommitTransaction(
						ctx,
						&rdsdata.CommitTransactionInput{
							ResourceArn:   arn,
							SecretArn:     secret,
							TransactionId: begin.TransactionId,
						},
					)
				} else {
					_, err = data.RollbackTransaction(
						ctx,
						&rdsdata.RollbackTransactionInput{
							ResourceArn:   arn,
							SecretArn:     secret,
							TransactionId: begin.TransactionId,
						},
					)
				}

				if err != nil {
					t.Fatal(err)
				}
			}

			out = execute("SELECT COUNT(*) FROM items", nil, nil, false, "")
			if out.Records[0][0].(*datatypes.FieldMemberLongValue).Value != 4 {
				t.Fatal(out)
			}

			_, err = data.ExecuteStatement(
				ctx,
				&rdsdata.ExecuteStatementInput{
					ResourceArn: arn,
					SecretArn:   secret,
					Sql:         new("SELECT * FROM nonexistent_table"),
				},
			)
			code(t, err, "BadRequestException")
			bad, err := secretsClient(
				s,
				"test",
				"us-east-1",
			).CreateSecret(ctx, &secretsmanager.CreateSecretInput{
				Name: new("bad"), SecretString: new(`{"username":"testuser","password":"wrong"}`),
			})
			if err != nil {
				t.Fatal(err)
			}

			_, err = data.ExecuteStatement(
				ctx,
				&rdsdata.ExecuteStatementInput{ResourceArn: arn, SecretArn: bad.ARN, Sql: new("SELECT 1")},
			)
			code(t, err, "ForbiddenException")
			typed := []datatypes.SqlParameter{
				{
					Name:     new("day"),
					Value:    &datatypes.FieldMemberStringValue{Value: "2026-01-02"},
					TypeHint: datatypes.TypeHintDate,
				},
				{
					Name:     new("clock"),
					Value:    &datatypes.FieldMemberStringValue{Value: "12:34:56"},
					TypeHint: datatypes.TypeHintTime,
				},
				{
					Name:     new("stamp"),
					Value:    &datatypes.FieldMemberStringValue{Value: "2026-01-02 12:34:56"},
					TypeHint: datatypes.TypeHintTimestamp,
				},
				{
					Name:     new("uuid"),
					Value:    &datatypes.FieldMemberStringValue{Value: "123e4567-e89b-12d3-a456-426614174000"},
					TypeHint: datatypes.TypeHintUuid,
				},
				{
					Name:     new("json"),
					Value:    &datatypes.FieldMemberStringValue{Value: `{"ok":true}`},
					TypeHint: datatypes.TypeHintJson,
				},
				{Name: new("double"), Value: &datatypes.FieldMemberDoubleValue{Value: 1.25}},
				{Name: new("null"), Value: &datatypes.FieldMemberIsNull{Value: true}},
			}

			out = execute(
				"SELECT :day AS d, :clock AS t, :stamp AS ts, :uuid AS u, :json AS j, :double AS f, CAST(:null AS CHAR) AS n",
				typed,
				nil,
				true,
				"",
			)
			if len(out.Records) != 1 || len(out.Records[0]) != 7 {
				t.Fatal(out)
			}

			binaryType := "BYTEA"
			if engine == "aurora-mysql" {
				binaryType = "VARBINARY(100)"
			}

			execute("CREATE TABLE binary_data (payload "+binaryType+")", nil, nil, false, "")
			execute("INSERT INTO binary_data VALUES (:payload)", []datatypes.SqlParameter{
				{Name: new("payload"), Value: &datatypes.FieldMemberBlobValue{Value: []byte{0, 1, 255}}},
			}, nil, false, "")
			out = execute("SELECT payload FROM binary_data", nil, nil, true, "")
			blob, ok := out.Records[0][0].(*datatypes.FieldMemberBlobValue)
			if !ok || len(blob.Value) != 3 || blob.Value[2] != 255 {
				t.Fatal(out)
			}

			schema := "public"
			if engine == "aurora-mysql" {
				schema = "testdb"
				execute("CREATE TABLE generated_ids (id INTEGER PRIMARY KEY AUTO_INCREMENT)", nil, nil, false, "")
				out = execute("INSERT INTO generated_ids VALUES (NULL)", nil, nil, false, "")
				if len(out.GeneratedFields) != 1 ||
					out.GeneratedFields[0].(*datatypes.FieldMemberLongValue).Value != 1 {
					t.Fatal(out)
				}
			}

			_, err = data.ExecuteStatement(ctx, &rdsdata.ExecuteStatementInput{
				ResourceArn: arn, SecretArn: secret, Database: new("testdb"), Schema: new(schema), Sql: new("SELECT 1"),
			})
			if err != nil {
				t.Fatal(err)
			}

			// A final snapshot exercises the shared runtime's crash-consistent volume copy.
			_, err = c.DeleteDBInstance(
				ctx,
				&rds.DeleteDBInstanceInput{DBInstanceIdentifier: new(id + "-member"), SkipFinalSnapshot: new(true)},
			)
			if err != nil {
				t.Fatal(err)
			}

			_, err = c.DeleteDBCluster(
				ctx,
				&rds.DeleteDBClusterInput{DBClusterIdentifier: new(id), FinalDBSnapshotIdentifier: new(id + "-final")},
			)
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

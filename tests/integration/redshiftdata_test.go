package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	clusters "github.com/aws/aws-sdk-go-v2/service/redshift"
	sdk "github.com/aws/aws-sdk-go-v2/service/redshiftdata"
	"github.com/aws/aws-sdk-go-v2/service/redshiftdata/types"
)

func redshiftDataClient(url string) *sdk.Client {
	return sdk.New(sdk.Options{Region: "us-east-1", BaseEndpoint: new(url), Credentials: creds("test")})
}

func putRedshiftFixture(t *testing.T, url, id, body string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url+"/_tinystack/redshift-data/fixtures/"+id, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("fixture: %d", resp.StatusCode)
	}
}

func waitRedshiftStatement(
	t *testing.T,
	c *sdk.Client,
	id *string,
	want types.StatusString,
) *sdk.DescribeStatementOutput {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, err := c.DescribeStatement(context.Background(), &sdk.DescribeStatementInput{Id: id})
		if err != nil {
			t.Fatal(err)
		}

		if out.Status == want {
			return out
		}

		if out.Status == types.StatusStringFailed && want != types.StatusStringFailed {
			t.Fatalf("statement failed: %+v", out)
		}

		time.Sleep(25 * time.Millisecond)
	}

	t.Fatal("statement did not reach " + want)
	return nil
}

func TestRedshiftDataOperations(t *testing.T) {
	server := server(t)
	ctx := context.Background()
	_, w := createServerless(t, server.URL, "datawarehouse")
	c := redshiftDataClient(server.URL)
	putRedshiftFixture(
		t,
		server.URL,
		"select",
		`{"target":{"workgroupName":"datawarehouse"},
"database":"dev",
"contains":"select 'hello'",
"columnMetadata":[{"name":"message",
"typeName":"varchar"}],
"records":[[{"stringValue":"fixture answer"}]]}`,
	)
	in := &sdk.ExecuteStatementInput{
		WorkgroupName: w.WorkgroupName,
		Database:      new("dev"),
		Sql:           new("select :message"),
		Parameters:    []types.SqlParameter{{Name: new("message"), Value: new("hello")}},
		StatementName: new("query-one"),
		ClientToken:   new("one"),
	}

	out, err := c.ExecuteStatement(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	replay, err := c.ExecuteStatement(ctx, in)
	if err != nil || *replay.Id != *out.Id {
		t.Fatal(replay, err)
	}

	changed := *in
	changed.Sql = new("select 2")
	_, err = c.ExecuteStatement(ctx, &changed)
	wantAccessAnalyzerError(t, err, "ValidationException")
	waitRedshiftStatement(t, c, out.Id, types.StatusStringFinished)
	result, err := c.GetStatementResult(ctx, &sdk.GetStatementResultInput{Id: out.Id})
	if err != nil || len(result.Records) != 1 {
		t.Fatal(result, err)
	}

	if field, ok := result.Records[0][0].(*types.FieldMemberStringValue); !ok || field.Value != "fixture answer" {
		t.Fatal(result)
	}

	_, err = c.CancelStatement(ctx, &sdk.CancelStatementInput{Id: out.Id})
	wantAccessAnalyzerError(t, err, "ValidationException")
	csvIn := *in
	csvIn.ClientToken = new("csv")
	csvIn.ResultFormat = types.ResultFormatStringCsv
	csvOut, err := c.ExecuteStatement(ctx, &csvIn)
	if err != nil {
		t.Fatal(err)
	}

	waitRedshiftStatement(t, c, csvOut.Id, types.StatusStringFinished)
	csvResult, err := c.GetStatementResultV2(ctx, &sdk.GetStatementResultV2Input{Id: csvOut.Id})
	if err != nil || len(csvResult.Records) != 1 {
		t.Fatal(csvResult, err)
	}

	_, err = c.GetStatementResult(ctx, &sdk.GetStatementResultInput{Id: csvOut.Id})
	wantAccessAnalyzerError(t, err, "ValidationException")
	cancelled, err := c.ExecuteStatement(
		ctx,
		&sdk.ExecuteStatementInput{WorkgroupName: w.WorkgroupName, Database: new("dev"), Sql: new("select 'hello'")},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CancelStatement(ctx, &sdk.CancelStatementInput{Id: cancelled.Id})
	if err != nil {
		t.Fatal(err)
	}

	waitRedshiftStatement(t, c, cancelled.Id, types.StatusStringAborted)
	failed, err := c.ExecuteStatement(
		ctx,
		&sdk.ExecuteStatementInput{WorkgroupName: w.WorkgroupName, Database: new("dev"), Sql: new("select missing")},
	)
	if err != nil {
		t.Fatal(err)
	}

	failure := waitRedshiftStatement(t, c, failed.Id, types.StatusStringFailed)
	if !strings.Contains(*failure.Error, "/_tinystack/redshift-data/fixtures") {
		t.Fatal(failure)
	}

	putRedshiftFixture(t, server.URL, "update", `{"contains":"update table","numberOfRecordsUpdated":3}`)
	updated, err := c.ExecuteStatement(
		ctx,
		&sdk.ExecuteStatementInput{WorkgroupName: w.WorkgroupName, Database: new("dev"), Sql: new("update table")},
	)
	if err != nil {
		t.Fatal(err)
	}

	waitRedshiftStatement(t, c, updated.Id, types.StatusStringFinished)
	_, err = c.GetStatementResult(ctx, &sdk.GetStatementResultInput{Id: updated.Id})
	wantAccessAnalyzerError(t, err, "ValidationException")
	batch, err := c.BatchExecuteStatement(
		ctx,
		&sdk.BatchExecuteStatementInput{
			WorkgroupName: w.WorkgroupName,
			Database:      new("dev"),
			Sqls:          []string{"select 'hello'", "update table"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	b := waitRedshiftStatement(t, c, batch.Id, types.StatusStringFinished)
	if len(b.SubStatements) != 2 {
		t.Fatal(b)
	}

	_, err = c.GetStatementResult(ctx, &sdk.GetStatementResultInput{Id: b.SubStatements[0].Id})
	if err != nil {
		t.Fatal(err)
	}

	badBatch, err := c.BatchExecuteStatement(
		ctx,
		&sdk.BatchExecuteStatementInput{
			WorkgroupName: w.WorkgroupName,
			Database:      new("dev"),
			Sqls:          []string{"select 'hello'", "missing"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	waitRedshiftStatement(t, c, badBatch.Id, types.StatusStringFailed)
	list, err := c.ListStatements(ctx, &sdk.ListStatementsInput{MaxResults: 1, Status: types.StatusStringAll})
	if err != nil || len(list.Statements) != 1 || list.NextToken == nil {
		t.Fatal(list, err)
	}

	_, err = c.ListStatements(ctx, &sdk.ListStatementsInput{NextToken: list.NextToken, MaxResults: 1})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ListStatements(ctx, &sdk.ListStatementsInput{RoleLevel: new(true)})
	wantAccessAnalyzerError(t, err, "ValidationException")

	named, err := c.ListStatements(
		ctx,
		&sdk.ListStatementsInput{StatementName: new("query-"), Status: types.StatusStringFinished},
	)
	if err != nil || len(named.Statements) != 2 {
		t.Fatal(named, err)
	}

	_, err = c.ExecuteStatement(
		ctx,
		&sdk.ExecuteStatementInput{WorkgroupName: new("missing"), Database: new("dev"), Sql: new("select 1")},
	)
	wantAccessAnalyzerError(t, err, "ValidationException")
	_, err = c.ExecuteStatement(
		ctx,
		&sdk.ExecuteStatementInput{
			WorkgroupName: w.WorkgroupName,
			Database:      new("dev"),
			Sql:           new("select 1"),
			SecretArn:     new("arn:aws:secretsmanager:us-east-1:000000000000:secret:missing"),
		},
	)
	wantAccessAnalyzerError(t, err, "ValidationException")
	provisioned := clusters.New(
		clusters.Options{Region: "us-east-1", BaseEndpoint: new(server.URL), Credentials: creds("test")},
	)
	_, err = provisioned.CreateCluster(
		ctx,
		&clusters.CreateClusterInput{
			ClusterIdentifier:  new("provisioned"),
			NodeType:           new("dc2.large"),
			ClusterType:        new("single-node"),
			MasterUsername:     new("admin"),
			MasterUserPassword: new("Password123!"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	putRedshiftFixture(
		t,
		server.URL,
		"cluster",
		`{"target":{"clusterIdentifier":"provisioned"},
"contains":"select 1",
"columnMetadata":[{"name":"one",
"typeName":"int4"}],
"records":[[{"longValue":1}]]}`,
	)
	cluster, err := c.ExecuteStatement(
		ctx,
		&sdk.ExecuteStatementInput{
			ClusterIdentifier: new("provisioned"),
			DbUser:            new("admin"),
			Database:          new("dev"),
			Sql:               new("select 1"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	waitRedshiftStatement(t, c, cluster.Id, types.StatusStringFinished)
	_, err = c.GetStatementResult(ctx, &sdk.GetStatementResultInput{Id: cluster.Id})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRedshiftDataPaginationRestart(t *testing.T) {
	dir := t.TempDir()
	server, stop := persistent(t, dir, &fakeRuntime{})
	_, w := createServerless(t, server.URL, "restartdata")
	c := redshiftDataClient(server.URL)
	rows := make([]string, 1001)
	for i := range rows {
		rows[i] = fmt.Sprintf(`[{"longValue":%d}]`, i)
	}

	putRedshiftFixture(
		t,
		server.URL,
		"many",
		`{"contains":"many","columnMetadata":[{"name":"n"}],"records":[`+strings.Join(rows, ",")+`]}`,
	)
	out, err := c.ExecuteStatement(
		context.Background(),
		&sdk.ExecuteStatementInput{WorkgroupName: w.WorkgroupName, Database: new("dev"), Sql: new("select many")},
	)
	if err != nil {
		t.Fatal(err)
	}

	stop()
	server, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	c = redshiftDataClient(server.URL)
	waitRedshiftStatement(t, c, out.Id, types.StatusStringFinished)
	page, err := c.GetStatementResult(context.Background(), &sdk.GetStatementResultInput{Id: out.Id})
	if err != nil || len(page.Records) != 1000 || page.NextToken == nil {
		t.Fatal(page, err)
	}

	next, err := c.GetStatementResult(
		context.Background(),
		&sdk.GetStatementResultInput{Id: out.Id, NextToken: page.NextToken},
	)
	if err != nil || len(next.Records) != 1 || next.NextToken != nil {
		t.Fatal(next, err)
	}
}

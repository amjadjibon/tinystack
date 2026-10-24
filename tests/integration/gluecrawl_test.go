package integration_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/glue"
	gt "github.com/aws/aws-sdk-go-v2/service/glue/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/parquet-go/parquet-go"

	"github.com/amjadjibon/tinystack/internal/services/lambda/local"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

func gluecrawlSetup(t *testing.T, srv *httptest.Server) *glue.Client {
	t.Helper()
	const trust = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
		`"Principal":{"Service":"glue.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	_, err := iamClient(srv, "test", "us-east-1").CreateRole(
		context.Background(),
		&iam.CreateRoleInput{RoleName: new("gluecrawl"), AssumeRolePolicyDocument: new(trust)},
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range []string{"gluecrawl", "results"} {
		_, err := s3Client(
			srv,
			"test",
			"us-east-1",
		).CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: new(n)})
		if err != nil {
			t.Fatal(err)
		}
	}

	return glueClient(srv)
}

func gluecrawlPut(t *testing.T, srv *httptest.Server, key string, body []byte) {
	t.Helper()
	_, err := s3Client(srv, "test", "us-east-1").PutObject(
		context.Background(),
		&s3.PutObjectInput{Bucket: new("gluecrawl"), Key: new(key), Body: bytes.NewReader(body)},
	)
	if err != nil {
		t.Fatal(err)
	}
}

func gluecrawlWait(t *testing.T, c *glue.Client, n string) *gt.Crawler {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out, err := c.GetCrawler(context.Background(), &glue.GetCrawlerInput{Name: new(n)})
		if err != nil {
			t.Fatal(err)
		}

		if out.Crawler.State == gt.CrawlerStateReady {
			return out.Crawler
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("crawl did not complete")
	return nil
}

func gluecrawlStart(t *testing.T, c *glue.Client, n string) {
	t.Helper()
	_, err := c.StartCrawler(context.Background(), &glue.StartCrawlerInput{Name: new(n)})
	if err != nil {
		t.Fatal(err)
	}

	out := gluecrawlWait(t, c, n)
	if out.LastCrawl == nil || out.LastCrawl.Status != gt.LastCrawlStatusSucceeded {
		t.Fatalf("crawl failed: %+v", out.LastCrawl)
	}
}

func TestGluecrawlFormatsAthena(t *testing.T) {
	srv := server(t)
	c := gluecrawlSetup(t, srv)
	ctx := context.Background()
	bodies := map[string][]byte{
		"csv/year=2025/data.csv": []byte("id,name,active\n1,alice,true\n2,bob,false\n"),
		"tsv/data.tsv":           []byte("id\tname\n3\tcarol\n"),
		"pipe/data.txt":          []byte("id|name\n4|dave\n"),
		"json/data.json":         []byte("{\"id\":5,\"name\":\"eve\",\"nested\":{\"x\":true},\"a\":[1,2]}\n"),
	}

	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	_, _ = zw.Write([]byte("id,name\n6,frank\n"))
	_ = zw.Close()
	bodies["gzip/data.csv.gz"] = compressed.Bytes()
	type row struct {
		Name string `parquet:"name"`
		ID   int64  `parquet:"id"`
	}

	var pq bytes.Buffer
	if err := parquet.Write(&pq, []row{{Name: "grace", ID: 7}}); err != nil {
		t.Fatal(err)
	}

	bodies["parquet/data.parquet"] = pq.Bytes()
	for key, body := range bodies {
		gluecrawlPut(t, srv, key, body)
	}

	targets := []gt.S3Target{}
	for _, n := range []string{"csv", "tsv", "pipe", "json", "gzip", "parquet"} {
		targets = append(targets, gt.S3Target{Path: new("s3://gluecrawl/" + n + "/")})
	}

	_, err := c.CreateCrawler(
		ctx,
		&glue.CreateCrawlerInput{
			Name:         new("formats"),
			Role:         new("gluecrawl"),
			DatabaseName: new("lake"),
			Targets:      &gt.CrawlerTargets{S3Targets: targets},
			Tags:         map[string]string{"team": "core"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	gluecrawlStart(t, c, "formats")
	for _, n := range []string{"csv", "tsv", "pipe", "json", "gzip", "parquet"} {
		rows := athenaRows(t, athenaClient(srv), "SELECT id,name FROM "+n+" ORDER BY id")
		if len(rows) < 2 {
			t.Fatalf("%s: %+v", n, rows)
		}
	}

	parts, err := c.GetPartitions(ctx, &glue.GetPartitionsInput{DatabaseName: new("lake"), TableName: new("csv")})
	if err != nil || len(parts.Partitions) != 1 || parts.Partitions[0].Values[0] != "2025" {
		t.Fatalf("partitions %+v %v", parts, err)
	}

	m, err := c.GetCrawlerMetrics(ctx, &glue.GetCrawlerMetricsInput{CrawlerNameList: []string{"formats"}})
	if err != nil || len(m.CrawlerMetricsList) != 1 || m.CrawlerMetricsList[0].TablesCreated != 6 {
		t.Fatalf("metrics %+v %v", m, err)
	}

	h, err := c.ListCrawls(ctx, &glue.ListCrawlsInput{CrawlerName: new("formats")})
	if err != nil || len(h.Crawls) != 1 || string(h.Crawls[0].State) != "SUCCEEDED" {
		t.Fatalf("history %+v %v", h, err)
	}

	list, err := c.ListCrawlers(ctx, &glue.ListCrawlersInput{Tags: map[string]string{"team": "core"}})
	if err != nil || !reflect.DeepEqual(list.CrawlerNames, []string{"formats"}) {
		t.Fatalf("list %+v %v", list, err)
	}
}

func TestGluecrawlClassifiers(t *testing.T) {
	srv := server(t)
	c := gluecrawlSetup(t, srv)
	ctx := context.Background()
	for _, n := range []string{"a", "b"} {
		_, err := c.CreateClassifier(
			ctx,
			&glue.CreateClassifierInput{
				CsvClassifier: &gt.CreateCsvClassifierRequest{
					Name:           new(n),
					Delimiter:      new("|"),
					ContainsHeader: gt.CsvHeaderOptionPresent,
					Header:         []string{"custom_id", "custom_name"},
				},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	got, err := c.GetClassifier(ctx, &glue.GetClassifierInput{Name: new("a")})
	if err != nil || got.Classifier.CsvClassifier.Version != 1 || got.Classifier.CsvClassifier.CreationTime == nil {
		t.Fatalf("classifier %+v %v", got, err)
	}

	_, err = c.UpdateClassifier(
		ctx,
		&glue.UpdateClassifierInput{
			CsvClassifier: &gt.UpdateCsvClassifierRequest{Name: new("a"), QuoteSymbol: new("'")},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	got, err = c.GetClassifier(ctx, &glue.GetClassifierInput{Name: new("a")})
	if err != nil || got.Classifier.CsvClassifier.Version != 2 || got.Classifier.CsvClassifier.LastUpdated == nil {
		t.Fatalf("updated %+v %v", got, err)
	}

	page, err := c.GetClassifiers(ctx, &glue.GetClassifiersInput{MaxResults: new(int32(1))})
	if err != nil || len(page.Classifiers) != 1 || page.NextToken == nil {
		t.Fatalf("page %+v %v", page, err)
	}

	second, err := c.GetClassifiers(
		ctx,
		&glue.GetClassifiersInput{MaxResults: new(int32(1)), NextToken: page.NextToken},
	)
	if err != nil || len(second.Classifiers) != 1 || *second.Classifiers[0].CsvClassifier.Name != "b" {
		t.Fatalf("second %+v %v", second, err)
	}

	_, err = c.CreateClassifier(
		ctx,
		&glue.CreateClassifierInput{
			JsonClassifier: &gt.CreateJsonClassifierRequest{Name: new("json"), JsonPath: new("$.records[*]")},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateClassifier(
		ctx,
		&glue.CreateClassifierInput{
			CsvClassifier: &gt.CreateCsvClassifierRequest{Name: new("bad"), Delimiter: new("too long")},
		},
	)
	if errorCode(err) != "InvalidInputException" {
		t.Fatal(err)
	}

	gluecrawlPut(t, srv, "custom/data.txt", []byte("id|name\n1|alice\n"))
	_, err = c.CreateCrawler(
		ctx,
		&glue.CreateCrawlerInput{
			Name:         new("custom"),
			Role:         new("gluecrawl"),
			DatabaseName: new("lake"),
			Classifiers:  []string{"a", "b"},
			Targets:      &gt.CrawlerTargets{S3Targets: []gt.S3Target{{Path: new("s3://gluecrawl/custom/")}}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	gluecrawlStart(t, c, "custom")
	table, err := c.GetTable(ctx, &glue.GetTableInput{DatabaseName: new("lake"), Name: new("custom")})
	if err != nil || *table.Table.StorageDescriptor.Columns[0].Name != "custom_id" {
		t.Fatalf("custom %+v %v", table, err)
	}

	for _, n := range []string{"a", "b", "json"} {
		if _, deleteErr := c.DeleteClassifier(ctx, &glue.DeleteClassifierInput{Name: new(n)}); deleteErr != nil {
			t.Fatal(deleteErr)
		}
	}

	_, err = c.GetClassifier(ctx, &glue.GetClassifierInput{Name: new("a")})
	if errorCode(err) != "EntityNotFoundException" {
		t.Fatal(fmt.Sprint(err))
	}
}

func TestGluecrawlPoliciesAndErrors(t *testing.T) {
	srv := server(t)
	c := gluecrawlSetup(t, srv)
	ctx := context.Background()
	gluecrawlPut(t, srv, "policy/year=2025/data.csv", []byte("id,name\n1,alice\n"))
	gluecrawlPut(t, srv, "policy/skip.csv", []byte("bad,data\n0,skip\n"))
	input := &glue.CreateCrawlerInput{
		Name:         new("policy"),
		Role:         new("gluecrawl"),
		DatabaseName: new("lake"),
		Targets: &gt.CrawlerTargets{
			S3Targets: []gt.S3Target{{Path: new("s3://gluecrawl/policy/"), Exclusions: []string{"skip.csv"}}},
		},
		SchemaChangePolicy: &gt.SchemaChangePolicy{
			UpdateBehavior: gt.UpdateBehaviorUpdateInDatabase,
			DeleteBehavior: gt.DeleteBehaviorDeleteFromDatabase,
		},
	}

	_, err := c.CreateCrawler(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	gluecrawlStart(t, c, "policy")
	gluecrawlPut(t, srv, "policy/year=2025/data.csv", []byte("id,name,enabled\n1,alice,true\n"))
	gluecrawlStart(t, c, "policy")
	table, err := c.GetTable(ctx, &glue.GetTableInput{DatabaseName: new("lake"), Name: new("policy")})
	if err != nil || len(table.Table.StorageDescriptor.Columns) != 3 {
		t.Fatalf("schema %+v %v", table, err)
	}

	_, err = c.UpdateCrawler(
		ctx,
		&glue.UpdateCrawlerInput{
			Name: new("policy"),
			SchemaChangePolicy: &gt.SchemaChangePolicy{
				UpdateBehavior: gt.UpdateBehaviorLog,
				DeleteBehavior: gt.DeleteBehaviorLog,
			},
			RecrawlPolicy: &gt.RecrawlPolicy{RecrawlBehavior: gt.RecrawlBehaviorCrawlNewFoldersOnly},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	gluecrawlPut(t, srv, "policy/year=2025/data.csv", []byte("id,name\n1,alice\n"))
	gluecrawlPut(t, srv, "policy/year=2026/data.csv", []byte("id,name,enabled\n2,bob,false\n"))
	gluecrawlStart(t, c, "policy")
	parts, err := c.GetPartitions(ctx, &glue.GetPartitionsInput{DatabaseName: new("lake"), TableName: new("policy")})
	if err != nil || len(parts.Partitions) != 2 {
		t.Fatalf("incremental partitions %+v %v", parts, err)
	}

	_, err = c.UpdateCrawler(
		ctx,
		&glue.UpdateCrawlerInput{
			Name:          new("policy"),
			RecrawlPolicy: &gt.RecrawlPolicy{RecrawlBehavior: gt.RecrawlBehaviorCrawlEverything},
			SchemaChangePolicy: &gt.SchemaChangePolicy{
				UpdateBehavior: gt.UpdateBehaviorUpdateInDatabase,
				DeleteBehavior: gt.DeleteBehaviorDeleteFromDatabase,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"policy/year=2025/data.csv", "policy/year=2026/data.csv"} {
		_, deleteErr := s3Client(srv, "test", "us-east-1").DeleteObject(
			ctx,
			&s3.DeleteObjectInput{Bucket: new("gluecrawl"), Key: new(key)},
		)
		if deleteErr != nil {
			t.Fatal(deleteErr)
		}
	}

	gluecrawlStart(t, c, "policy")
	_, err = c.GetTable(ctx, &glue.GetTableInput{DatabaseName: new("lake"), Name: new("policy")})
	code(t, err, "EntityNotFoundException")
	_, err = c.StopCrawler(ctx, &glue.StopCrawlerInput{Name: new("policy")})
	code(t, err, "CrawlerNotRunningException")
	_, err = c.CreateCrawler(
		ctx,
		&glue.CreateCrawlerInput{
			Name:         new("bad"),
			Role:         new("absent"),
			DatabaseName: new("lake"),
			Targets:      input.Targets,
		},
	)
	code(t, err, "InvalidInputException")
	_, err = c.UpdateCrawler(
		ctx,
		&glue.UpdateCrawlerInput{
			Name:          new("policy"),
			Configuration: new(`{"Grouping":{"TableGroupingPolicy":"CombineCompatibleSchemas"}}`),
		},
	)
	code(t, err, "InvalidInputException")
	// Terraform sends unset connection and queue members as empty strings; only set values are unsupported.
	path := input.Targets.S3Targets[0].Path
	_, err = c.UpdateCrawler(
		ctx,
		&glue.UpdateCrawlerInput{
			Name:    new("policy"),
			Targets: &gt.CrawlerTargets{S3Targets: []gt.S3Target{{Path: path, ConnectionName: new("")}}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateCrawler(
		ctx,
		&glue.UpdateCrawlerInput{
			Name:    new("policy"),
			Targets: &gt.CrawlerTargets{S3Targets: []gt.S3Target{{Path: path, EventQueueArn: new("arn:aws:sqs:x")}}},
		},
	)
	code(t, err, "InvalidInputException")
	_, err = c.UpdateCrawlerSchedule(
		ctx,
		&glue.UpdateCrawlerScheduleInput{CrawlerName: new("policy"), Schedule: new("cron(* * * * ? *)")},
	)
	code(t, err, "InvalidInputException")
	_, err = c.UpdateCrawlerSchedule(
		ctx,
		&glue.UpdateCrawlerScheduleInput{CrawlerName: new("policy"), Schedule: new("cron(0/5 * * * ? *)")},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.StopCrawlerSchedule(ctx, &glue.StopCrawlerScheduleInput{CrawlerName: new("policy")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.StartCrawlerSchedule(ctx, &glue.StartCrawlerScheduleInput{CrawlerName: new("policy")})
	if err != nil {
		t.Fatal(err)
	}

	arn := "arn:aws:glue:us-east-1:000000000000:crawler/policy"
	_, err = c.TagResource(
		ctx,
		&glue.TagResourceInput{ResourceArn: new(arn), TagsToAdd: map[string]string{"team": "platform"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.GetTags(ctx, &glue.GetTagsInput{ResourceArn: new(arn)})
	if err != nil || tags.Tags["team"] != "platform" {
		t.Fatalf("tags %+v %v", tags, err)
	}

	_, err = c.UntagResource(ctx, &glue.UntagResourceInput{ResourceArn: new(arn), TagsToRemove: []string{"team"}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteCrawler(ctx, &glue.DeleteCrawlerInput{Name: new("policy")})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGluecrawlWorkflow(t *testing.T) {
	srv := server(t)
	c := gluecrawlSetup(t, srv)
	ctx := context.Background()
	gluecrawlPut(t, srv, "workflow/data.csv", []byte("id,name\n1,alice\n"))
	for _, n := range []string{"first", "second"} {
		_, err := c.CreateCrawler(
			ctx,
			&glue.CreateCrawlerInput{
				Name:         new(n),
				Role:         new("gluecrawl"),
				DatabaseName: new("lake"),
				TablePrefix:  new(n + "_"),
				Targets:      &gt.CrawlerTargets{S3Targets: []gt.S3Target{{Path: new("s3://gluecrawl/workflow/")}}},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err := c.CreateWorkflow(ctx, &glue.CreateWorkflowInput{Name: new("flow")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateTrigger(
		ctx,
		&glue.CreateTriggerInput{
			Name:         new("source"),
			Type:         gt.TriggerTypeOnDemand,
			WorkflowName: new("flow"),
			Actions:      []gt.Action{{CrawlerName: new("first")}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateTrigger(
		ctx,
		&glue.CreateTriggerInput{
			Name:            new("chain"),
			Type:            gt.TriggerTypeConditional,
			WorkflowName:    new("flow"),
			StartOnCreation: true,
			Actions:         []gt.Action{{CrawlerName: new("second")}},
			Predicate: &gt.Predicate{
				Conditions: []gt.Condition{{CrawlerName: new("first"), CrawlState: gt.CrawlStateSucceeded}},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	run, err := c.StartWorkflowRun(ctx, &glue.StartWorkflowRunInput{Name: new("flow")})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "crawler workflow", func() bool {
		out, err := c.GetWorkflowRun(
			ctx,
			&glue.GetWorkflowRunInput{Name: new("flow"), RunId: run.RunId, IncludeGraph: new(true)},
		)
		if err != nil {
			t.Fatal(err)
		}

		if out.Run.Status != gt.WorkflowRunStatusCompleted {
			return false
		}

		if out.Run.Statistics.SucceededActions != 2 || len(out.Run.Graph.Nodes) != 4 || len(out.Run.Graph.Edges) != 3 {
			t.Fatalf("workflow %+v", out.Run)
		}

		return true
	})
}

func TestGluecrawlScheduleStopAndRestart(t *testing.T) {
	ctx := context.Background()
	store := &memory.Store{}
	srv, stop := serverStore(t, testConfig(), local.New(local.Options{}), store)
	defer stop()
	c := gluecrawlSetup(t, srv)
	gluecrawlPut(t, srv, "scheduled/data.csv", []byte("id,name\n1,alice\n"))
	_, err := c.CreateCrawler(
		ctx,
		&glue.CreateCrawlerInput{
			Name:         new("scheduled"),
			Role:         new("gluecrawl"),
			DatabaseName: new("lake"),
			Schedule:     new("cron(0/5 * * * ? *)"),
			Targets:      &gt.CrawlerTargets{S3Targets: []gt.S3Target{{Path: new("s3://gluecrawl/scheduled/")}}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	// Advance the durable deadline rather than waiting five minutes.
	err = store.Update(ctx, func(tx storage.WriteTx) error {
		key := "000000000000/us-east-1/glue/crawler/scheduled"
		r, _, readErr := storage.Read[map[string]json.RawMessage](tx, key)
		if readErr != nil {
			return readErr
		}

		(*r)["Next"] = json.RawMessage("1")
		return storage.Write(tx, key, r)
	})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "crawler schedule", func() bool {
		out, getErr := c.GetCrawler(ctx, &glue.GetCrawlerInput{Name: new("scheduled")})
		return getErr == nil && out.Crawler.LastCrawl != nil &&
			out.Crawler.LastCrawl.Status == gt.LastCrawlStatusSucceeded
	})

	_, err = c.StopCrawlerSchedule(ctx, &glue.StopCrawlerScheduleInput{CrawlerName: new("scheduled")})
	if err != nil {
		t.Fatal(err)
	}

	// A durable unclaimed run remains RUNNING until a worker claims it. Claim it as a previous session
	// to reproduce the exact on-disk state left by a process interruption before catalog commit.
	dir := t.TempDir()
	persistentSrv, closePersistent := gluejobPersistent(t, dir)
	pc := gluecrawlSetup(t, persistentSrv)
	_, err = pc.CreateCrawler(
		ctx,
		&glue.CreateCrawlerInput{
			Name:         new("interrupted"),
			Role:         new("gluecrawl"),
			DatabaseName: new("lake"),
			Targets:      &gt.CrawlerTargets{S3Targets: []gt.S3Target{{Path: new("s3://gluecrawl/empty/")}}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = pc.StartCrawler(ctx, &glue.StartCrawlerInput{Name: new("interrupted")})
	if err != nil {
		t.Fatal(err)
	}

	closePersistent()
	disk, err := sqlite.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	err = disk.Update(ctx, func(tx storage.WriteTx) error {
		key := "000000000000/us-east-1/glue/crawler/interrupted"
		r, _, readErr := storage.Read[map[string]json.RawMessage](tx, key)
		if readErr != nil {
			return readErr
		}

		var crawler map[string]any
		if decodeErr := json.Unmarshal((*r)["Crawler"], &crawler); decodeErr != nil {
			return decodeErr
		}

		crawler["State"] = "RUNNING"
		body, encodeErr := json.Marshal(crawler)
		if encodeErr != nil {
			return encodeErr
		}

		(*r)["Crawler"] = body
		(*r)["Owner"] = json.RawMessage(`"previous-session"`)
		return storage.Write(tx, key, r)
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}

	persistentSrv, closePersistent = gluejobPersistent(t, dir)
	defer closePersistent()
	recovered := gluecrawlWait(t, glueClient(persistentSrv), "interrupted")
	if recovered.LastCrawl == nil || recovered.LastCrawl.Status != gt.LastCrawlStatusFailed ||
		recovered.LastCrawl.ErrorMessage == nil {
		t.Fatalf("restart %+v", recovered.LastCrawl)
	}
}

func TestGluecrawlStop(t *testing.T) {
	srv := server(t)
	c := gluecrawlSetup(t, srv)
	ctx := context.Background()
	gluecrawlPut(t, srv, "stop/data.csv", []byte("id,name\n"+strings.Repeat("1,alice\n", 200000)))
	_, err := c.CreateCrawler(
		ctx,
		&glue.CreateCrawlerInput{
			Name:         new("stop"),
			Role:         new("gluecrawl"),
			DatabaseName: new("lake"),
			Targets:      &gt.CrawlerTargets{S3Targets: []gt.S3Target{{Path: new("s3://gluecrawl/stop/")}}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.StartCrawler(ctx, &glue.StartCrawlerInput{Name: new("stop")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.StartCrawler(ctx, &glue.StartCrawlerInput{Name: new("stop")})
	code(t, err, "CrawlerRunningException")
	_, err = c.DeleteCrawler(ctx, &glue.DeleteCrawlerInput{Name: new("stop")})
	code(t, err, "CrawlerRunningException")
	_, err = c.StopCrawler(ctx, &glue.StopCrawlerInput{Name: new("stop")})
	if err != nil {
		t.Fatal(err)
	}

	stopped := gluecrawlWait(t, c, "stop")
	if stopped.LastCrawl == nil || stopped.LastCrawl.Status != gt.LastCrawlStatusCancelled {
		t.Fatalf("stop %+v", stopped.LastCrawl)
	}

	_, err = c.GetTable(ctx, &glue.GetTableInput{DatabaseName: new("lake"), Name: new("stop")})
	code(t, err, "EntityNotFoundException")
}

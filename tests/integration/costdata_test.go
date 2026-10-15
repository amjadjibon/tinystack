package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	budgets "github.com/aws/aws-sdk-go-v2/service/budgets"
	bt "github.com/aws/aws-sdk-go-v2/service/budgets/types"
	ce "github.com/aws/aws-sdk-go-v2/service/costexplorer"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
)

func fixtureRequest(t *testing.T, s *httptest.Server, method, path, body string, status int) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(
		context.Background(),
		method,
		s.URL+"/_tinystack/"+path,
		strings.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status {
		t.Fatalf("%s %s: status %d: %s", method, path, response.StatusCode, data)
	}
	return data
}

func TestCostFixturesQueries(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := costExplorerClient(s, "test", "eu-west-1")
	body := `{"provenance":"integration","lines":[
 {"id":"charge","start":"2026-10-01","end":"2026-10-02","service":"Amazon Simple Storage Service",
"region":"eu-west-1","tags":{"team":"core"},"unblendedCost":"10.10","currency":"USD","estimated":true},
 {"id":"refund","start":"2026-10-01T12:00:00Z","end":"2026-10-01T13:00:00Z",
"service":"Amazon Simple Storage Service","unblendedCost":"-0.20","currency":"USD"},
 {"id":"compute","start":"2026-10-02","end":"2026-10-03","service":"Amazon Elastic Compute Cloud - Compute",
"usageType":"BoxUsage","unblendedCost":"2","currency":"USD"}]}`
	fixtureRequest(t, s, "PUT", "costs/fixtures/first", body, 200)
	fixtureRequest(
		t,
		s,
		"PUT",
		"costs/fixtures/second",
		`{"provenance":"extra","lines":[{"id":"extra","start":"2026-10-01","end":"2026-10-02",
"service":"Amazon Simple Storage Service","unblendedCost":"0.10","currency":"USD"}]}`,
		200,
	)
	period := &types.DateInterval{Start: new("2026-10-01"), End: new("2026-10-04")}
	query := &ce.GetCostAndUsageInput{
		TimePeriod:  period,
		Granularity: types.GranularityDaily,
		Metrics:     []string{"UnblendedCost"},
	}
	got, err := c.GetCostAndUsage(ctx, query)
	if err != nil || len(got.ResultsByTime) != 3 {
		t.Fatal(got, err)
	}
	if aws.ToString(got.ResultsByTime[0].Total["UnblendedCost"].Amount) != "10" || !got.ResultsByTime[0].Estimated ||
		aws.ToString(got.ResultsByTime[2].Total["UnblendedCost"].Amount) != "0" {
		t.Fatal(got)
	}
	query.Granularity = types.GranularityMonthly
	got, err = c.GetCostAndUsage(ctx, query)
	if err != nil || aws.ToString(got.ResultsByTime[0].Total["UnblendedCost"].Amount) != "12" {
		t.Fatal(got, err)
	}
	query.Filter = &types.Expression{
		And: []types.Expression{
			{Tags: &types.TagValues{Key: new("team"), Values: []string{"core"}}},
			{
				Not: &types.Expression{
					Dimensions: &types.DimensionValues{Key: types.DimensionService, Values: []string{"missing"}},
				},
			},
		},
	}
	query.GroupBy = []types.GroupDefinition{
		{Type: types.GroupDefinitionTypeDimension, Key: new("SERVICE")},
		{Type: types.GroupDefinitionTypeTag, Key: new("team")},
	}
	got, err = c.GetCostAndUsage(ctx, query)
	if err != nil || len(got.ResultsByTime[0].Groups) != 1 || got.ResultsByTime[0].Groups[0].Keys[1] != "team$core" ||
		aws.ToString(got.ResultsByTime[0].Groups[0].Metrics["UnblendedCost"].Amount) != "10.1" {
		t.Fatal(got, err)
	}
	dims, err := c.GetDimensionValues(
		ctx,
		&ce.GetDimensionValuesInput{TimePeriod: period, Dimension: types.DimensionService, MaxResults: new(int32(1))},
	)
	if err != nil || dims.NextPageToken == nil || aws.ToInt32(dims.TotalSize) != 2 {
		t.Fatal(dims, err)
	}
	_, err = c.GetDimensionValues(
		ctx,
		&ce.GetDimensionValuesInput{
			TimePeriod:    period,
			Dimension:     types.DimensionService,
			MaxResults:    new(int32(1)),
			NextPageToken: dims.NextPageToken,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	tags, err := c.GetTags(ctx, &ce.GetTagsInput{TimePeriod: period, TagKey: new("team"), SearchString: new("cor")})
	if err != nil || len(tags.Tags) != 1 || tags.Tags[0] != "core" {
		t.Fatal(tags, err)
	}
	tags, err = c.GetTags(ctx, &ce.GetTagsInput{TimePeriod: period})
	if err != nil || len(tags.Tags) != 1 || tags.Tags[0] != "team" {
		t.Fatal(tags, err)
	}
	wide := &ce.GetCostAndUsageInput{
		TimePeriod:  &types.DateInterval{Start: new("2026-10-01"), End: new("2026-12-01")},
		Granularity: types.GranularityDaily,
		Metrics:     []string{"UnblendedCost"},
	}
	page, err := c.GetCostAndUsage(ctx, wide)
	if err != nil || len(page.ResultsByTime) != 31 || page.NextPageToken == nil {
		t.Fatal(page, err)
	}
	wide.NextPageToken = page.NextPageToken
	page, err = c.GetCostAndUsage(ctx, wide)
	if err != nil || len(page.ResultsByTime) != 30 {
		t.Fatal(page, err)
	}
	fixtureRequest(t, s, "PUT", "costs/fixtures/first", `{"provenance":"replaced","lines":[]}`, 200)
	_, err = c.GetDimensionValues(
		ctx,
		&ce.GetDimensionValuesInput{
			TimePeriod:    period,
			Dimension:     types.DimensionService,
			MaxResults:    new(int32(1)),
			NextPageToken: dims.NextPageToken,
		},
	)
	wantCostExplorerError(t, err, "InvalidNextTokenException")
	query.Filter = nil
	query.GroupBy = nil
	got, err = c.GetCostAndUsage(ctx, query)
	if err != nil || aws.ToString(got.ResultsByTime[0].Total["UnblendedCost"].Amount) != "0.1" {
		t.Fatal(got, err)
	}
	isolated, err := costExplorerClient(s, "111111111111", "us-east-1").GetCostAndUsage(ctx, query)
	if err != nil || aws.ToString(isolated.ResultsByTime[0].Total["UnblendedCost"].Amount) != "0" {
		t.Fatal(isolated, err)
	}
	fixtureRequest(t, s, "POST", "reset", "", 200)
	got, err = c.GetCostAndUsage(ctx, query)
	if err != nil || aws.ToString(got.ResultsByTime[0].Total["UnblendedCost"].Amount) != "0" {
		t.Fatal(got, err)
	}
}

func TestCostFixturesValidation(t *testing.T) {
	s := server(t)
	valid := `{"lines":[{"id":"one","start":"2026-10-01","end":"2026-10-02","service":"S3","unblendedCost":"1",
"currency":"USD"}]}`
	fixtureRequest(t, s, "PUT", "costs/fixtures/valid", valid, 200)
	for _, body := range []string{
		"null", `{"lines":[],"estimated":null}`, `{"lines":[],"unknown":1}`, strings.Replace(valid, `"1"`, `"1e2"`, 1),
		strings.Replace(valid, "USD", "EUR", 1),
		strings.Replace(valid, "2026-10-02", "2026-10-03", 1),
		strings.Replace(valid, `"id":"one"`, `"id":"one","linkedAccount":"111111111111"`, 1),
	} {
		fixtureRequest(t, s, "PUT", "costs/fixtures/valid", body, 400)
	}
	var fixture map[string]any
	if err := json.Unmarshal(fixtureRequest(t, s, "GET", "costs/fixtures/valid", "", 200), &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture["lineCount"] != float64(1) {
		t.Fatal(fixture)
	}
	fixtureRequest(t, s, "GET", "costs/fixtures?account=111111111111", "", 400)
	fixtureRequest(t, s, "GET", "costs/fixtures", "", 200)
	fixtureRequest(t, s, "DELETE", "costs/fixtures/valid", "", 204)
	fixtureRequest(t, s, "GET", "costs/fixtures/valid", "", 404)
}

//nolint:staticcheck // Exercise supported legacy CostFilters; modern FilterExpression remains unsupported.
func TestBudgetFixtureActualSpend(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	client := budgetsClient(s, "test", "ap-south-1")
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Nanosecond)
	body := fmt.Sprintf(
		`{"lines":[{"id":"actual","start":%q,"end":%q,"service":"S3","unblendedCost":"-2.125","currency":"USD"}]}`,
		start.Format(time.RFC3339Nano),
		end.Format(time.RFC3339Nano),
	)
	fixtureRequest(t, s, "PUT", "costs/fixtures/budget", body, 200)
	for _, unit := range []bt.TimeUnit{bt.TimeUnitMonthly, bt.TimeUnitQuarterly, bt.TimeUnitAnnually} {
		name := string(unit)
		_, err := client.CreateBudget(
			ctx,
			&budgets.CreateBudgetInput{
				AccountId: new("000000000000"),
				Budget: &bt.Budget{
					BudgetName:  new(name),
					BudgetType:  bt.BudgetTypeCost,
					TimeUnit:    unit,
					BudgetLimit: &bt.Spend{Amount: new("10"), Unit: new("USD")},
				},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		got, err := client.DescribeBudget(
			ctx,
			&budgets.DescribeBudgetInput{AccountId: new("000000000000"), BudgetName: new(name)},
		)
		if err != nil || got.Budget.CalculatedSpend == nil ||
			aws.ToString(got.Budget.CalculatedSpend.ActualSpend.Amount) != "-2.125" ||
			got.Budget.CalculatedSpend.ForecastedSpend != nil {
			t.Fatal(got, err)
		}
	}
	listed, err := client.DescribeBudgets(ctx, &budgets.DescribeBudgetsInput{AccountId: new("000000000000")})
	if err != nil || len(listed.Budgets) != 3 {
		t.Fatal(listed, err)
	}
	for _, b := range listed.Budgets {
		if b.CalculatedSpend == nil || aws.ToString(b.CalculatedSpend.ActualSpend.Amount) != "-2.125" {
			t.Fatal(b)
		}
	}
	for _, tc := range []struct {
		filters map[string][]string
		actual  *string
		name    string
	}{
		{map[string][]string{"SERVICE": {"S3"}}, new("-2.125"), "filtered"},
		{map[string][]string{"REGION": {"nonexistent"}}, new("0"), "excluded"},
		{map[string][]string{"AZ": {"zone"}}, nil, "unsupported"},
		{map[string][]string{"SERVICE": {}}, nil, "empty-filter"},
	} {
		_, createErr := client.CreateBudget(
			ctx,
			&budgets.CreateBudgetInput{AccountId: new("000000000000"), Budget: &bt.Budget{
				BudgetName: new(tc.name), BudgetType: bt.BudgetTypeCost, TimeUnit: bt.TimeUnitMonthly,
				BudgetLimit: &bt.Spend{Amount: new("10"), Unit: new("USD")}, CostFilters: tc.filters,
			}},
		)
		if createErr != nil {
			t.Fatal(createErr)
		}
		got, describeErr := client.DescribeBudget(
			ctx,
			&budgets.DescribeBudgetInput{AccountId: new("000000000000"), BudgetName: new(tc.name)},
		)
		if describeErr != nil {
			t.Fatal(describeErr)
		}
		if tc.actual == nil {
			if got.Budget.CalculatedSpend != nil {
				t.Fatal(got)
			}
		} else if got.Budget.CalculatedSpend == nil ||
			aws.ToString(got.Budget.CalculatedSpend.ActualSpend.Amount) != *tc.actual {
			t.Fatal(got)
		}
	}
	_, err = client.CreateBudget(ctx, &budgets.CreateBudgetInput{AccountId: new("000000000000"), Budget: &bt.Budget{
		BudgetName: new("planned"), BudgetType: bt.BudgetTypeCost, TimeUnit: bt.TimeUnitMonthly,
		PlannedBudgetLimits: map[string]bt.Spend{"0": {Amount: new("100"), Unit: new("USD")}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	planned, err := client.DescribeBudget(
		ctx,
		&budgets.DescribeBudgetInput{AccountId: new("000000000000"), BudgetName: new("planned")},
	)
	if err != nil || planned.Budget.CalculatedSpend == nil ||
		aws.ToString(planned.Budget.CalculatedSpend.ActualSpend.Amount) != "-2.125" {
		t.Fatal(planned, err)
	}
}

func TestCostFixturesPersistence(t *testing.T) {
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	data := fixtureRequest(
		t,
		s,
		"PUT",
		"costs/fixtures/persisted",
		`{"provenance":"disk","lines":[{"id":"line","start":"2026-10-01","end":"2026-10-02","service":"S3",
"unblendedCost":"0.123456789012","currency":"USD"}]}`,
		200,
	)
	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	restored := fixtureRequest(t, s, "GET", "costs/fixtures/persisted", "", 200)
	if !bytes.Equal(restored, data) {
		t.Fatalf("fixture changed across restart: %s", restored)
	}
	fixtureRequest(t, s, "POST", "reset", "", 200)
	fixtureRequest(t, s, "GET", "costs/fixtures/persisted", "", 404)
}

func TestCostQueriesRejectUnsupported(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	client := costExplorerClient(s, "test", "us-east-1")
	period := &types.DateInterval{Start: new("2026-10-01"), End: new("2026-11-01")}
	for _, filter := range []*types.Expression{
		{Dimensions: &types.DimensionValues{Key: "AZ", Values: []string{"zone"}}},
		{Tags: &types.TagValues{
			Key: new("team"), Values: []string{"core"},
			MatchOptions: []types.MatchOption{types.MatchOptionStartsWith},
		}},
		{CostCategories: &types.CostCategoryValues{Key: new("category"), Values: []string{"value"}}},
		{And: []types.Expression{}},
	} {
		_, err := client.GetCostAndUsage(
			ctx,
			&ce.GetCostAndUsageInput{
				TimePeriod:  period,
				Granularity: types.GranularityDaily,
				Metrics:     []string{"UnblendedCost"},
				Filter:      filter,
			},
		)
		wantCostExplorerError(t, err, "ValidationException")
	}
	_, err := client.GetCostAndUsage(
		ctx,
		&ce.GetCostAndUsageInput{
			TimePeriod:  period,
			Granularity: types.GranularityHourly,
			Metrics:     []string{"UnblendedCost"},
		},
	)
	wantCostExplorerError(t, err, "ValidationException")
	_, err = client.GetCostAndUsage(
		ctx,
		&ce.GetCostAndUsageInput{
			TimePeriod:  period,
			Granularity: types.GranularityDaily,
			Metrics:     []string{"BlendedCost"},
		},
	)
	wantCostExplorerError(t, err, "ValidationException")
}

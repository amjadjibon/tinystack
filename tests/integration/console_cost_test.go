package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	budgets "github.com/aws/aws-sdk-go-v2/service/budgets"
	cur "github.com/aws/aws-sdk-go-v2/service/costandusagereportservice"
	ce "github.com/aws/aws-sdk-go-v2/service/costexplorer"
)

func TestConsoleCostDefinitions(t *testing.T) {
	srv := server(t)
	const account = "000000000000"
	call := func(service, operation string, input map[string]any) map[string]any {
		t.Helper()
		out := consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, service, operation, account, "", input), http.StatusOK)
		return out["output"].(map[string]any)
	}
	var capabilities map[string]any
	if internal(t, srv, http.MethodGet, consoleActionsPath, &capabilities) != http.StatusOK {
		t.Fatal(capabilities)
	}

	for _, service := range []string{"budgets", "costexplorer", "costandusagereportservice"} {
		if capabilities[service] != true {
			t.Fatalf("missing capability: %s", service)
		}
	}

	budget := map[string]any{
		"BudgetName": "console-cost", "BudgetType": "COST", "TimeUnit": "MONTHLY",
		"BudgetLimit": map[string]any{"Amount": "100", "Unit": "USD"},
	}
	call("budgets", "CreateBudget", map[string]any{"AccountId": account, "Budget": budget})
	budget["BudgetLimit"] = map[string]any{"Amount": "200", "Unit": "USD"}
	call("budgets", "UpdateBudget", map[string]any{"AccountId": account, "NewBudget": budget})
	out, err := budgetsClient(srv, "test", "eu-west-1").DescribeBudget(context.Background(),
		&budgets.DescribeBudgetInput{AccountId: new(account), BudgetName: new("console-cost")})
	if err != nil || *out.Budget.BudgetLimit.Amount != "200" {
		t.Fatalf("SDK budget: %+v, %v", out, err)
	}

	monitor := call("costexplorer", "CreateAnomalyMonitor", map[string]any{"AnomalyMonitor": map[string]any{
		"MonitorName": "console-monitor", "MonitorType": "DIMENSIONAL", "MonitorDimension": "SERVICE",
	}})["MonitorArn"].(string)
	call("costexplorer", "UpdateAnomalyMonitor", map[string]any{"MonitorArn": monitor, "MonitorName": "renamed"})
	monitors, err := costExplorerClient(srv, "test", "ap-southeast-1").GetAnomalyMonitors(context.Background(),
		&ce.GetAnomalyMonitorsInput{})
	if err != nil || len(monitors.AnomalyMonitors) != 1 || *monitors.AnomalyMonitors[0].MonitorName != "renamed" {
		t.Fatalf("SDK monitors: %+v, %v", monitors, err)
	}

	report := map[string]any{
		"ReportName": "console-report", "S3Bucket": "reports", "S3Prefix": "cost/",
		"S3Region": "us-east-1", "TimeUnit": "DAILY", "Format": "textORcsv", "Compression": "GZIP",
		"AdditionalSchemaElements": []any{},
	}
	call("costandusagereportservice", "PutReportDefinition", map[string]any{"ReportDefinition": report})
	report["S3Prefix"] = "updated/"
	call("costandusagereportservice", "ModifyReportDefinition", map[string]any{
		"ReportName": "console-report", "ReportDefinition": report,
	})
	reports, err := curClient(srv, "test", "eu-west-1").DescribeReportDefinitions(context.Background(),
		&cur.DescribeReportDefinitionsInput{})
	if err != nil || len(reports.ReportDefinitions) != 1 || *reports.ReportDefinitions[0].S3Prefix != "updated/" {
		t.Fatalf("SDK reports: %+v, %v", reports, err)
	}

	call("budgets", "DeleteBudget", map[string]any{"AccountId": account, "BudgetName": "console-cost"})
	call("costexplorer", "DeleteAnomalyMonitor", map[string]any{"MonitorArn": monitor})
	call("costandusagereportservice", "DeleteReportDefinition", map[string]any{"ReportName": "console-report"})
}

func TestConsoleCostSignaturesAndScope(t *testing.T) {
	srv := verifyingServer(t)
	for _, tc := range []struct{ service, signing, operation string }{
		{"budgets", "budgets", "DescribeBudgets"},
		{"costexplorer", "ce", "GetAnomalyMonitors"},
		{"costexplorer", "ce", "GetCostAndUsage"},
		{"costexplorer", "ce", "GetDimensionValues"},
		{"costexplorer", "ce", "GetTags"},
		{"costandusagereportservice", "cur", "DescribeReportDefinitions"},
	} {
		t.Run(tc.service, func(t *testing.T) {
			for _, account := range []string{"000000000000", "111111111111"} {
				input := map[string]any{}
				switch tc.operation {
				case "GetCostAndUsage", "GetDimensionValues", "GetTags":
					input["TimePeriod"] = map[string]any{"Start": "2026-10-01", "End": "2026-10-16"}
					if tc.operation == "GetCostAndUsage" {
						input["Granularity"] = "DAILY"
						input["Metrics"] = []string{"UnblendedCost"}
					}
					if tc.operation == "GetDimensionValues" {
						input["Dimension"] = "SERVICE"
					}
				}
				if tc.service == "budgets" {
					input["AccountId"] = account
				}

				r := consoleLambdaRequest(t, srv, tc.service, tc.operation, account, "", input)
				body, err := r.GetBody()
				if err != nil {
					t.Fatal(err)
				}

				raw, err := io.ReadAll(body)
				body.Close()
				if err != nil {
					t.Fatal(err)
				}

				sum := sha256.Sum256(raw)
				if err := v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{
					AccessKeyID: "test", SecretAccessKey: "test",
				}, r, hex.EncodeToString(sum[:]), tc.signing, "us-east-1", time.Now()); err != nil {
					t.Fatal(err)
				}

				status := http.StatusOK
				if account != "000000000000" {
					status = http.StatusForbidden
				}

				consoleResponse(t, srv, r, status)
			}
			consoleResponse(t, srv, consoleLambdaRequest(t, srv, tc.service, tc.operation,
				"000000000000", "", map[string]any{}), http.StatusForbidden)
			consoleResponse(t, srv, consoleLambdaRequest(t, srv, tc.service, "UnsupportedOperation",
				"000000000000", "", map[string]any{}), http.StatusBadRequest)
		})
	}
}

func TestConsoleCostQueries(t *testing.T) {
	srv := server(t)
	period := map[string]any{"Start": "2026-09-01", "End": "2026-11-01"}
	for _, tc := range []struct {
		operation string
		input     map[string]any
		field     string
	}{
		{"GetCostAndUsage", map[string]any{
			"TimePeriod": period, "Granularity": "DAILY", "Metrics": []string{"UnblendedCost"},
		}, "ResultsByTime"},
		{"GetDimensionValues", map[string]any{"TimePeriod": period, "Dimension": "SERVICE"}, "ReturnSize"},
		{"GetTags", map[string]any{"TimePeriod": period}, "Tags"},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			out := consoleResponse(t, srv, consoleLambdaRequest(t, srv, "costexplorer", tc.operation,
				"000000000000", "", tc.input), http.StatusOK)["output"].(map[string]any)
			if _, ok := out[tc.field]; !ok {
				t.Fatalf("missing %s: %+v", tc.field, out)
			}
			if tc.operation == "GetCostAndUsage" {
				token, ok := out["NextPageToken"].(string)
				if !ok || token == "" {
					t.Fatal("expected paginated daily periods")
				}
				tc.input["NextPageToken"] = token
				consoleResponse(t, srv, consoleLambdaRequest(t, srv, "costexplorer", tc.operation,
					"000000000000", "", tc.input), http.StatusOK)
			}
		})
	}
}

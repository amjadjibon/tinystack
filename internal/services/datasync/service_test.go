package datasync

import (
	"context"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/datasync"
	"github.com/amjadjibon/tinystack/internal/common"
)

func TestGeneratedValidation(t *testing.T) {
	descriptor := api.Descriptor(new(Service))
	for _, tt := range []struct {
		input any
		op    string
	}{
		{op: "CreateLocationS3", input: &api.CreateLocationS3Request{S3BucketArn: new("arn:aws:s3:::bucket")}},
		{op: "ListLocations", input: &api.ListLocationsRequest{MaxResults: new(int64(101))}},
		{op: "DescribeTask", input: &api.DescribeTaskRequest{TaskArn: new("wrong")}},
		{op: "UpdateTask", input: &api.UpdateTaskRequest{
			TaskArn: new("arn:aws:datasync:us-east-1:000000000000:task/task-00000000000000000"),
			Options: &api.Options{VerifyMode: new("WRONG")},
		}},
	} {
		t.Run(tt.op, func(t *testing.T) {
			if _, err := descriptor.Operations[tt.op].Invoke(context.Background(), tt.input); err == nil {
				t.Fatal("model constraint was not enforced")
			}
		})
	}
}

func TestSimplePatterns(t *testing.T) {
	for _, tt := range []struct {
		pattern, key           string
		include, want, invalid bool
	}{
		{"/folder", "folder/file", true, true, false},
		{"/folder", "folder2/file", true, false, false},
		{"/folder*|/other", "other/file", true, true, false},
		{"*.tmp", "a/b.tmp", false, true, false},
		{`/a\|b|/other`, "a|b", true, true, false},
		{`/literal\*`, "literal*", true, true, false},
		{`/literal\*`, "literalABC", true, false, false},
		{"*.txt", "file.txt", true, false, true},
		{`/bad\`, "bad", false, false, true},
	} {
		t.Run(tt.pattern, func(t *testing.T) {
			rules := []api.FilterRule{{FilterType: new("SIMPLE_PATTERN"), Value: new(tt.pattern)}}
			expressions, err := compileRules(rules, tt.include)
			if tt.invalid {
				if err == nil {
					t.Fatal("expected filter rejection")
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			matched := false
			for _, expression := range expressions {
				matched = matched || expression.MatchString("/"+tt.key)
			}

			if matched != tt.want {
				t.Fatalf("matched %v, want %v", matched, tt.want)
			}
		})
	}
}

func TestOptionsMergeAndValidation(t *testing.T) {
	base := mergeOptions(nil, &api.Options{LogLevel: new("BASIC")})
	merged := mergeOptions(base, &api.Options{TransferMode: new("ALL"), VerifyMode: new("NONE")})
	if common.Value(base.TransferMode) != "CHANGED" || common.Value(merged.LogLevel) != "BASIC" {
		t.Fatal("merge modified base or lost options")
	}

	if err := validateTaskOptions(merged, nil, nil); err != nil {
		t.Fatal(err)
	}

	merged.PreserveDeletedFiles = new("REMOVE")
	if err := validateTaskOptions(merged, nil, nil); err == nil {
		t.Fatal("ALL and REMOVE must fail")
	}

	if pageLimit(new(int64(0))) != 100 {
		t.Fatal("zero page size must use default")
	}
}

func TestSkippedVerificationResult(t *testing.T) {
	execution := new(executionRecord{DescribeTaskExecutionResponse: api.DescribeTaskExecutionResponse{
		Status: new(statusVerifying), Options: &api.Options{VerifyMode: new(optionNone)},
		StartTime: new(timestamp()), Result: &api.TaskExecutionResultDetail{VerifyStatus: new(phasePending)},
	}})
	service := new(Service)
	if err := service.verify(context.Background(), execution); err != nil {
		t.Fatal(err)
	}

	if common.Value(execution.Status) != statusSuccess || common.Value(execution.Result.VerifyStatus) != statusSuccess {
		t.Fatalf("skipped verification must report a modelled phase status: %+v", execution.Result)
	}
}

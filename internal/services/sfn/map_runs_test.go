package sfn

import (
	"context"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/sfn"
)

func TestMapRunModelConstraints(t *testing.T) {
	descriptor := api.Descriptor(new(Service))
	mapARN := "arn:aws:states:us-east-1:000000000000:mapRun:m/l:id"
	for _, tc := range []struct {
		input any
		name  string
	}{
		{name: "DescribeMapRun", input: &api.DescribeMapRunInput{}},
		{name: "ListMapRuns", input: &api.ListMapRunsInput{}},
		{name: "UpdateMapRun", input: &api.UpdateMapRunInput{MapRunArn: new(mapARN), MaxConcurrency: new(int64(-1))}},
		{name: "UpdateMapRun", input: &api.UpdateMapRunInput{MapRunArn: new(mapARN), MaxConcurrency: new(int64(10001))}},
		{name: "UpdateMapRun", input: &api.UpdateMapRunInput{
			MapRunArn: new(mapARN), ToleratedFailurePercentage: new(float64(101)),
		}},
		{name: "UpdateMapRun", input: &api.UpdateMapRunInput{MapRunArn: new(mapARN), ToleratedFailureCount: new(int64(-1))}},
	} {
		if _, err := descriptor.Operations[tc.name].Invoke(context.Background(), tc.input); err == nil {
			t.Fatalf("%s accepted %+v", tc.name, tc.input)
		}
	}
}

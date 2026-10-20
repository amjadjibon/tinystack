package logs

import (
	"context"
	"errors"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
)

func TestSubscriptionPermissionErrors(t *testing.T) {
	s := &Service{authorizer: func(context.Context, catalog.ServiceDelivery) error {
		return errors.New("denied")
	}}
	scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}
	for _, tc := range []struct{ name, target, want string }{
		{
			name: "Lambda", target: "arn:aws:lambda:us-east-1:000000000000:function:worker",
			want: "Could not execute the lambda function. Make sure you have given CloudWatch Logs " +
				"permission to execute your function.",
		},
		{
			name: "Firehose", target: "arn:aws:firehose:us-east-1:000000000000:deliverystream/stream",
			want: "Could not deliver test message to specified Firehose stream. " +
				"Check if the given Firehose stream is in ACTIVE state.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := s.checkDestination(context.Background(), scope, "app", tc.target, "role")
			awsErr, ok := errors.AsType[*awserror.Error](err)
			if !ok || awsErr.Code != "InvalidParameterException" || awsErr.Message != tc.want {
				t.Fatalf("checkDestination = %v, want InvalidParameterException: %s", err, tc.want)
			}
		})
	}
}

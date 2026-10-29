package firehose

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// AuthorizationResources permits delivery roles to scope writes to a single delivery stream.
func (s *Service) AuthorizationResources(
	ctx context.Context, operation string, input any,
) ([]catalog.AuthorizationResource, error) {
	if operation != "PutRecord" && operation != "PutRecordBatch" {
		return nil, nil
	}

	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name := catalog.InputString(input, "DeliveryStreamName")
	if name == "" {
		return nil, nil
	}

	target := catalog.AuthorizationResource{ARN: streamARN(m.Scope, name), TagsKnown: true}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		st, found, readErr := storage.Read[deliveryStream](tx, streamKey(m.Scope, name))
		if readErr != nil {
			return readErr
		}

		if found {
			target.Tags = common.CloneTags(st.Tags)
		}

		return nil
	})
	return []catalog.AuthorizationResource{target}, err
}

// authorizeSource checks a cross-account read as the configured Kinesis source role.
func (s *Service) authorizeSource(ctx context.Context, scope request.Scope, st *deliveryStream) error {
	arn := common.Value(st.Source.KinesisStreamARN)
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 || parts[4] == scope.AccountID {
		return nil
	}

	for _, operation := range []string{"DescribeStream", "ListShards", "GetShardIterator", "GetRecords"} {
		err := s.authorizer.Authorize(ctx, catalog.ServiceDelivery{
			Principal: "firehose.amazonaws.com", SourceARN: streamARN(scope, st.Name), SourceAccount: scope.AccountID,
			Role: &catalog.DeliveryRole{
				ARN: common.Value(st.Source.RoleARN), Session: st.Name,
				Service: "kinesis", Operation: operation, Params: map[string]any{"StreamARN": arn},
			},
		})
		if err != nil {
			return err
		}
	}

	return nil
}

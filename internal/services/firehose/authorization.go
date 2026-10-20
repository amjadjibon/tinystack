package firehose

import (
	"context"

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

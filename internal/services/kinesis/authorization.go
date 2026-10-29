package kinesis

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// AuthorizationResources resolves stream and consumer actions to the owning resource's policy.
func (s *Service) AuthorizationResources(
	ctx context.Context,
	operation string,
	input any,
) ([]catalog.AuthorizationResource, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name, arn := catalog.InputString(input, "StreamName"), catalog.InputString(input, "StreamARN")
	resource := catalog.InputString(input, "ResourceARN")
	consumerARN := catalog.InputString(input, "ConsumerARN")
	consumerName := catalog.InputString(input, "ConsumerName")
	switch operation {
	case "ListStreams", "DescribeLimits":
		return nil, nil
	case "CreateStream":
		return []catalog.AuthorizationResource{
			{ARN: StreamARN(m.Scope, name), OwnerAccount: m.AccountID, TagsKnown: true},
		}, nil
	case "GetRecords":
		it, ok := decodeIterator(catalog.InputString(input, "ShardIterator"))
		if !ok {
			// In-process permission probes supply a placeholder iterator and a concrete ARN.
			// GetRecords validates actual iterators before reading any records.
			if arn != "" {
				break
			}

			return nil, invalidArgument("Invalid ShardIterator")
		}

		fromIterator := StreamARN(request.Scope{AccountID: it.Account, Region: it.Region}, it.Stream)
		if arn != "" && arn != fromIterator {
			return nil, invalidArgument("StreamARN does not match ShardIterator")
		}

		arn = fromIterator
	case "ListShards":
		if token := catalog.InputString(input, "NextToken"); token != "" {
			arn, _, _ = strings.Cut(token, "|")
			name = ""
		}
	}

	if resource != "" {
		if strings.Contains(resource, "/consumer/") {
			consumerARN = resource
		} else {
			arn = resource
		}
	}

	if consumerARN != "" {
		arn, _, _ = strings.Cut(consumerARN, "/consumer/")
	}

	var target catalog.AuthorizationResource
	err = s.view(
		ctx,
		optionalName(name),
		optionalName(arn),
		func(_ storage.ReadTx, scope request.Scope, st *stream) error {
			target = catalog.AuthorizationResource{
				ARN:          StreamARN(scope, st.Name),
				OwnerAccount: scope.AccountID,
				Policy:       st.Policy,
				Tags:         st.Tags,
				TagsKnown:    true,
			}

			if consumerARN != "" ||
				((operation == "DescribeStreamConsumer" || operation == "DeregisterStreamConsumer") && consumerName != "") {
				for i := range st.Consumers {
					c := &st.Consumers[i]
					if c.ARN == consumerARN || (consumerARN == "" && c.Name == consumerName) {
						target.ARN, target.Policy = c.ARN, c.Policy
						return nil
					}
				}

				return notFound(scope, "consumer")
			}

			return nil
		},
	)
	return []catalog.AuthorizationResource{target}, err
}

func optionalName(value string) *string {
	if value == "" {
		return nil
	}

	return &value
}

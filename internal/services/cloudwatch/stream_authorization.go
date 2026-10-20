package cloudwatch

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// AuthorizationResources resolves metric stream actions and tag calls to their regional ARNs.
func (s *Service) AuthorizationResources(
	ctx context.Context, operation string, input any,
) ([]catalog.AuthorizationResource, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var names []string
	switch operation {
	case "PutMetricStream", "GetMetricStream", "DeleteMetricStream":
		names = catalog.InputStrings(input, "Name")
	case "StartMetricStreams", "StopMetricStreams":
		names = catalog.InputStrings(input, "Names")
	case "TagResource", "UntagResource", "ListTagsForResource":
		if name, ok := strings.CutPrefix(catalog.InputString(input, "ResourceARN"), streamARN(m.Scope, "")); ok {
			names = []string{name}
		}
	}

	var out []catalog.AuthorizationResource
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, name := range names {
			if name == "" {
				continue
			}

			target := catalog.AuthorizationResource{ARN: streamARN(m.Scope, name), TagsKnown: true}
			st, found, readErr := storage.Read[metricStream](tx, streamKey(m.Scope, name))
			if readErr != nil {
				return readErr
			}

			if found {
				target.Tags = common.CloneTags(st.Tags)
			}

			out = append(out, target)
		}

		return nil
	})
	return out, err
}

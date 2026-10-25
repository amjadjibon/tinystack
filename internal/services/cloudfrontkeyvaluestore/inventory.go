package cloudfrontkeyvaluestore

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

// Resources returns no separate resources: stores and their tags are inventoried
// once by CloudFront, their owner. Keys have no CloudFormation resource type.
func (s *Service) Resources(context.Context) ([]catalog.Resource, error) { return nil, nil }

var _ catalog.Inventory = (*Service)(nil)

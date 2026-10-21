package bedrockruntime

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

// Resources is empty: inference requests do not create persistent AWS resources.
func (s *Service) Resources(context.Context) ([]catalog.Resource, error) {
	return []catalog.Resource{}, nil
}

package comprehend

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

// Resources lists service resources. Synchronous analysis creates no resources.
func (s *Service) Resources(context.Context) ([]catalog.Resource, error) {
	return []catalog.Resource{}, nil
}

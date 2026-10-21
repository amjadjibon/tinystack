package rdsdata

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

// Resources is empty: Data API transactions are ephemeral, and clusters belong to RDS.
func (s *Service) Resources(context.Context) ([]catalog.Resource, error) { return nil, nil }

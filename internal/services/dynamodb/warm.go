package dynamodb

import (
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	"github.com/amjadjibon/tinystack/internal/common"
)

// New tables and GSIs expose their default warm capacity immediately. TinyStack
// does not prewarm partitions or simulate traffic-driven scaling.
func tableWarmThroughput(p *api.ProvisionedThroughputDescription) *api.TableWarmThroughputDescription {
	return &api.TableWarmThroughputDescription{
		ReadUnitsPerSecond:  new(max(int64(12000), common.Value(p.ReadCapacityUnits))),
		WriteUnitsPerSecond: new(max(int64(4000), common.Value(p.WriteCapacityUnits))),
		Status:              new("ACTIVE"),
	}
}

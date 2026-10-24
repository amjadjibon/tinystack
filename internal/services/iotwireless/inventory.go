package iotwireless

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// inventoryTypes name the inventory type of each kind.
var inventoryTypes = map[string]string{
	kindDestination:    "destination",
	kindDeviceProfile:  "device-profile",
	kindServiceProfile: "service-profile",
	kindDevice:         "wireless-device",
	kindGateway:        "wireless-gateway",
	kindMulticast:      "multicast-group",
	kindFuota:          "fuota-task",
}

// Resources exposes IoT Wireless resources to inventory and tagging.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok {
				continue
			}

			kind, _, _ := strings.Cut(rest, "/")
			typ, ok := inventoryTypes[kind]
			if !ok {
				continue
			}

			r, _, err := storage.Read[record](tx, key)
			if err != nil {
				return err
			}

			name := r.Name
			if name == "" {
				name = r.ID
			}

			out = append(out, catalog.Resource{
				Account: account, Region: region, Type: typ, Name: name, ARN: r.Arn, Tags: maps.Clone(r.Tags),
				Details: map[string]any{"Configuration": r},
			})
		}

		return nil
	})

	return out, err
}

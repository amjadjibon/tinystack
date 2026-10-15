package cloudwatch

import (
	"context"
	"math/big"
	"slices"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudwatch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/costdata"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const billingNamespace = "AWS/Billing"

// billingSample is durable so restarts do not duplicate a minute's sample.
type billingSample struct {
	Services []string
	Slot     int64
}

func (s *Service) visitBilling(ctx context.Context) {
	var accounts []string
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys(costdata.BillingIndexPrefix) {
			accounts = append(accounts, strings.TrimPrefix(key, costdata.BillingIndexPrefix))
		}

		return nil
	})
	if err != nil {
		s.logger.Warn("listing billing fixtures failed", "error", err)
		return
	}

	for _, account := range accounts {
		if err := s.sampleBilling(ctx, account); err != nil {
			s.logger.Warn("publishing billing metrics failed", "account", account, "error", err)
		}
	}
}

// sampleBilling reads fixtures, claims the slot, and ingests metrics in one transaction.
// Reset and concurrent fixture replacements therefore cannot resurrect a stale sample.
func (s *Service) sampleBilling(ctx context.Context, account string) error {
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		scope := request.Scope{AccountID: account}
		index := costdata.BillingIndexKey(scope)
		dirty, exists := tx.Get(index)
		if !exists {
			return nil
		}

		now := s.now().UTC()
		key := scope.Key(service, "billing", "sample")
		previous, sampled, err := storage.Read[billingSample](tx, key)
		if err != nil {
			return err
		}

		slot := now.Unix() / 60
		if sampled && previous.Slot == slot && string(dirty) != "dirty" {
			return nil
		}

		fixtures, err := storage.Records[costdata.Fixture](tx, scope.Prefix("costdata", "fixture"))
		if err != nil {
			return err
		}

		if len(fixtures) == 0 && !sampled {
			tx.Delete(index)
			return nil
		}

		start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		lines := costdata.Lines(fixtures)
		selected := costdata.Select(lines, start, now, func(costdata.Line) bool { return true })
		services := map[string][]costdata.Line{}
		for i := range lines {
			if name := billingServiceName(lines[i].Service); name != "" {
				services[name] = nil
			}
		}

		for i := range selected {
			if name := billingServiceName(selected[i].Service); name != "" {
				services[name] = append(services[name], selected[i])
			}
		}

		var names []string
		for name := range services {
			names = append(names, name)
		}

		slices.Sort(names)
		if sampled {
			// Removed services receive one zero, like deleting the last fixture.
			for _, name := range previous.Services {
				if _, present := services[name]; !present {
					services[name] = nil
				}
			}
		}

		if err := s.ingestBilling(tx, account, "", costdata.Sum(selected), now); err != nil {
			return err
		}

		for name, serviceLines := range services {
			if err := s.ingestBilling(tx, account, name, costdata.Sum(serviceLines), now); err != nil {
				return err
			}
		}

		if len(fixtures) == 0 {
			tx.Delete(index)
			tx.Delete(key)
			return nil
		}

		tx.Put(index, nil)
		return storage.Write(tx, key, billingSample{Slot: slot, Services: names})
	})
}

// ingestBilling is the narrow internal AWS namespace ingestion path. The datum uses
// the same validation, storage and retention as PutMetricData; its public guard stays intact.
func (s *Service) ingestBilling(tx storage.WriteTx, account, name, amount string, now time.Time) error {
	decimal, _ := new(big.Rat).SetString(amount)
	value, _ := decimal.Float64()
	d := api.MetricDatum{
		MetricName: new("EstimatedCharges"), Unit: new(unitNone), Value: new(value),
		Dimensions: []api.Dimension{{Name: new("Currency"), Value: new("USD")}},
	}
	if name != "" {
		d.Dimensions = append(d.Dimensions, api.Dimension{Name: new("ServiceName"), Value: new(name)})
	}

	p, err := datum(&d, now)
	if err != nil {
		return err
	}

	scope := request.Scope{AccountID: account, Region: "us-east-1"}
	return s.storeMetric(tx, scope, billingNamespace, &d, &p, now)
}

func billingServiceName(label string) string {
	// AWS's CE CLI example identifies the S3 SERVICE label, and its billing example identifies AmazonS3:
	// https://docs.aws.amazon.com/cli/latest/reference/ce/get-cost-and-usage.html
	// https://aws.amazon.com/blogs/developer/monitoring-your-estimated-costs-with-windows-powershell/
	// Other labels remain total-only until their correspondence can be established.
	if label == "Amazon Simple Storage Service" {
		return "AmazonS3"
	}

	return ""
}

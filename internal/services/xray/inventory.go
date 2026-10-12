package xray

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok {
				continue
			}

			kind, _, _ := strings.Cut(rest, "/")
			details := map[string]any{"RecordsOnly": true, "ExecutionReady": false}
			r := catalog.Resource{Account: account, Region: region, Type: kind, Details: details}
			switch kind {
			case groupKind:
				rec, _, e := storage.Read[groupRecord](tx, key)
				if e != nil {
					return e
				}

				r.Name = *rec.Group.GroupName
				r.ARN = *rec.Group.GroupARN
				r.Tags = maps.Clone(rec.Tags)
				details["Configuration"] = rec.Group
			case samplingRuleKind:
				rec, _, e := storage.Read[samplingRuleRecord](tx, key)
				if e != nil {
					return e
				}

				if *rec.Record.SamplingRule.RuleName == defaultRuleName {
					continue
				}

				r.Name = *rec.Record.SamplingRule.RuleName
				r.ARN = *rec.Record.SamplingRule.RuleARN
				r.Tags = maps.Clone(rec.Tags)
				details["Configuration"] = rec.Record.SamplingRule
			default:
				continue
			}

			out = append(out, r)
		}

		return nil
	})
	return out, err
}

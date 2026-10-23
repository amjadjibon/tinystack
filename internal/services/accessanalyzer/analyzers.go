package accessanalyzer

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/accessanalyzer"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// CreateAnalyzer creates an analyzer for the specified account or organization.
func (s *Service) CreateAnalyzer(
	ctx context.Context,
	in *api.CreateAnalyzerRequest,
) (*api.CreateAnalyzerResponse, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	name, analyzerType := common.Value(in.AnalyzerName), common.Value(in.Type)
	if err := validateAnalyzerConfiguration(in.Configuration); err != nil {
		return nil, err
	}

	ruleNames := map[string]bool{}
	for _, rule := range in.ArchiveRules {
		if ruleNames[common.Value(rule.RuleName)] {
			return nil, awserror.Validation("Archive rule %s is listed more than once.", common.Value(rule.RuleName))
		}

		ruleNames[common.Value(rule.RuleName)] = true
		if err := validateFilter(rule.Filter); err != nil {
			return nil, err
		}
	}

	now := time.Now().UTC()
	arn := analyzerArn(scope, name)
	record := analyzerRecord{
		CreatedAt:     now,
		Tags:          common.CloneTags(in.Tags),
		Configuration: in.Configuration,
		ARN:           arn,
		Name:          name,
		Type:          analyzerType,
		Status:        statusActive,
	}

	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := analyzerKey(scope, name)
		_, found, readErr := storage.Read[analyzerRecord](tx, k)
		if readErr != nil {
			return readErr
		}

		if found {
			return conflict(fmt.Sprintf("Analyzer %s already exists.", name))
		}

		if writeErr := storage.Write(tx, k, record); writeErr != nil {
			return writeErr
		}

		for _, rule := range in.ArchiveRules {
			ruleName := common.Value(rule.RuleName)
			ruleRec := archiveRuleRecord{
				CreatedAt:    now,
				UpdatedAt:    now,
				Filter:       rule.Filter,
				AnalyzerName: name,
				RuleName:     ruleName,
			}

			ruleKey := archiveRuleKey(scope, name, ruleName)
			if writeErr := storage.Write(tx, ruleKey, ruleRec); writeErr != nil {
				return writeErr
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateAnalyzerResponse{Arn: new(arn)}, nil
}

// GetAnalyzer retrieves information about the specified analyzer.
func (s *Service) GetAnalyzer(
	ctx context.Context,
	in *api.GetAnalyzerRequest,
) (*api.GetAnalyzerResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.AnalyzerName)
	if name == "" {
		return nil, notFound("Analyzer", "")
	}

	var rec analyzerRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, found, readErr := storage.Read[analyzerRecord](tx, analyzerKey(scope, name))
		if readErr != nil {
			return readErr
		}

		if !found {
			return notFound("Analyzer", name)
		}

		rec = *r
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.GetAnalyzerResponse{
		Analyzer: &api.AnalyzerSummary{
			Arn:           new(rec.ARN),
			Name:          new(rec.Name),
			Type:          new(rec.Type),
			Status:        new(rec.Status),
			CreatedAt:     new(rec.CreatedAt),
			Tags:          maps.Clone(rec.Tags),
			Configuration: rec.Configuration,
		},
	}, nil
}

// ListAnalyzers retrieves a list of analyzers.
func (s *Service) ListAnalyzers(
	ctx context.Context,
	in *api.ListAnalyzersRequest,
) (*api.ListAnalyzersResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var summaries []api.AnalyzerSummary
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		prefix := scope.Prefix(serviceName, kindAnalyzer)
		for _, k := range tx.Keys(prefix) {
			r, found, readErr := storage.Read[analyzerRecord](tx, k)
			if readErr != nil {
				return readErr
			}

			if !found {
				continue
			}

			if in.Type != nil && *in.Type != "" && r.Type != *in.Type {
				continue
			}

			summaries = append(summaries, api.AnalyzerSummary{
				Arn:           new(r.ARN),
				Name:          new(r.Name),
				Type:          new(r.Type),
				Status:        new(r.Status),
				CreatedAt:     new(r.CreatedAt),
				Tags:          maps.Clone(r.Tags),
				Configuration: r.Configuration,
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	limit := pageSize(in.MaxResults)
	page, next, err := common.Page(
		summaries,
		func(s api.AnalyzerSummary) string { return common.Value(s.Name) },
		in.NextToken,
		limit,
		invalidToken(),
	)
	if err != nil {
		return nil, err
	}

	return &api.ListAnalyzersResponse{
		Analyzers: page,
		NextToken: next,
	}, nil
}

// UpdateAnalyzer updates the configuration of the specified analyzer.
func (s *Service) UpdateAnalyzer(
	ctx context.Context,
	in *api.UpdateAnalyzerRequest,
) (*api.UpdateAnalyzerResponse, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	name := common.Value(in.AnalyzerName)
	if err := validateAnalyzerConfiguration(in.Configuration); err != nil {
		return nil, err
	}

	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := analyzerKey(scope, name)
		r, found, readErr := storage.Read[analyzerRecord](tx, k)
		if readErr != nil {
			return readErr
		}

		if !found {
			return notFound("Analyzer", name)
		}

		r.Configuration = in.Configuration
		return storage.Write(tx, k, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateAnalyzerResponse{
		Configuration: in.Configuration,
	}, nil
}

// DeleteAnalyzer deletes the specified analyzer.
func (s *Service) DeleteAnalyzer(
	ctx context.Context,
	in *api.DeleteAnalyzerRequest,
) (*api.DeleteAnalyzerOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.AnalyzerName)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := analyzerKey(scope, name)
		_, found, readErr := storage.Read[analyzerRecord](tx, k)
		if readErr != nil {
			return readErr
		}

		if !found {
			return notFound("Analyzer", name)
		}

		tx.Delete(k)

		prefix := archiveRulePrefix(scope, name)
		for _, ruleKey := range tx.Keys(prefix) {
			tx.Delete(ruleKey)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteAnalyzerOutput{}, nil
}

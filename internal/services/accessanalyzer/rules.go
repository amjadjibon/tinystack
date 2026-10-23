package accessanalyzer

import (
	"context"
	"fmt"
	"slices"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/accessanalyzer"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// CreateArchiveRule creates an archive rule for the specified analyzer.
func (s *Service) CreateArchiveRule(
	ctx context.Context,
	in *api.CreateArchiveRuleRequest,
) (*api.CreateArchiveRuleOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	analyzerName := common.Value(in.AnalyzerName)
	ruleName := common.Value(in.RuleName)
	if err := validateFilter(in.Filter); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	record := archiveRuleRecord{
		CreatedAt:    now,
		UpdatedAt:    now,
		Filter:       in.Filter,
		AnalyzerName: analyzerName,
		RuleName:     ruleName,
	}

	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		_, found, readErr := storage.Read[analyzerRecord](tx, analyzerKey(scope, analyzerName))
		if readErr != nil {
			return readErr
		}

		if !found {
			return notFound("Analyzer", analyzerName)
		}

		rk := archiveRuleKey(scope, analyzerName, ruleName)
		_, ruleFound, ruleErr := storage.Read[archiveRuleRecord](tx, rk)
		if ruleErr != nil {
			return ruleErr
		}

		if ruleFound {
			return conflict(fmt.Sprintf("Archive rule %s already exists.", ruleName))
		}

		return storage.Write(tx, rk, record)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateArchiveRuleOutput{}, nil
}

// GetArchiveRule retrieves information about an archive rule for the specified analyzer.
func (s *Service) GetArchiveRule(
	ctx context.Context,
	in *api.GetArchiveRuleRequest,
) (*api.GetArchiveRuleResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	analyzerName := common.Value(in.AnalyzerName)
	ruleName := common.Value(in.RuleName)
	var rec archiveRuleRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, found, readErr := storage.Read[analyzerRecord](tx, analyzerKey(scope, analyzerName))
		if readErr != nil {
			return readErr
		}

		if !found {
			return notFound("Analyzer", analyzerName)
		}

		r, ruleFound, ruleErr := storage.Read[archiveRuleRecord](tx, archiveRuleKey(scope, analyzerName, ruleName))
		if ruleErr != nil {
			return ruleErr
		}

		if !ruleFound {
			return notFound("ArchiveRule", ruleName)
		}

		rec = *r
		return nil
	})
	if err != nil {
		return nil, err
	}

	filterCopy := make(map[string]api.Criterion, len(rec.Filter))
	for k, v := range rec.Filter {
		filterCopy[k] = api.Criterion{
			Eq:       slices.Clone(v.Eq),
			Neq:      slices.Clone(v.Neq),
			Contains: slices.Clone(v.Contains),
			Exists:   v.Exists,
		}
	}

	return &api.GetArchiveRuleResponse{
		ArchiveRule: &api.ArchiveRuleSummary{
			RuleName:  new(rec.RuleName),
			Filter:    filterCopy,
			CreatedAt: new(rec.CreatedAt),
			UpdatedAt: new(rec.UpdatedAt),
		},
	}, nil
}

// ListArchiveRules retrieves a list of archive rules created for the specified analyzer.
func (s *Service) ListArchiveRules(
	ctx context.Context,
	in *api.ListArchiveRulesRequest,
) (*api.ListArchiveRulesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	analyzerName := common.Value(in.AnalyzerName)
	var summaries []api.ArchiveRuleSummary
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, found, readErr := storage.Read[analyzerRecord](tx, analyzerKey(scope, analyzerName))
		if readErr != nil {
			return readErr
		}

		if !found {
			return notFound("Analyzer", analyzerName)
		}

		prefix := archiveRulePrefix(scope, analyzerName)
		for _, k := range tx.Keys(prefix) {
			r, ruleFound, ruleErr := storage.Read[archiveRuleRecord](tx, k)
			if ruleErr != nil {
				return ruleErr
			}

			if !ruleFound {
				continue
			}

			filterCopy := make(map[string]api.Criterion, len(r.Filter))
			for fk, fv := range r.Filter {
				filterCopy[fk] = api.Criterion{
					Eq:       slices.Clone(fv.Eq),
					Neq:      slices.Clone(fv.Neq),
					Contains: slices.Clone(fv.Contains),
					Exists:   fv.Exists,
				}
			}

			summaries = append(summaries, api.ArchiveRuleSummary{
				RuleName:  new(r.RuleName),
				Filter:    filterCopy,
				CreatedAt: new(r.CreatedAt),
				UpdatedAt: new(r.UpdatedAt),
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
		func(s api.ArchiveRuleSummary) string { return common.Value(s.RuleName) },
		in.NextToken,
		limit,
		invalidToken(),
	)
	if err != nil {
		return nil, err
	}

	return &api.ListArchiveRulesResponse{
		ArchiveRules: page,
		NextToken:    next,
	}, nil
}

// UpdateArchiveRule updates the specified archive rule for an analyzer.
func (s *Service) UpdateArchiveRule(
	ctx context.Context,
	in *api.UpdateArchiveRuleRequest,
) (*api.UpdateArchiveRuleOutput, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	analyzerName := common.Value(in.AnalyzerName)
	ruleName := common.Value(in.RuleName)
	if err := validateFilter(in.Filter); err != nil {
		return nil, err
	}

	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		_, found, readErr := storage.Read[analyzerRecord](tx, analyzerKey(scope, analyzerName))
		if readErr != nil {
			return readErr
		}

		if !found {
			return notFound("Analyzer", analyzerName)
		}

		rk := archiveRuleKey(scope, analyzerName, ruleName)
		r, ruleFound, ruleErr := storage.Read[archiveRuleRecord](tx, rk)
		if ruleErr != nil {
			return ruleErr
		}

		if !ruleFound {
			return notFound("ArchiveRule", ruleName)
		}

		r.Filter = in.Filter
		r.UpdatedAt = time.Now().UTC()
		return storage.Write(tx, rk, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateArchiveRuleOutput{}, nil
}

// DeleteArchiveRule deletes the specified archive rule.
func (s *Service) DeleteArchiveRule(
	ctx context.Context,
	in *api.DeleteArchiveRuleRequest,
) (*api.DeleteArchiveRuleOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	analyzerName := common.Value(in.AnalyzerName)
	ruleName := common.Value(in.RuleName)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		_, found, readErr := storage.Read[analyzerRecord](tx, analyzerKey(scope, analyzerName))
		if readErr != nil {
			return readErr
		}

		if !found {
			return notFound("Analyzer", analyzerName)
		}

		rk := archiveRuleKey(scope, analyzerName, ruleName)
		_, ruleFound, ruleErr := storage.Read[archiveRuleRecord](tx, rk)
		if ruleErr != nil {
			return ruleErr
		}

		if !ruleFound {
			return notFound("ArchiveRule", ruleName)
		}

		tx.Delete(rk)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteArchiveRuleOutput{}, nil
}

// ApplyArchiveRule applies the specified archive rule to the analyzer's findings.
func (s *Service) ApplyArchiveRule(
	ctx context.Context,
	in *api.ApplyArchiveRuleRequest,
) (*api.ApplyArchiveRuleOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	analyzerName, err := parseAnalyzerArn(scope, common.Value(in.AnalyzerArn))
	if err != nil {
		return nil, err
	}

	ruleName := common.Value(in.RuleName)
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, found, readErr := storage.Read[analyzerRecord](tx, analyzerKey(scope, analyzerName))
		if readErr != nil {
			return readErr
		}

		if !found {
			return notFound("Analyzer", common.Value(in.AnalyzerArn))
		}

		_, ruleFound, ruleErr := storage.Read[archiveRuleRecord](tx, archiveRuleKey(scope, analyzerName, ruleName))
		if ruleErr != nil {
			return ruleErr
		}

		if !ruleFound {
			return notFound("ArchiveRule", ruleName)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.ApplyArchiveRuleOutput{}, nil
}

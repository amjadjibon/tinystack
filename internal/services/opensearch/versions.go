package opensearch

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/opensearch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// This is an emulator catalogue, shared by discovery and compatibility checks.
var versions = func() []string {
	result := []string{
		"Elasticsearch_7.10",
		"OpenSearch_1.3",
		"OpenSearch_2.3",
		"OpenSearch_2.5",
		"OpenSearch_2.7",
		"OpenSearch_2.9",
		"OpenSearch_2.11",
		"OpenSearch_2.13",
		"OpenSearch_2.15",
		"OpenSearch_2.17",
		"OpenSearch_2.19",
		"OpenSearch_3.1",
		"OpenSearch_3.3",
	}
	slices.Sort(result)
	return result
}()

func pageLimit(maximum *int64) (int, error) {
	if maximum == nil {
		return 100, nil
	}

	if *maximum < 1 || *maximum > 100 {
		return 0, invalid("MaxResults must be between 1 and 100")
	}

	return int(*maximum), nil
}

func (s *Service) ListVersions(_ context.Context, in *api.ListVersionsRequest) (*api.ListVersionsResponse, error) {
	if in == nil {
		in = &api.ListVersionsRequest{}
	}

	limit, err := pageLimit(in.MaxResults)
	if err != nil {
		return nil, err
	}

	items, next, err := common.Page(
		versions,
		func(v string) string { return v },
		in.NextToken,
		limit,
		invalid("Invalid NextToken"),
	)
	return &api.ListVersionsResponse{Versions: items, NextToken: next}, err
}

func compatible(source string) []string {
	family, number, _ := strings.Cut(source, "_")
	majorText, minorText, _ := strings.Cut(number, ".")
	major, _ := strconv.Atoi(majorText)
	minor, _ := strconv.Atoi(minorText)
	var targets []string
	for _, target := range versions {
		f, n, _ := strings.Cut(target, "_")
		ma, mi, _ := strings.Cut(n, ".")
		a, _ := strconv.Atoi(ma)
		b, _ := strconv.Atoi(mi)
		if (family == engineElasticsearch && f == engineOpenSearch && a == 1) ||
			(family == f && ((a == major && b > minor) || a == major+1)) {
			targets = append(targets, target)
		}
	}

	return targets
}

func (s *Service) readDomain(ctx context.Context, name *string) (*domainRecord, error) {
	if common.Value(name) == "" {
		return nil, invalid("DomainName is required")
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	var rec *domainRecord
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scope.Key(serviceName, domainKind, *name)
		var found bool
		var readErr error
		rec, found, readErr = storage.Read[domainRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			return notFound("Domain not found: %s", *name)
		}

		return settle(tx, key, rec)
	})
	return rec, err
}

func (s *Service) GetCompatibleVersions(
	ctx context.Context,
	in *api.GetCompatibleVersionsRequest,
) (*api.GetCompatibleVersionsResponse, error) {
	sources := versions
	if in != nil && in.DomainName != nil {
		rec, err := s.readDomain(ctx, in.DomainName)
		if err != nil {
			return nil, err
		}

		sources = []string{common.Value(rec.Status.EngineVersion)}
	}

	out := &api.GetCompatibleVersionsResponse{}
	for _, v := range sources {
		out.CompatibleVersions = append(
			out.CompatibleVersions,
			api.CompatibleVersionsMap{SourceVersion: new(v), TargetVersions: compatible(v)},
		)
	}

	return out, nil
}

func (s *Service) UpgradeDomain(ctx context.Context, in *api.UpgradeDomainRequest) (*api.UpgradeDomainResponse, error) {
	if in == nil || common.Value(in.DomainName) == "" || common.Value(in.TargetVersion) == "" {
		return nil, invalid("DomainName and TargetVersion are required")
	}

	if s.runtime != nil {
		return nil, unsupported(
			"Engine upgrades and upgrade checks are not supported in Docker mode; " +
				"create a domain with the desired engine version",
		)
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id := common.RandomID(16)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scope.Key(serviceName, domainKind, *in.DomainName)
		rec, found, readErr := storage.Read[domainRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			return notFound("Domain not found: %s", *in.DomainName)
		}

		if changeErr := settle(tx, key, rec); changeErr != nil {
			return changeErr
		}

		if common.Value(rec.Status.Processing) || common.Value(rec.Status.Deleted) {
			return invalid("Domain is processing another change")
		}

		if !slices.Contains(compatible(common.Value(rec.Status.EngineVersion)), *in.TargetVersion) {
			return invalid("TargetVersion is not compatible with the domain engine")
		}

		previous := common.Clone(rec.Status)
		if changeErr := applyAdvanced(rec.Status, in.AdvancedOptions); changeErr != nil {
			return changeErr
		}

		rec.UpdatedAt = time.Now().UTC()
		rec.UpgradeTarget, rec.UpgradeCheck = *in.TargetVersion, common.Value(in.PerformCheckOnly)
		if rec.UpgradeCheck && len(in.AdvancedOptions) > 0 {
			return invalid("AdvancedOptions cannot change during a check-only upgrade")
		}

		if !rec.UpgradeCheck {
			rec.Status.EngineVersion = in.TargetVersion
		}
		beginChange(rec, previous)
		// Publish the target only after the upgrade finishes.
		rec.Status.EngineVersion = previous.EngineVersion
		rec.PendingUntil = rec.UpdatedAt.Add(time.Second)
		rec.Status.Processing, rec.Status.UpgradeProcessing = new(true), new(true)
		rec.Status.DomainProcessingStatus = new("UpgradingEngineVersion")
		step := "UPGRADE"
		if rec.UpgradeCheck {
			step = "PRE_UPGRADE_CHECK"
		}

		rec.History = append(
			rec.History,
			api.UpgradeHistory{
				UpgradeName:    new(fmt.Sprintf("%s-%s", *in.TargetVersion, id)),
				StartTimestamp: common.Seconds(rec.UpdatedAt),
				UpgradeStatus:  new("IN_PROGRESS"),
				StepsList: []api.UpgradeStepItem{
					{UpgradeStep: new(step), UpgradeStepStatus: new("IN_PROGRESS"), ProgressPercent: new(float64(0))},
				},
			},
		)
		return storage.Write(tx, key, *rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpgradeDomainResponse{
		UpgradeId:        new(id),
		DomainName:       in.DomainName,
		TargetVersion:    in.TargetVersion,
		PerformCheckOnly: new(common.Value(in.PerformCheckOnly)),
		AdvancedOptions:  in.AdvancedOptions,
	}, nil
}

func (s *Service) GetUpgradeStatus(
	ctx context.Context,
	in *api.GetUpgradeStatusRequest,
) (*api.GetUpgradeStatusResponse, error) {
	if in == nil {
		return nil, invalid("DomainName is required")
	}

	rec, err := s.readDomain(ctx, in.DomainName)
	if err != nil {
		return nil, err
	}

	if len(rec.History) == 0 {
		return &api.GetUpgradeStatusResponse{}, nil
	}

	h := rec.History[len(rec.History)-1]
	return &api.GetUpgradeStatusResponse{
		UpgradeName: h.UpgradeName,
		UpgradeStep: h.StepsList[0].UpgradeStep,
		StepStatus:  h.UpgradeStatus,
	}, nil
}

func (s *Service) GetUpgradeHistory(
	ctx context.Context,
	in *api.GetUpgradeHistoryRequest,
) (*api.GetUpgradeHistoryResponse, error) {
	if in == nil {
		return nil, invalid("DomainName is required")
	}

	limit, err := pageLimit(in.MaxResults)
	if err != nil {
		return nil, err
	}

	rec, err := s.readDomain(ctx, in.DomainName)
	if err != nil {
		return nil, err
	}

	items, next, err := common.Page(
		rec.History,
		func(h api.UpgradeHistory) string {
			return fmt.Sprintf("%020.6f-%s", common.Value(h.StartTimestamp), common.Value(h.UpgradeName))
		},
		in.NextToken,
		limit,
		invalid("Invalid NextToken"),
	)
	return &api.GetUpgradeHistoryResponse{UpgradeHistories: items, NextToken: next}, err
}

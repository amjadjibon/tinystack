package securityhub

import (
	"context"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/securityhub"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Catalogue ARNs: https://docs.aws.amazon.com/cli/latest/reference/securityhub/describe-standards.html
// https://docs.aws.amazon.com/securityhub/latest/userguide/cis-aws-foundations-benchmark.html
// https://docs.aws.amazon.com/securityhub/latest/userguide/standards-reference.html
func standards(scope request.Scope) []api.Standard {
	entries := []struct {
		path, name string
		def        bool
	}{
		{
			"standards/aws-foundational-security-best-practices/v/1.0.0",
			"AWS Foundational Security Best Practices v1.0.0",
			true,
		},
		{"ruleset/cis-aws-foundations-benchmark/v/1.2.0", "CIS AWS Foundations Benchmark v1.2.0", true},
		{"standards/cis-aws-foundations-benchmark/v/1.4.0", "CIS AWS Foundations Benchmark v1.4.0", false},
		{"standards/cis-aws-foundations-benchmark/v/3.0.0", "CIS AWS Foundations Benchmark v3.0.0", false},
		{"standards/pci-dss/v/3.2.1", "PCI DSS v3.2.1", false},
		{"standards/nist-800-53/v/5.0.0", "NIST SP 800-53 Rev. 5", false},
	}

	out := make([]api.Standard, 0, len(entries))
	for _, e := range entries {
		value := "arn:aws:securityhub:" + scope.Region + "::" + e.path
		if strings.HasPrefix(e.path, "ruleset/") {
			value = "arn:aws:securityhub:::" + e.path
		}

		out = append(
			out,
			api.Standard{
				StandardsArn:     new(value),
				Name:             new(e.name),
				Description:      new(e.name),
				EnabledByDefault: new(e.def),
			},
		)
	}

	return out
}

func enableStandard(
	tx storage.WriteTx,
	scope request.Scope,
	value string,
) (api.StandardsSubscription, error) {
	for _, standard := range standards(scope) {
		if common.Value(standard.StandardsArn) != value {
			continue
		}

		_, path, _ := strings.Cut(value, "::")
		path = strings.TrimPrefix(path, ":")
		_, path, _ = strings.Cut(path, "/")
		sub := api.StandardsSubscription{
			StandardsArn:             new(value),
			StandardsSubscriptionArn: new(arn(scope, "subscription/"+path)),
			StandardsStatus:          new("READY"),
		}

		return sub, storage.Write(
			tx,
			scope.Key(serviceName, "standard", common.Value(sub.StandardsSubscriptionArn)),
			sub,
		)
	}

	return api.StandardsSubscription{}, invalid("StandardsArn is not in the supported catalogue.")
}

func page[T any](
	items []T,
	id func(T) string,
	token *string,
	maxResults *int64,
) (selected []T, next *string, err error) {
	limit := int(*common.OrElse(maxResults, new(int64(100))))
	if limit < 1 || limit > 100 {
		return nil, nil, invalid("MaxResults must be between 1 and 100.")
	}

	return common.Page(items, id, token, limit, invalid("NextToken is invalid."))
}

func (s *Service) DescribeStandards(
	ctx context.Context,
	in *api.DescribeStandardsRequest,
) (*api.DescribeStandardsResponse, error) {
	out := &api.DescribeStandardsResponse{}
	err := s.transaction(ctx, func(_ storage.WriteTx, scope request.Scope, _ *hubRecord) error {
		var err error
		out.Standards, out.NextToken, err = page(
			standards(scope),
			func(v api.Standard) string { return common.Value(v.StandardsArn) },
			in.NextToken,
			in.MaxResults,
		)
		return err
	})

	return out, err
}

func (s *Service) BatchEnableStandards(
	ctx context.Context,
	in *api.BatchEnableStandardsRequest,
) (*api.BatchEnableStandardsResponse, error) {
	out := &api.BatchEnableStandardsResponse{}
	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *hubRecord) error {
		for _, r := range in.StandardsSubscriptionRequests {
			v, err := enableStandard(tx, scope, common.Value(r.StandardsArn))
			if err != nil {
				return err
			}

			out.StandardsSubscriptions = append(out.StandardsSubscriptions, v)
		}

		return nil
	})

	return out, err
}

func (s *Service) BatchDisableStandards(
	ctx context.Context,
	in *api.BatchDisableStandardsRequest,
) (*api.BatchDisableStandardsResponse, error) {
	out := &api.BatchDisableStandardsResponse{}
	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *hubRecord) error {
		for _, value := range in.StandardsSubscriptionArns {
			k := scope.Key(serviceName, "standard", value)
			sub, found, err := storage.Read[api.StandardsSubscription](tx, k)
			if err != nil {
				return err
			}

			if !found {
				return missing()
			}

			tx.Delete(k)
			sub.StandardsStatus = new("DELETING")
			out.StandardsSubscriptions = append(out.StandardsSubscriptions, *sub)
		}

		return nil
	})

	return out, err
}

func (s *Service) GetEnabledStandards(
	ctx context.Context,
	in *api.GetEnabledStandardsRequest,
) (*api.GetEnabledStandardsResponse, error) {
	out := &api.GetEnabledStandardsResponse{}
	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *hubRecord) error {
		records, err := storage.Records[api.StandardsSubscription](tx, scope.Prefix(serviceName, "standard"))
		if err != nil {
			return err
		}

		items := []api.StandardsSubscription{}
		for _, r := range records {
			if len(in.StandardsSubscriptionArns) == 0 ||
				slices.Contains(in.StandardsSubscriptionArns, common.Value(r.StandardsSubscriptionArn)) {
				items = append(items, *r)
			}
		}

		out.StandardsSubscriptions, out.NextToken, err = page(
			items,
			func(v api.StandardsSubscription) string { return common.Value(v.StandardsSubscriptionArn) },
			in.NextToken,
			in.MaxResults,
		)
		return err
	})

	return out, err
}

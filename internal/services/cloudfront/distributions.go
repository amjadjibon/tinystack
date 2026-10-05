package cloudfront

import (
	"context"
	"fmt"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudfront"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type distributionRecord struct {
	Distribution *api.Distribution `json:"distribution"`
	Tags         map[string]string `json:"tags,omitempty"`
	ETag         string            `json:"etag"`
}

func parseTags(tags *api.Tags) map[string]string {
	if tags == nil {
		return nil
	}
	m := make(map[string]string, len(tags.Items))
	for _, t := range tags.Items {
		if t.Key != nil && t.Value != nil {
			m[*t.Key] = *t.Value
		}
	}
	return m
}

func toSummary(rec *distributionRecord) api.DistributionSummary {
	if rec == nil || rec.Distribution == nil {
		return api.DistributionSummary{}
	}
	cfg := rec.Distribution.DistributionConfig
	s := api.DistributionSummary{
		ARN:              rec.Distribution.ARN,
		DomainName:       rec.Distribution.DomainName,
		Id:               rec.Distribution.Id,
		LastModifiedTime: rec.Distribution.LastModifiedTime,
		Status:           rec.Distribution.Status,
		ETag:             &rec.ETag,
	}
	if cfg != nil {
		s.Aliases = cfg.Aliases
		s.AnycastIpListId = cfg.AnycastIpListId
		s.CacheBehaviors = cfg.CacheBehaviors
		s.Comment = cfg.Comment
		s.ConnectionFunctionAssociation = cfg.ConnectionFunctionAssociation
		s.ConnectionMode = cfg.ConnectionMode
		s.CustomErrorResponses = cfg.CustomErrorResponses
		s.DefaultCacheBehavior = cfg.DefaultCacheBehavior
		s.Enabled = cfg.Enabled
		s.HttpVersion = cfg.HttpVersion
		s.IsIPV6Enabled = cfg.IsIPV6Enabled
		s.OriginGroups = cfg.OriginGroups
		s.Origins = cfg.Origins
		s.PriceClass = cfg.PriceClass
		s.Restrictions = cfg.Restrictions
		s.Staging = cfg.Staging
		s.ViewerCertificate = cfg.ViewerCertificate
		s.ViewerMtlsConfig = cfg.ViewerMtlsConfig
		s.WebACLId = cfg.WebACLId
	}
	return s
}

func (s *Service) createDistribution(
	ctx context.Context, account string, cfg *api.DistributionConfig, tags map[string]string,
) (*api.CreateDistributionResult, error) {
	ref := value(cfg.CallerReference)
	if ref == "" {
		return nil, invalidArgument("CallerReference is required")
	}

	var created *distributionRecord
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		// Check idempotency / caller reference
		if _, exists := tx.Get(key(account, kindReference, ref)); exists {
			return distributionAlreadyExists()
		}

		// Check CNAME / alias collisions
		if cfg.Aliases != nil {
			for _, a := range cfg.Aliases.Items {
				lower := strings.ToLower(a)
				if _, exists := tx.Get(key(account, kindAlias, lower)); exists {
					return cnameAlreadyExists(a)
				}
			}
		}

		// Generate unique ID
		var id string
		for {
			id = newID("E")
			if _, exists := tx.Get(key(account, kindDistribution, id)); !exists {
				break
			}
		}

		etag := newID("E")
		domainName := strings.ToLower(id) + ".cloudfront.net"
		arn := fmt.Sprintf("arn:aws:cloudfront::%s:distribution/%s", account, id)
		now := s.now().UTC()

		dist := &api.Distribution{
			Id:                            &id,
			ARN:                           &arn,
			DomainName:                    &domainName,
			Status:                        new("Deployed"),
			LastModifiedTime:              &now,
			DistributionConfig:            cfg,
			InProgressInvalidationBatches: new(int64(0)),
			ActiveTrustedKeyGroups: &api.ActiveTrustedKeyGroups{
				Enabled:  new(false),
				Quantity: new(int64(0)),
			},
			ActiveTrustedSigners: &api.ActiveTrustedSigners{
				Enabled:  new(false),
				Quantity: new(int64(0)),
			},
		}

		rec := &distributionRecord{
			Distribution: dist,
			Tags:         tags,
			ETag:         etag,
		}

		if writeErr := write(tx, key(account, kindDistribution, id), rec); writeErr != nil {
			return writeErr
		}
		tx.Put(key(account, kindReference, ref), []byte(id))

		if cfg.Aliases != nil {
			for _, a := range cfg.Aliases.Items {
				tx.Put(key(account, kindAlias, strings.ToLower(a)), []byte(id))
			}
		}

		created = rec
		return nil
	})
	if err != nil {
		return nil, err
	}

	location := "https://cloudfront.amazonaws.com/2020-05-31/distribution/" + value(created.Distribution.Id)
	return &api.CreateDistributionResult{
		Distribution: created.Distribution,
		ETag:         &created.ETag,
		Location:     &location,
	}, nil
}

func (s *Service) CreateDistribution(
	ctx context.Context, in *api.CreateDistributionRequest,
) (*api.CreateDistributionResult, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.DistributionConfig == nil {
		return nil, invalidArgument("DistributionConfig is required")
	}

	return s.createDistribution(ctx, m.AccountID, in.DistributionConfig, nil)
}

func (s *Service) CreateDistributionWithTags(
	ctx context.Context, in *api.CreateDistributionWithTagsRequest,
) (*api.CreateDistributionWithTagsResult, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.DistributionConfigWithTags == nil || in.DistributionConfigWithTags.DistributionConfig == nil {
		return nil, invalidArgument("DistributionConfig is required")
	}

	tags := parseTags(in.DistributionConfigWithTags.Tags)
	res, err := s.createDistribution(ctx, m.AccountID, in.DistributionConfigWithTags.DistributionConfig, tags)
	if err != nil {
		return nil, err
	}

	return &api.CreateDistributionWithTagsResult{
		Distribution: res.Distribution,
		ETag:         res.ETag,
		Location:     res.Location,
	}, nil
}

func (s *Service) GetDistribution(
	ctx context.Context, in *api.GetDistributionRequest,
) (*api.GetDistributionResult, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	id := value(in.Id)
	if id == "" {
		return nil, noSuchDistribution("")
	}

	var rec *distributionRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, found, readErr := read[distributionRecord](tx, key(m.AccountID, kindDistribution, id))
		if readErr != nil {
			return readErr
		}
		if !found {
			return noSuchDistribution(id)
		}
		rec = r
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.GetDistributionResult{
		Distribution: rec.Distribution,
		ETag:         &rec.ETag,
	}, nil
}

func (s *Service) GetDistributionConfig(
	ctx context.Context, in *api.GetDistributionConfigRequest,
) (*api.GetDistributionConfigResult, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	id := value(in.Id)
	if id == "" {
		return nil, noSuchDistribution("")
	}

	var rec *distributionRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, found, readErr := read[distributionRecord](tx, key(m.AccountID, kindDistribution, id))
		if readErr != nil {
			return readErr
		}
		if !found {
			return noSuchDistribution(id)
		}
		rec = r
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.GetDistributionConfigResult{
		DistributionConfig: rec.Distribution.DistributionConfig,
		ETag:               &rec.ETag,
	}, nil
}

func (s *Service) UpdateDistribution(
	ctx context.Context, in *api.UpdateDistributionRequest,
) (*api.UpdateDistributionResult, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	id := value(in.Id)
	if id == "" {
		return nil, noSuchDistribution("")
	}
	if in.DistributionConfig == nil {
		return nil, invalidArgument("DistributionConfig is required")
	}

	var updated *distributionRecord
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, found, readErr := read[distributionRecord](tx, key(m.AccountID, kindDistribution, id))
		if readErr != nil {
			return readErr
		}
		if !found {
			return noSuchDistribution(id)
		}

		if in.IfMatch == nil || (*in.IfMatch != "*" && *in.IfMatch != rec.ETag) {
			return invalidIfMatchVersion()
		}

		// Update aliases
		oldAliases := map[string]bool{}
		if rec.Distribution.DistributionConfig != nil && rec.Distribution.DistributionConfig.Aliases != nil {
			for _, a := range rec.Distribution.DistributionConfig.Aliases.Items {
				oldAliases[strings.ToLower(a)] = true
			}
		}

		newAliases := map[string]bool{}
		if in.DistributionConfig.Aliases != nil {
			for _, a := range in.DistributionConfig.Aliases.Items {
				lower := strings.ToLower(a)
				newAliases[lower] = true
				if !oldAliases[lower] {
					existingID, exists := tx.Get(key(m.AccountID, kindAlias, lower))
					if exists && string(existingID) != id {
						return cnameAlreadyExists(a)
					}
				}
			}
		}

		for oldA := range oldAliases {
			if !newAliases[oldA] {
				tx.Delete(key(m.AccountID, kindAlias, oldA))
			}
		}
		for newA := range newAliases {
			tx.Put(key(m.AccountID, kindAlias, newA), []byte(id))
		}

		now := s.now().UTC()
		rec.ETag = newID("E")
		rec.Distribution.DistributionConfig = in.DistributionConfig
		rec.Distribution.LastModifiedTime = &now

		if writeErr := write(tx, key(m.AccountID, kindDistribution, id), rec); writeErr != nil {
			return writeErr
		}
		updated = rec
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateDistributionResult{
		Distribution: updated.Distribution,
		ETag:         &updated.ETag,
	}, nil
}

func (s *Service) DeleteDistribution(
	ctx context.Context, in *api.DeleteDistributionRequest,
) (*api.DeleteDistributionOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	id := value(in.Id)
	if id == "" {
		return nil, noSuchDistribution("")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, found, readErr := read[distributionRecord](tx, key(m.AccountID, kindDistribution, id))
		if readErr != nil {
			return readErr
		}
		if !found {
			return noSuchDistribution(id)
		}

		if in.IfMatch == nil || (*in.IfMatch != "*" && *in.IfMatch != rec.ETag) {
			return invalidIfMatchVersion()
		}

		cfg := rec.Distribution.DistributionConfig
		if cfg != nil && cfg.Enabled != nil && *cfg.Enabled {
			return distributionNotDisabled()
		}

		tx.Delete(key(m.AccountID, kindDistribution, id))

		if cfg != nil && cfg.CallerReference != nil {
			tx.Delete(key(m.AccountID, kindReference, *cfg.CallerReference))
		}

		if cfg != nil && cfg.Aliases != nil {
			for _, a := range cfg.Aliases.Items {
				tx.Delete(key(m.AccountID, kindAlias, strings.ToLower(a)))
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteDistributionOutput{}, nil
}

func (s *Service) ListDistributions(
	ctx context.Context, in *api.ListDistributionsRequest,
) (*api.ListDistributionsResult, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var items []api.DistributionSummary
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		prefix := m.AccountID + "/" + service + "/" + kindDistribution + "/"
		keys := tx.Keys(prefix)
		slices.Sort(keys)

		for _, k := range keys {
			rec, found, readErr := read[distributionRecord](tx, k)
			if readErr != nil {
				return readErr
			}
			if found {
				items = append(items, toSummary(rec))
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	maxItems := int64(100)
	if in != nil && in.MaxItems != nil && *in.MaxItems > 0 {
		maxItems = *in.MaxItems
	}

	marker := ""
	if in != nil && in.Marker != nil {
		marker = *in.Marker
	}

	start := 0
	if marker != "" {
		for i := range items {
			if value(items[i].Id) > marker {
				start = i
				break
			}
			if i == len(items)-1 {
				start = len(items)
			}
		}
	}

	page := items[start:]
	truncated := false
	var nextMarker *string
	if int64(len(page)) > maxItems {
		truncated = true
		page = page[:maxItems]
		nextMarker = page[len(page)-1].Id
	}

	quantity := int64(len(page))
	return &api.ListDistributionsResult{
		DistributionList: &api.DistributionList{
			IsTruncated: &truncated,
			Items:       page,
			Marker:      &marker,
			MaxItems:    &maxItems,
			NextMarker:  nextMarker,
			Quantity:    &quantity,
		},
	}, nil
}

package s3

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// aliasBucket resolves S3 Control's partition-wide alias index in the current snapshot.
func aliasBucket(tx storage.ReadTx, scope request.Scope, name string) (string, error) {
	if !strings.HasSuffix(name, "-s3alias") {
		return name, nil
	}

	r, found, err := storage.Read[struct{ Bucket, Account, Region, Name string }](tx, "aws/s3control/aliases/"+name)
	if err != nil {
		return "", err
	}

	if !found {
		return "", noSuchBucket()
	}

	if r.Region != scope.Region {
		return "", fail(301, "PermanentRedirect", "Access point is in "+r.Region)
	}

	return r.Bucket, nil
}

// accessPointTargets requires authorization for both the bucket and access point.
func (s *Service) accessPointTargets(
	ctx context.Context,
	targets []catalog.AuthorizationResource,
) ([]catalog.AuthorizationResource, error) {
	out := make([]catalog.AuthorizationResource, 0, len(targets)*2)
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, target := range targets {
			path := strings.TrimPrefix(target.ARN, "arn:aws:s3:::")
			name, key, object := strings.Cut(path, "/")
			if !strings.HasSuffix(name, "-s3alias") {
				out = append(out, target)
				continue
			}

			alias, found, txErr := storage.Read[struct{ Bucket, Account, Region, Name string }](
				tx,
				"aws/s3control/aliases/"+name,
			)
			if txErr != nil {
				return txErr
			}

			if !found {
				return noSuchBucket()
			}

			scope := request.Scope{AccountID: alias.Account, Region: alias.Region}
			ap, found, txErr := storage.Read[struct {
				Tags                           map[string]string
				AccessPointArn                 *string
				PublicAccessBlockConfiguration *api.PublicAccessBlockConfiguration
				Policy                         string
			}](tx, scope.Key("s3control", "accesspoint", alias.Name))
			if txErr != nil {
				return txErr
			}

			if !found {
				return noSuchBucket()
			}

			target.ARN = "arn:aws:s3:::" + alias.Bucket
			if object {
				target.ARN += "/" + key
			}

			target.Values = map[string][]string{
				"s3:dataaccesspointarn":     {common.Value(ap.AccessPointArn)},
				"s3:dataaccesspointaccount": {alias.Account},
			}

			out = append(out, target)
			point := target
			point.ARN = common.Value(ap.AccessPointArn)
			if object {
				point.ARN += "/object/" + key
			}

			point.Policy = ap.Policy
			m, txErr := request.Regional(ctx)
			if txErr != nil {
				return txErr
			}

			bucketKey, txErr := authorizationBucketKey(tx, alias.Account, alias.Region, alias.Bucket)
			if txErr != nil {
				return txErr
			}

			meta, _, txErr := storage.Read[bucketMeta](tx, bucketKey)
			if txErr != nil {
				return txErr
			}

			var bucketConfig *api.PublicAccessBlockConfiguration
			if meta != nil {
				bucketConfig = meta.PublicAccessBlock
			}

			point.Policy, txErr = restrictPublicPolicy(
				tx,
				scope,
				m.Principal,
				ap.Policy,
				ap.PublicAccessBlockConfiguration,
				bucketConfig,
			)
			if txErr != nil {
				return txErr
			}

			out = append(out, point)
		}

		return nil
	})
	return out, err
}

// BucketScope resolves a real bucket's owning scope without requiring a regional endpoint.
func (s *Service) BucketScope(ctx context.Context, name, owner string) (request.Scope, error) {
	var scope request.Scope
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		index, found, txErr := storage.Read[bucketIndex](tx, indexPrefix+name)
		if txErr != nil {
			return txErr
		}

		if !found {
			return noSuchBucket()
		}

		if index.Account != owner {
			return accessDenied()
		}

		scope = request.Scope{AccountID: index.Account, Region: index.Region}
		return nil
	})
	return scope, err
}

// PolicyIsPublicOrEmpty handles resources with no installed policy.
func PolicyIsPublicOrEmpty(document string) (bool, error) {
	if document == "" {
		return false, nil
	}

	return PolicyIsPublic(document)
}

func bucketHasAccessPoints(tx storage.ReadTx, scope request.Scope, name string) (bool, error) {
	points, err := storage.Records[struct{ Bucket *string }](tx, scope.Prefix("s3control", "accesspoint"))
	if err != nil {
		return false, err
	}

	for _, point := range points {
		if common.Value(point.Bucket) == name {
			return true, nil
		}
	}

	scope.Region = ""
	mraps, err := storage.Records[struct{ Regions []struct{ Bucket *string } }](tx, scope.Prefix("s3control", "mrap"))
	if err != nil {
		return false, err
	}

	for _, point := range mraps {
		for _, region := range point.Regions {
			if common.Value(region.Bucket) == name {
				return true, nil
			}
		}
	}

	return false, nil
}

// restrictPublicPolicy enforces the account/bucket/access-point restriction in the authorization snapshot.
func restrictPublicPolicy(
	tx storage.ReadTx,
	scope request.Scope,
	principal *request.Principal,
	document string,
	configs ...*api.PublicAccessBlockConfiguration,
) (string, error) {
	if principal != nil && principal.Account == scope.AccountID {
		return document, nil
	}

	account, _, err := storage.Read[api.PublicAccessBlockConfiguration](
		tx,
		(request.Scope{AccountID: scope.AccountID}).Key("s3control", "publicaccessblock", "account"),
	)
	if err != nil {
		return "", err
	}

	configs = append(configs, account)
	restricted := false
	for _, config := range configs {
		restricted = restricted || (config != nil && common.Value(config.RestrictPublicBuckets))
	}

	if !restricted {
		return document, nil
	}

	public, err := PolicyIsPublicOrEmpty(document)
	if err != nil {
		return "", err
	}

	if public {
		return `{"Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"*"}]}`, nil
	}

	return document, nil
}

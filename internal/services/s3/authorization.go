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

const actionBypassGovernanceRetention = "s3:BypassGovernanceRetention" // #nosec G101

// AuthorizationResources separates bucket, object, copy, and multi-delete targets.
func (s *Service) authorizationTargets(
	ctx context.Context,
	operation string,
	input any,
) ([]catalog.AuthorizationResource, error) {
	bucket := catalog.InputString(input, "Bucket")
	if bucket == "" {
		return nil, nil
	}

	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	base := "arn:aws:s3:::" + bucket
	key := catalog.InputString(input, "Key")
	if key == "" && operation != "DeleteObjects" {
		target := catalog.AuthorizationResource{ARN: base, TagsKnown: true}
		err := s.store.View(ctx, func(tx storage.ReadTx) error {
			bucketKey, err := authorizationBucketKey(tx, m.AccountID, m.Region, bucket)
			if err != nil {
				return err
			}

			meta, _, readErr := storage.Read[bucketMeta](tx, bucketKey)
			if meta != nil {
				target.Tags = common.CloneTags(meta.Tags)
			}

			return readErr
		})
		return []catalog.AuthorizationResource{target}, err
	}

	if in, ok := input.(*api.DeleteObjectsRequest); ok {
		var out []catalog.AuthorizationResource
		var versions []string
		if in.Delete != nil {
			for _, object := range in.Delete.Objects {
				versions = append(versions, common.Value(object.VersionId))
				action := "s3:DeleteObject"
				if common.Value(object.VersionId) != "" {
					action = "s3:DeleteObjectVersion"
				}

				out = append(
					out,
					catalog.AuthorizationResource{ARN: base + "/" + common.Value(object.Key), Action: action},
				)
				if common.Value(in.BypassGovernanceRetention) {
					versions = append(versions, common.Value(object.VersionId))
					out = append(
						out,
						catalog.AuthorizationResource{
							ARN:    base + "/" + common.Value(object.Key),
							Action: actionBypassGovernanceRetention,
						},
					)
				}
			}
		}

		return out, s.authorizationObjectTags(ctx, &m.Scope, out, versions)
	}

	action := ""
	if catalog.InputString(input, "VersionId") != "" {
		switch operation {
		case "GetObject", "HeadObject":
			action = "s3:GetObjectVersion"
		case "GetObjectAttributes":
			action = "s3:GetObjectVersionAttributes"
		case "DeleteObject":
			action = "s3:DeleteObjectVersion"
		case "GetObjectTagging":
			action = "s3:GetObjectVersionTagging"
		case "GetObjectAcl":
			action = "s3:GetObjectVersionAcl"
		case "PutObjectAcl":
			action = "s3:PutObjectVersionAcl"
		case "PutObjectTagging":
			action = "s3:PutObjectVersionTagging"
		case "DeleteObjectTagging":
			action = "s3:DeleteObjectVersionTagging"
		}
	}

	out := []catalog.AuthorizationResource{{ARN: base + "/" + key, Action: action}}
	versions := []string{catalog.InputString(input, "VersionId")}
	if in, ok := input.(*api.DeleteObjectRequest); ok && common.Value(in.BypassGovernanceRetention) {
		out = append(out, catalog.AuthorizationResource{ARN: base + "/" + key, Action: actionBypassGovernanceRetention})
		versions = append(versions, common.Value(in.VersionId))
	}

	if in, ok := input.(*api.PutObjectRetentionRequest); ok && common.Value(in.BypassGovernanceRetention) {
		out = append(out, catalog.AuthorizationResource{ARN: base + "/" + key, Action: actionBypassGovernanceRetention})
		versions = append(versions, common.Value(in.VersionId))
	}

	if operation == "CopyObject" || operation == "UploadPartCopy" {
		sourceBucket, sourceKey, version, err := copySource(new(catalog.InputString(input, "CopySource")))
		if err != nil {
			return nil, err
		}

		path := sourceBucket + "/" + sourceKey
		action := "s3:GetObject"
		versions = append(versions, version)
		if version != "" {
			action = "s3:GetObjectVersion"
		}

		out = append(out, catalog.AuthorizationResource{ARN: "arn:aws:s3:::" + path, Action: action})
	}

	return out, s.authorizationObjectTags(ctx, &m.Scope, out, versions)
}

// authorizationObjectTags reads metadata only, including historical versions,
// without reading or copying object bodies. Missing objects have absent tags.
func (s *Service) authorizationObjectTags(
	ctx context.Context, scope *request.Scope, targets []catalog.AuthorizationResource, versions []string,
) error {
	return s.store.View(ctx, func(tx storage.ReadTx) error {
		for i := range targets {
			path := strings.TrimPrefix(targets[i].ARN, "arn:aws:s3:::")
			bucket, objectName, _ := strings.Cut(path, "/")
			bucketKey, err := authorizationBucketKey(tx, scope.AccountID, scope.Region, bucket)
			if err != nil {
				return err
			}

			targets[i].TagsKnown = true
			stored, _, err := storage.Read[object](tx, bucketKey+"/o/"+objectName)
			if err != nil {
				return err
			}

			if stored != nil && (versions[i] == "" || versionOf(stored) == versions[i]) {
				targets[i].Tags = common.CloneTags(stored.Tags)
				continue
			}

			if versions[i] == "" {
				continue
			}

			old, err := history(tx, bucketKey, objectName)
			if err != nil {
				return err
			}

			for j := range old {
				if versionOf(&old[j]) == versions[i] {
					targets[i].Tags = common.CloneTags(old[j].Tags)
					break
				}
			}
		}

		return nil
	})
}

// AuthorizationResources snapshots owner and policy alongside each resolved target.
func (s *Service) AuthorizationResources(
	ctx context.Context,
	operation string,
	input any,
) ([]catalog.AuthorizationResource, error) {
	targets, err := s.authorizationTargets(ctx, operation, input)
	if err != nil {
		return nil, err
	}

	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for i := range targets {
			bucket, _, _ := strings.Cut(strings.TrimPrefix(targets[i].ARN, "arn:aws:s3:::"), "/")
			key, resolveErr := authorizationBucketKey(tx, m.AccountID, m.Region, bucket)
			if resolveErr != nil {
				return resolveErr
			}

			targets[i].OwnerAccount = strings.SplitN(key, "/", 2)[0]
			meta, _, readErr := storage.Read[bucketMeta](tx, key)
			if readErr != nil {
				return readErr
			}

			if meta != nil {
				policy, policyErr := restrictPublicPolicy(
					tx,
					request.Scope{AccountID: targets[i].OwnerAccount, Region: m.Region},
					m.Principal,
					meta.Policy,
					meta.PublicAccessBlock,
				)
				if policyErr != nil {
					return policyErr
				}

				targets[i].Policy = policy
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return s.accessPointTargets(ctx, targets)
}

func authorizationBucketKey(tx storage.ReadTx, account, region, bucket string) (string, error) {
	resolved, err := aliasBucket(tx, request.Scope{AccountID: account, Region: region}, bucket)
	if err != nil {
		return "", err
	}

	bucket = resolved
	var index bucketIndex
	found, err := readJSON(tx, indexPrefix+bucket, &index)
	if err != nil {
		return "", err
	}

	if found {
		account, region = index.Account, index.Region
	}

	return account + "/" + region + "/s3/" + bucket, nil
}

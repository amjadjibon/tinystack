package s3

import (
	"context"
	"net/url"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// AuthorizationResources separates bucket, object, copy, and multi-delete targets.
func (s *Service) AuthorizationResources(
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
			meta, _, readErr := storage.Read[bucketMeta](tx, m.AccountID+"/"+m.Region+"/s3/"+bucket)
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
			}
		}

		return out, s.authorizationObjectTags(ctx, &m.Scope, out, versions)
	}

	action := ""
	if catalog.InputString(input, "VersionId") != "" {
		switch operation {
		case "GetObject", "HeadObject":
			action = "s3:GetObjectVersion"
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
	if operation == "CopyObject" || operation == "UploadPartCopy" {
		source, err := url.QueryUnescape(catalog.InputString(input, "CopySource"))
		if err != nil {
			return nil, err
		}

		source = strings.TrimPrefix(source, "/")
		path, query, _ := strings.Cut(source, "?")
		action := "s3:GetObject"
		parsed, _ := url.ParseQuery(query)
		versions = append(versions, parsed.Get("versionId"))
		if parsed.Get("versionId") != "" {
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
			bucketKey := scope.AccountID + "/" + scope.Region + "/s3/" + bucket
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

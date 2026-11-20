package glue

import (
	"context"
	"maps"
	"regexp"
	"slices"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var metadataPattern = regexp.MustCompile(`^[a-zA-Z0-9+=._/@-]+$`)

func validSchemaMetadata(pair *api.MetadataKeyValuePair) bool {
	if pair == nil {
		return false
	}

	k, v := common.Value(pair.MetadataKey), common.Value(pair.MetadataValue)
	return len(k) <= 128 && len(v) <= 256 && metadataPattern.MatchString(k) && metadataPattern.MatchString(v)
}

func hasSchemaMetadata(info api.MetadataInfo, value string) bool {
	return value == common.Value(info.MetadataValue) ||
		slices.ContainsFunc(info.OtherMetadataValueList, func(v api.OtherMetadataValueListItem) bool {
			return value == common.Value(v.MetadataValue)
		})
}

func (s *Service) PutSchemaVersionMetadata(
	ctx context.Context, in *api.PutSchemaVersionMetadataInput,
) (*api.PutSchemaVersionMetadataResponse, error) {
	if !validSchemaMetadata(in.MetadataKeyValue) {
		return nil, invalid("Invalid schema version metadata key or value.")
	}

	pair := in.MetadataKeyValue
	out := &api.PutSchemaVersionMetadataResponse{}
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		v, err := findSchemaVersion(tx, scope, in.SchemaVersionId, in.SchemaId, in.SchemaVersionNumber)
		if err != nil {
			return err
		}

		schema, err := loadSchema(tx, scope, v.Schema)
		if err != nil {
			return err
		}

		*out = api.PutSchemaVersionMetadataResponse{
			SchemaArn: schema.SchemaArn, SchemaName: schema.SchemaName, RegistryName: schema.RegistryName,
			LatestVersion: new(*schema.LatestSchemaVersion == *v.VersionNumber), VersionNumber: v.VersionNumber,
			SchemaVersionId: v.SchemaVersionId, MetadataKey: pair.MetadataKey, MetadataValue: pair.MetadataValue,
		}

		info := v.Metadata[*pair.MetadataKey]
		if hasSchemaMetadata(info, *pair.MetadataValue) {
			return nil
		}

		count := len(v.Metadata)
		for _, info := range v.Metadata {
			count += len(info.OtherMetadataValueList)
		}

		if count >= 10 {
			return awserror.New(
				"ResourceNumberLimitExceededException",
				"A schema version can have at most 10 metadata pairs.",
			)
		}

		if v.Metadata == nil {
			v.Metadata = map[string]api.MetadataInfo{}
		}

		if info.MetadataValue != nil {
			info.OtherMetadataValueList = append([]api.OtherMetadataValueListItem{{
				MetadataValue: info.MetadataValue, CreatedTime: info.CreatedTime,
			}}, info.OtherMetadataValueList...)
		}

		info.MetadataValue, info.CreatedTime = pair.MetadataValue, new(s.now().UTC().Format(time.RFC3339Nano))
		v.Metadata[*pair.MetadataKey] = info
		return storage.Write(tx, schemaVersionKey(scope, v.Schema, *v.VersionNumber), v)
	})
	return out, err
}

func (s *Service) QuerySchemaVersionMetadata(
	ctx context.Context, in *api.QuerySchemaVersionMetadataInput,
) (*api.QuerySchemaVersionMetadataResponse, error) {
	maximum := common.Value(common.OrElse(in.MaxResults, new(int64(25))))
	if maximum < 1 || maximum > 50 {
		return nil, invalid("MaxResults must be between 1 and 50.")
	}

	for i := range in.MetadataList {
		if !validSchemaMetadata(&in.MetadataList[i]) {
			return nil, invalid("Invalid schema version metadata filter.")
		}
	}

	out := &api.QuerySchemaVersionMetadataResponse{MetadataInfoMap: map[string]api.MetadataInfo{}}
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		v, err := findSchemaVersion(tx, scope, in.SchemaVersionId, in.SchemaId, in.SchemaVersionNumber)
		if err != nil {
			return err
		}

		keys := slices.Sorted(maps.Keys(v.Metadata))
		if len(in.MetadataList) > 0 {
			keys = slices.DeleteFunc(keys, func(key string) bool {
				return !slices.ContainsFunc(in.MetadataList, func(pair api.MetadataKeyValuePair) bool {
					return *pair.MetadataKey == key && hasSchemaMetadata(v.Metadata[key], *pair.MetadataValue)
				})
			})
		}

		selected, next, err := common.Page(keys, func(key string) string { return key },
			in.NextToken, int(maximum), invalid("Invalid NextToken."))
		if err != nil {
			return err
		}

		out.NextToken, out.SchemaVersionId = next, v.SchemaVersionId
		for _, key := range selected {
			out.MetadataInfoMap[key] = v.Metadata[key]
		}

		return nil
	})
	return out, err
}

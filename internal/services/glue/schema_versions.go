package glue

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const schemaVersionLimit = 100_000

type schemaVersionRecord struct {
	api.GetSchemaVersionResponse
	Metadata      map[string]api.MetadataInfo
	Schema        string
	DefinitionKey string
}

func schemaVersionPrefix(scope request.Scope, schema string) string {
	return scope.Key(service, "schema-version", schema+"/")
}

func schemaVersionKey(scope request.Scope, schema string, number int64) string {
	return schemaVersionPrefix(scope, schema) + fmt.Sprintf("%06d", number)
}

func schemaVersionIDKey(scope request.Scope, id string) string {
	return scope.Key(service, "schema-version-id", id)
}

func schemaDefinitionKey(scope request.Scope, schema, hash string) string {
	return scope.Key(service, "schema-definition", schema+"/"+hash)
}

func writeSchemaVersion(
	tx storage.WriteTx, scope request.Scope, n string, schema *schemaRecord, definition, hash, now string,
) (*schemaVersionRecord, error) {
	number := *schema.NextSchemaVersion
	if number > schemaVersionLimit {
		return nil, awserror.New("ResourceNumberLimitExceededException", "The schema version limit has been reached.")
	}

	id := uuid.NewV4().String()
	version := &schemaVersionRecord{
		GetSchemaVersionResponse: api.GetSchemaVersionResponse{
			SchemaVersionId:  new(id),
			SchemaDefinition: new(definition),
			DataFormat:       schema.DataFormat,
			SchemaArn:        schema.SchemaArn,
			VersionNumber:    new(number),
			Status:           new(schemaAvailable),
			CreatedTime:      new(now),
		},
		Schema: n, DefinitionKey: hash,
	}
	key := schemaVersionKey(scope, n, number)
	if err := storage.Write(tx, key, version); err != nil {
		return nil, err
	}

	if err := storage.Write(tx, schemaVersionIDKey(scope, id), key); err != nil {
		return nil, err
	}

	if err := storage.Write(tx, schemaDefinitionKey(scope, n, hash), key); err != nil {
		return nil, err
	}

	if schema.SchemaCheckpoint == nil {
		schema.SchemaCheckpoint = new(number)
	}

	schema.LatestSchemaVersion, schema.NextSchemaVersion, schema.UpdatedTime = new(number), new(number+1), new(now)
	return version, nil
}

func findSchemaVersion(
	tx storage.ReadTx, scope request.Scope, id *string, schemaID *api.SchemaId, number *api.SchemaVersionNumber,
) (*schemaVersionRecord, error) {
	var key string
	if id != nil {
		if schemaID != nil || number != nil {
			return nil, invalid("Use SchemaVersionId by itself.")
		}

		parsed, err := uuid.Parse(*id)
		if err != nil || parsed.String() != *id {
			return nil, invalid("SchemaVersionId must be a lowercase UUID.")
		}

		index, found, err := storage.Read[string](tx, schemaVersionIDKey(scope, *id))
		if err != nil || !found {
			return nil, common.ErrorsOr(err, notFound("Schema version %s not found.", *id))
		}

		key = *index
	} else {
		if number == nil || common.Value(number.LatestVersion) == (number.VersionNumber != nil) {
			return nil, invalid("Provide LatestVersion=true or a VersionNumber.")
		}

		n, err := schemaIdentifier(scope, schemaID)
		if err != nil {
			return nil, err
		}

		schema, err := loadSchema(tx, scope, n)
		if err != nil {
			return nil, err
		}

		selected := number.VersionNumber
		if common.Value(number.LatestVersion) {
			selected = schema.LatestSchemaVersion
			if selected == nil {
				return nil, notFound("Schema %s has no versions.", n)
			}
		}

		if *selected < 1 || *selected > schemaVersionLimit {
			return nil, invalid("VersionNumber must be between 1 and %d.", schemaVersionLimit)
		}

		key = schemaVersionKey(scope, n, *selected)
	}

	v, found, err := storage.Read[schemaVersionRecord](tx, key)
	if err != nil || !found {
		return nil, common.ErrorsOr(err, notFound("Schema version not found."))
	}

	return v, nil
}

func (s *Service) RegisterSchemaVersion(
	ctx context.Context, in *api.RegisterSchemaVersionInput,
) (*api.RegisterSchemaVersionResponse, error) {
	out := &api.RegisterSchemaVersionResponse{}
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		n, err := schemaIdentifier(scope, in.SchemaId)
		if err != nil {
			return err
		}

		schema, err := loadSchema(tx, scope, n)
		if err != nil {
			return err
		}

		definition := common.Value(in.SchemaDefinition)
		hash, shape, err := validatedSchema(*schema.DataFormat, *schema.Compatibility, definition)
		if err != nil {
			return err
		}

		key, found, err := storage.Read[string](tx, schemaDefinitionKey(scope, n, hash))
		if err != nil {
			return err
		}

		if found {
			v, _, readErr := storage.Read[schemaVersionRecord](tx, *key)
			if readErr != nil {
				return readErr
			}

			*out = api.RegisterSchemaVersionResponse{
				SchemaVersionId: v.SchemaVersionId, VersionNumber: v.VersionNumber, Status: v.Status,
			}
			return nil
		}

		if compatibilityErr := checkSchemaEvolution(tx, scope, n, schema, shape); compatibilityErr != nil {
			return compatibilityErr
		}

		now := s.now().UTC().Format(time.RFC3339Nano)
		v, err := writeSchemaVersion(tx, scope, n, schema, definition, hash, now)
		if err != nil {
			return err
		}

		*out = api.RegisterSchemaVersionResponse{
			SchemaVersionId: v.SchemaVersionId, VersionNumber: v.VersionNumber, Status: v.Status,
		}
		return storage.Write(tx, scope.Key(service, kindSchema, n), schema)
	})
	return out, err
}

func (s *Service) GetSchemaVersion(
	ctx context.Context, in *api.GetSchemaVersionInput,
) (*api.GetSchemaVersionResponse, error) {
	out := &api.GetSchemaVersionResponse{}
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		v, err := findSchemaVersion(tx, scope, in.SchemaVersionId, in.SchemaId, in.SchemaVersionNumber)
		if err != nil {
			return err
		}

		*out = v.GetSchemaVersionResponse
		return nil
	})
	return out, err
}

func (s *Service) ListSchemaVersions(
	ctx context.Context, in *api.ListSchemaVersionsInput,
) (*api.ListSchemaVersionsResponse, error) {
	out := &api.ListSchemaVersionsResponse{Schemas: []api.SchemaVersionListItem{}}
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		n, err := schemaIdentifier(scope, in.SchemaId)
		if err != nil {
			return err
		}

		if _, readErr := loadSchema(tx, scope, n); readErr != nil {
			return readErr
		}

		prefix := schemaVersionPrefix(scope, n)
		keys, next, err := registryPage(tx.Keys(prefix), prefix, in.NextToken, in.MaxResults)
		if err != nil {
			return err
		}

		out.NextToken = next
		for _, key := range keys {
			v, _, err := storage.Read[schemaVersionRecord](tx, key)
			if err != nil {
				return err
			}

			out.Schemas = append(out.Schemas, api.SchemaVersionListItem{
				SchemaArn: v.SchemaArn, SchemaVersionId: v.SchemaVersionId, VersionNumber: v.VersionNumber,
				Status: v.Status, CreatedTime: v.CreatedTime,
			})
		}

		return nil
	})
	return out, err
}

func (s *Service) GetSchemaByDefinition(
	ctx context.Context, in *api.GetSchemaByDefinitionInput,
) (*api.GetSchemaByDefinitionResponse, error) {
	out := &api.GetSchemaByDefinitionResponse{}
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		n, err := schemaIdentifier(scope, in.SchemaId)
		if err != nil {
			return err
		}

		schema, err := loadSchema(tx, scope, n)
		if err != nil {
			return err
		}

		hash, _, err := validatedSchema(*schema.DataFormat, *schema.Compatibility, common.Value(in.SchemaDefinition))
		if err != nil {
			return err
		}

		key, found, err := storage.Read[string](tx, schemaDefinitionKey(scope, n, hash))
		if err != nil || !found {
			return common.ErrorsOr(err, notFound("Schema definition not found."))
		}

		v, _, err := storage.Read[schemaVersionRecord](tx, *key)
		if err != nil {
			return err
		}

		*out = api.GetSchemaByDefinitionResponse{
			SchemaVersionId: v.SchemaVersionId, SchemaArn: v.SchemaArn, DataFormat: v.DataFormat,
			Status: v.Status, CreatedTime: v.CreatedTime,
		}
		return nil
	})
	return out, err
}

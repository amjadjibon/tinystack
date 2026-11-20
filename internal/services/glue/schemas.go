package glue

import (
	"context"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type schemaRecord struct {
	api.GetSchemaResponse
	Tags map[string]string
}

func schemaIdentifier(scope request.Scope, id *api.SchemaId) (string, error) {
	if id == nil || (id.SchemaArn == nil) == (id.SchemaName == nil) || id.SchemaArn != nil && id.RegistryName != nil {
		return "", invalid("Provide a SchemaArn or a SchemaName with an optional RegistryName.")
	}

	reg, n := common.Value(common.OrElse(id.RegistryName, new(defaultRegistry))), common.Value(id.SchemaName)
	if id.SchemaArn != nil {
		rest, found := strings.CutPrefix(*id.SchemaArn, glueResourcePrefix(scope, kindSchema))
		if !found {
			return "", notFound("Schema %s not found.", *id.SchemaArn)
		}

		reg, n, _ = strings.Cut(rest, "/")
	}

	if !registryNamePattern.MatchString(reg) || !registryNamePattern.MatchString(n) {
		return "", invalid("Invalid schema or registry name.")
	}

	return reg + "/" + n, nil
}

func loadSchema(tx storage.ReadTx, scope request.Scope, n string) (*schemaRecord, error) {
	record, found, err := storage.Read[schemaRecord](tx, scope.Key(service, kindSchema, n))
	if err != nil || !found {
		return nil, common.ErrorsOr(err, notFound("Schema %s not found.", n))
	}

	return record, nil
}

func (s *Service) CreateSchema(ctx context.Context, in *api.CreateSchemaInput) (*api.CreateSchemaResponse, error) {
	n, format := common.Value(in.SchemaName), common.Value(in.DataFormat)
	mode := common.Value(common.OrElse(in.Compatibility, new(compatNone)))
	if !registryNamePattern.MatchString(n) || len(common.Value(in.Description)) > 2048 {
		return nil, invalid("Invalid schema name or description.")
	}

	if err := checkTags(in.Tags); err != nil {
		return nil, err
	}

	if err := checkSchemaMode(format, mode); err != nil {
		return nil, err
	}

	var definition string
	if in.SchemaDefinition != nil {
		var err error
		definition, _, err = validatedSchema(format, mode, *in.SchemaDefinition)
		if err != nil {
			return nil, err
		}
	}

	id := common.OrElse(in.RegistryId, &api.RegistryId{RegistryName: new(defaultRegistry)})
	out := &api.CreateSchemaResponse{}
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		reg, err := registryIdentifier(scope, id)
		if err != nil {
			return err
		}

		if reg == defaultRegistry && id.RegistryArn == nil {
			if _, found := tx.Get(scope.Key(service, kindRegistry, reg)); !found {
				if _, createErr := s.newRegistry(tx, scope, reg, nil, nil); createErr != nil {
					return createErr
				}
			}
		}

		registry, err := findRegistry(tx, scope, id)
		if err != nil {
			return err
		}

		identifier := reg + "/" + n
		key := scope.Key(service, kindSchema, identifier)
		if _, found := tx.Get(key); found {
			return alreadyExists("Schema %s already exists.", identifier)
		}

		now := s.now().UTC().Format(time.RFC3339Nano)
		record := &schemaRecord{
			GetSchemaResponse: api.GetSchemaResponse{
				RegistryName: new(reg), RegistryArn: registry.RegistryArn,
				SchemaName: new(n), SchemaArn: new(glueResourcePrefix(scope, kindSchema) + identifier),
				Description: in.Description, DataFormat: new(format), Compatibility: new(mode),
				NextSchemaVersion: new(int64(1)), SchemaStatus: new(schemaAvailable),
				CreatedTime: new(now), UpdatedTime: new(now),
			},
			Tags: in.Tags,
		}
		if in.SchemaDefinition != nil {
			v, err := writeSchemaVersion(tx, scope, identifier, record, *in.SchemaDefinition, definition, now)
			if err != nil {
				return err
			}

			out.SchemaVersionId, out.SchemaVersionStatus = v.SchemaVersionId, v.Status
		}

		out.RegistryName, out.RegistryArn = record.RegistryName, record.RegistryArn
		out.SchemaName, out.SchemaArn = record.SchemaName, record.SchemaArn
		out.Description, out.DataFormat, out.Compatibility = record.Description, record.DataFormat, record.Compatibility
		out.SchemaCheckpoint, out.LatestSchemaVersion = record.SchemaCheckpoint, record.LatestSchemaVersion
		out.NextSchemaVersion, out.SchemaStatus, out.Tags = record.NextSchemaVersion, record.SchemaStatus, record.Tags
		return storage.Write(tx, key, record)
	})
	return out, err
}

func (s *Service) GetSchema(ctx context.Context, in *api.GetSchemaInput) (*api.GetSchemaResponse, error) {
	out := &api.GetSchemaResponse{}
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		n, err := schemaIdentifier(scope, in.SchemaId)
		if err != nil {
			return err
		}

		record, err := loadSchema(tx, scope, n)
		if err != nil {
			return err
		}

		*out = record.GetSchemaResponse
		return nil
	})
	return out, err
}

func (s *Service) ListSchemas(ctx context.Context, in *api.ListSchemasInput) (*api.ListSchemasResponse, error) {
	out := &api.ListSchemasResponse{Schemas: []api.SchemaListItem{}}
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		prefix := scope.Prefix(service, kindSchema)
		if in.RegistryId != nil {
			reg, err := findRegistry(tx, scope, in.RegistryId)
			if err != nil {
				return err
			}

			prefix += *reg.RegistryName + "/"
		}

		keys, next, err := registryPage(tx.Keys(prefix), prefix, in.NextToken, in.MaxResults)
		if err != nil {
			return err
		}

		out.NextToken = next
		for _, key := range keys {
			sc, _, err := storage.Read[schemaRecord](tx, key)
			if err != nil {
				return err
			}

			out.Schemas = append(out.Schemas, api.SchemaListItem{
				RegistryName: sc.RegistryName, SchemaName: sc.SchemaName, SchemaArn: sc.SchemaArn,
				Description: sc.Description, SchemaStatus: sc.SchemaStatus, CreatedTime: sc.CreatedTime,
				UpdatedTime: sc.UpdatedTime,
			})
		}

		return nil
	})
	return out, err
}

func deleteSchema(tx storage.WriteTx, scope request.Scope, n string) error {
	for _, key := range tx.Keys(schemaVersionPrefix(scope, n)) {
		version, _, err := storage.Read[schemaVersionRecord](tx, key)
		if err != nil {
			return err
		}

		tx.Delete(schemaVersionIDKey(scope, *version.SchemaVersionId))
		tx.Delete(schemaDefinitionKey(scope, n, version.DefinitionKey))
		tx.Delete(key)
	}

	tx.Delete(scope.Key(service, kindSchema, n))
	return nil
}

func (s *Service) DeleteSchema(ctx context.Context, in *api.DeleteSchemaInput) (*api.DeleteSchemaResponse, error) {
	out := &api.DeleteSchemaResponse{}
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		n, err := schemaIdentifier(scope, in.SchemaId)
		if err != nil {
			return err
		}

		record, err := loadSchema(tx, scope, n)
		if err != nil {
			return err
		}

		*out = api.DeleteSchemaResponse{
			SchemaArn: record.SchemaArn, SchemaName: record.SchemaName,
			Status: new(schemaDeleting),
		}
		return deleteSchema(tx, scope, n)
	})
	return out, err
}

package integration_test

import (
	"context"
	"testing"
	"uuid"

	"github.com/aws/aws-sdk-go-v2/service/glue"
	gt "github.com/aws/aws-sdk-go-v2/service/glue/types"
)

func TestGlueSchemaRegistrySerializerFlow(t *testing.T) {
	ctx := context.Background()
	c := glueClient(server(t))
	reg, err := c.CreateRegistry(ctx, &glue.CreateRegistryInput{RegistryName: new("events")})
	if err != nil {
		t.Fatal(err)
	}

	regID := &gt.RegistryId{RegistryArn: reg.RegistryArn}
	info, err := c.GetRegistry(ctx, &glue.GetRegistryInput{RegistryId: regID})
	if err != nil || info.Status != gt.RegistryStatusAvailable || *info.RegistryName != "events" {
		t.Fatalf("registry: %+v, %v", info, err)
	}

	id := &gt.SchemaId{RegistryName: new("events"), SchemaName: new("message")}
	const original = `{"type":"record","name":"Message","fields":[{"name":"id","type":"int"}]}`
	const updated = `{"type":"record","name":"Message","fields":[
		{"name":"id","type":"long"},{"name":"note","type":["null","string"],"default":null}]}`
	lookup := &glue.GetSchemaByDefinitionInput{SchemaId: id, SchemaDefinition: new(original)}
	_, err = c.GetSchemaByDefinition(ctx, lookup)
	code(t, err, "EntityNotFoundException")

	create := &glue.CreateSchemaInput{
		RegistryId: regID, SchemaName: id.SchemaName, DataFormat: gt.DataFormatAvro,
		Compatibility: gt.CompatibilityBackwardAll, SchemaDefinition: new(original),
		Tags: map[string]string{"team": "events"},
	}
	created, err := c.CreateSchema(ctx, create)
	if err != nil {
		t.Fatal(err)
	}

	if _, parseErr := uuid.Parse(*created.SchemaVersionId); parseErr != nil || *created.SchemaCheckpoint != 1 ||
		created.SchemaVersionStatus != gt.SchemaVersionStatusAvailable {
		t.Fatalf("auto-registered schema: %+v, %v", created, parseErr)
	}

	_, err = c.CreateSchema(ctx, create)
	code(t, err, "AlreadyExistsException")
	lookup.SchemaDefinition = new(`{"name":"Message", "fields": [ {"type":"int", "name":"id"} ], "type":"record"}`)
	found, err := c.GetSchemaByDefinition(ctx, lookup)
	if err != nil || *found.SchemaVersionId != *created.SchemaVersionId || found.DataFormat != gt.DataFormatAvro {
		t.Fatalf("lookup by definition: %+v, %v", found, err)
	}

	first, err := c.GetSchemaVersion(ctx, &glue.GetSchemaVersionInput{SchemaVersionId: found.SchemaVersionId})
	if err != nil || *first.SchemaDefinition != original || *first.VersionNumber != 1 {
		t.Fatalf("lookup by UUID: %+v, %v", first, err)
	}

	registered, err := c.RegisterSchemaVersion(ctx, &glue.RegisterSchemaVersionInput{
		SchemaId: id, SchemaDefinition: new(updated),
	})
	if err != nil || *registered.VersionNumber != 2 || registered.Status != gt.SchemaVersionStatusAvailable {
		t.Fatalf("register compatible version: %+v, %v", registered, err)
	}

	again, err := c.RegisterSchemaVersion(ctx, &glue.RegisterSchemaVersionInput{
		SchemaId: id, SchemaDefinition: new(updated),
	})
	if err != nil || *again.SchemaVersionId != *registered.SchemaVersionId {
		t.Fatalf("idempotent registration: %+v, %v", again, err)
	}

	_, err = c.RegisterSchemaVersion(ctx, &glue.RegisterSchemaVersionInput{
		SchemaId:         id,
		SchemaDefinition: new(`{"type":"record","name":"Message","fields":[{"name":"id","type":"boolean"}]}`),
	})
	code(t, err, "InvalidInputException")
	byARN := &gt.SchemaId{SchemaArn: created.SchemaArn}
	current, err := c.GetSchemaVersion(ctx, &glue.GetSchemaVersionInput{
		SchemaId: byARN, SchemaVersionNumber: &gt.SchemaVersionNumber{LatestVersion: true},
	})
	if err != nil || *current.SchemaVersionId != *registered.SchemaVersionId {
		t.Fatalf("latest version: %+v, %v", current, err)
	}

	summary, err := c.GetSchema(ctx, &glue.GetSchemaInput{SchemaId: byARN})
	if err != nil || *summary.NextSchemaVersion != 3 || *summary.LatestSchemaVersion != 2 ||
		*summary.RegistryArn != *reg.RegistryArn {
		t.Fatalf("schema: %+v, %v", summary, err)
	}

	versions := glue.NewListSchemaVersionsPaginator(c, &glue.ListSchemaVersionsInput{
		SchemaId: byARN, MaxResults: new(int32(1)),
	})
	for _, number := range []int64{1, 2} {
		page, pageErr := versions.NextPage(ctx)
		if pageErr != nil || len(page.Schemas) != 1 || *page.Schemas[0].VersionNumber != number {
			t.Fatalf("list version %d: %+v, %v", number, page, pageErr)
		}
	}

	if versions.HasMorePages() {
		t.Fatal("incompatible registration changed the version history")
	}

	tags, err := c.GetTags(ctx, &glue.GetTagsInput{ResourceArn: created.SchemaArn})
	if err != nil || tags.Tags["team"] != "events" {
		t.Fatalf("schema tags: %+v, %v", tags, err)
	}

	for _, value := range []string{"first", "second", "second"} {
		_, putErr := c.PutSchemaVersionMetadata(ctx, &glue.PutSchemaVersionMetadataInput{
			SchemaVersionId: registered.SchemaVersionId,
			MetadataKeyValue: &gt.MetadataKeyValuePair{
				MetadataKey: new("producer"), MetadataValue: new(value),
			},
		})
		if putErr != nil {
			t.Fatal(putErr)
		}
	}

	meta, err := c.QuerySchemaVersionMetadata(ctx, &glue.QuerySchemaVersionMetadataInput{
		SchemaVersionId: registered.SchemaVersionId,
	})
	if err != nil {
		t.Fatal(err)
	}

	producer := meta.MetadataInfoMap["producer"]
	if producer.MetadataValue == nil || *producer.MetadataValue != "second" ||
		len(producer.OtherMetadataValueList) != 1 || *producer.OtherMetadataValueList[0].MetadataValue != "first" {
		t.Fatalf("schema metadata: %+v", producer)
	}

	list, err := c.ListSchemas(ctx, &glue.ListSchemasInput{RegistryId: regID})
	if err != nil || len(list.Schemas) != 1 || *list.Schemas[0].SchemaArn != *created.SchemaArn {
		t.Fatalf("schemas: %+v, %v", list, err)
	}

	_, err = c.DeleteRegistry(ctx, &glue.DeleteRegistryInput{RegistryId: regID})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.GetSchemaVersion(ctx, &glue.GetSchemaVersionInput{SchemaVersionId: registered.SchemaVersionId})
	code(t, err, "EntityNotFoundException")
	_, err = c.GetRegistry(ctx, &glue.GetRegistryInput{RegistryId: regID})
	code(t, err, "EntityNotFoundException")
}

func TestGlueSchemaRegistryJSONAndUnsupportedInputs(t *testing.T) {
	ctx := context.Background()
	c := glueClient(server(t))
	id := &gt.SchemaId{SchemaName: new("message")}
	const first = `{"type":"object","properties":{"id":{"type":"integer"}},"additionalProperties":false}`
	const second = `{"type":"object","properties":{"id":{"type":"number"},"label":{"type":"string"}},
		"additionalProperties":false}`
	const incompatible = `{"type":"object","properties":{"id":{"type":"number"},"label":{"type":"string"}},
		"required":["label"],"additionalProperties":false}`

	in := &glue.CreateSchemaInput{
		SchemaName: id.SchemaName, DataFormat: gt.DataFormatJson,
		Compatibility: gt.CompatibilityBackward, SchemaDefinition: new(first),
	}
	created, err := c.CreateSchema(ctx, in)
	if err != nil || *created.RegistryName != "default-registry" {
		t.Fatalf("default registry: %+v, %v", created, err)
	}

	r, err := c.ListRegistries(ctx, &glue.ListRegistriesInput{MaxResults: new(int32(1))})
	if err != nil || len(r.Registries) != 1 || *r.Registries[0].RegistryArn != *created.RegistryArn {
		t.Fatalf("default registry listing: %+v, %v", r, err)
	}

	registered, err := c.RegisterSchemaVersion(ctx, &glue.RegisterSchemaVersionInput{
		SchemaId: id, SchemaDefinition: new(second),
	})
	if err != nil || *registered.VersionNumber != 2 {
		t.Fatalf("compatible JSON change: %+v, %v", registered, err)
	}

	for _, definition := range []string{incompatible, `{"type":"object","patternProperties":{}}`, `{"type":3}`} {
		_, registrationErr := c.RegisterSchemaVersion(ctx, &glue.RegisterSchemaVersionInput{
			SchemaId: id, SchemaDefinition: new(definition),
		})
		code(t, registrationErr, "InvalidInputException")
	}

	_, err = c.DeleteSchema(ctx, &glue.DeleteSchemaInput{SchemaId: id})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.GetSchemaVersion(ctx, &glue.GetSchemaVersionInput{SchemaVersionId: created.SchemaVersionId})
	code(t, err, "EntityNotFoundException")
	_, err = c.GetSchemaByDefinition(ctx, &glue.GetSchemaByDefinitionInput{SchemaId: id, SchemaDefinition: new(first)})
	code(t, err, "EntityNotFoundException")

	in.DataFormat = gt.DataFormatProtobuf
	_, err = c.CreateSchema(ctx, in)
	code(t, err, "InvalidInputException")
	in.DataFormat, in.Compatibility = gt.DataFormatJson, gt.CompatibilityDisabled
	in.SchemaDefinition = nil
	_, err = c.CreateSchema(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.RegisterSchemaVersion(ctx, &glue.RegisterSchemaVersionInput{SchemaId: id, SchemaDefinition: new(first)})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.RegisterSchemaVersion(ctx, &glue.RegisterSchemaVersionInput{SchemaId: id, SchemaDefinition: new(second)})
	code(t, err, "InvalidInputException")
}

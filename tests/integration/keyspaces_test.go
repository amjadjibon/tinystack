package integration_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/keyspaces"
	"github.com/aws/aws-sdk-go-v2/service/keyspaces/types"
	tagging "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
)

func keyspacesClient(s *httptest.Server, account, region string) *sdk.Client {
	return sdk.New(sdk.Options{
		Region: region, BaseEndpoint: new(s.URL), Retryer: aws.NopRetryer{},
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: account, SecretAccessKey: "test"}, nil
		}),
	})
}

func keyspacesTable(keyspace, table string) *sdk.CreateTableInput {
	return &sdk.CreateTableInput{
		KeyspaceName: new(keyspace), TableName: new(table),
		SchemaDefinition: &types.SchemaDefinition{
			AllColumns: []types.ColumnDefinition{
				{Name: new("id"), Type: new("text")}, {Name: new("n"), Type: new("int")},
			},
			PartitionKeys: []types.PartitionKey{{Name: new("id")}},
		},
	}
}

func getKeyspacesTable(ctx context.Context, t *testing.T, c *sdk.Client, ks, table string) *sdk.GetTableOutput {
	t.Helper()
	value, err := c.GetTable(ctx, &sdk.GetTableInput{KeyspaceName: new(ks), TableName: new(table)})
	if err != nil {
		t.Fatal(err)
	}

	return value
}

func awaitKeyspacesTable(ctx context.Context, t *testing.T, c *sdk.Client, ks, table string) *sdk.GetTableOutput {
	t.Helper()
	ticker := time.NewTicker(40 * time.Millisecond)
	defer ticker.Stop()
	for {
		value := getKeyspacesTable(ctx, t, c, ks, table)
		if value.Status == types.TableStatusActive {
			return value
		}

		select {
		case <-ctx.Done():
			t.Fatalf("table %s.%s still %s: %v", ks, table, value.Status, ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestKeyspacesLifecycle(t *testing.T) {
	s := server(t)
	c := keyspacesClient(s, "test", "us-east-1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const keyspace, table = "test_keyspace", "events"
	created, err := c.CreateKeyspace(ctx, &sdk.CreateKeyspaceInput{KeyspaceName: new(keyspace)})
	if err != nil {
		t.Fatal(err)
	}

	arn := "arn:aws:cassandra:us-east-1:000000000000:/keyspace/test_keyspace/"
	if aws.ToString(created.ResourceArn) != arn {
		t.Fatal(created.ResourceArn)
	}

	_, err = c.UpdateKeyspace(ctx, &sdk.UpdateKeyspaceInput{
		KeyspaceName: new(keyspace),
		ReplicationSpecification: &types.ReplicationSpecification{
			ReplicationStrategy: types.RsMultiRegion, RegionList: []string{"us-east-1", "us-west-2"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateType(ctx, &sdk.CreateTypeInput{
		KeyspaceName: new(keyspace), TypeName: new("Address"),
		FieldDefinitions: []types.FieldDefinition{{Name: new("postal"), Type: new("text")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	in := keyspacesTable(keyspace, table)
	in.SchemaDefinition.AllColumns = append(in.SchemaDefinition.AllColumns,
		types.ColumnDefinition{Name: new("location"), Type: new("frozen<address>")})
	in.DefaultTimeToLive = new(int32(3600))
	in.Comment = &types.Comment{Message: new("sample")}
	in.PointInTimeRecovery = &types.PointInTimeRecovery{Status: types.PointInTimeRecoveryStatusEnabled}
	in.ClientSideTimestamps = &types.ClientSideTimestamps{Status: types.ClientSideTimestampsStatusEnabled}
	in.CapacitySpecification = &types.CapacitySpecification{
		ThroughputMode: types.ThroughputModeProvisioned, ReadCapacityUnits: new(int64(2)),
		WriteCapacityUnits: new(int64(3)),
	}
	in.EncryptionSpecification = &types.EncryptionSpecification{
		Type:             types.EncryptionTypeCustomerManagedKmsKey,
		KmsKeyIdentifier: new("arn:aws:kms:us-east-1:000000000000:key/example"),
	}
	createdTable, err := c.CreateTable(ctx, in)
	if err != nil || aws.ToString(createdTable.ResourceArn) != arn+"table/"+table {
		t.Fatalf("create table: %v %v", createdTable, err)
	}

	if v := getKeyspacesTable(ctx, t, c, keyspace, table); v.Status != types.TableStatusCreating {
		t.Fatalf("initial status: %s", v.Status)
	}

	v := awaitKeyspacesTable(ctx, t, c, keyspace, table)
	if v.CreationTimestamp == nil || v.PointInTimeRecovery.EarliestRestorableTimestamp == nil ||
		aws.ToInt32(v.DefaultTimeToLive) != 3600 || v.Ttl.Status != types.TimeToLiveStatusEnabled ||
		v.ClientSideTimestamps.Status != types.ClientSideTimestampsStatusEnabled ||
		aws.ToString(v.Comment.Message) != "sample" || aws.ToInt64(v.CapacitySpecification.WriteCapacityUnits) != 3 ||
		v.EncryptionSpecification.Type != types.EncryptionTypeCustomerManagedKmsKey {
		t.Fatalf("table configuration: %+v", v)
	}

	ks, err := c.GetKeyspace(ctx, &sdk.GetKeyspaceInput{KeyspaceName: new(keyspace)})
	if err != nil || ks.ReplicationStrategy != types.RsMultiRegion || len(ks.ReplicationGroupStatuses) != 2 ||
		ks.ReplicationGroupStatuses[0].KeyspaceStatus != types.KeyspaceStatusActive {
		t.Fatalf("replication: %+v %v", ks, err)
	}

	typ, err := c.GetType(ctx, &sdk.GetTypeInput{KeyspaceName: new(keyspace), TypeName: new("address")})
	if err != nil || typ.Status != types.TypeStatusActive || !slices.Equal(typ.DirectReferringTables, []string{table}) {
		t.Fatalf("type: %+v %v", typ, err)
	}

	typesPage, err := c.ListTypes(ctx, &sdk.ListTypesInput{KeyspaceName: new(keyspace)})
	if err != nil || !slices.Equal(typesPage.Types, []string{"address"}) {
		t.Fatalf("type list: %+v %v", typesPage, err)
	}

	_, err = c.DeleteType(ctx, &sdk.DeleteTypeInput{KeyspaceName: new(keyspace), TypeName: new("address")})
	wantCode(t, err, "ConflictException")

	_, err = c.UpdateTable(ctx, &sdk.UpdateTableInput{
		KeyspaceName: new(keyspace), TableName: new(table), DefaultTimeToLive: new(int32(0)),
		AddColumns:            []types.ColumnDefinition{{Name: new("extra"), Type: new("list<text>")}},
		CapacitySpecification: &types.CapacitySpecification{ThroughputMode: types.ThroughputModePayPerRequest},
	})
	if err != nil {
		t.Fatal(err)
	}

	if status := getKeyspacesTable(ctx, t, c, keyspace, table).Status; status != types.TableStatusUpdating {
		t.Fatalf("update status: %s", status)
	}

	v = awaitKeyspacesTable(ctx, t, c, keyspace, table)
	if len(v.SchemaDefinition.AllColumns) != 4 || aws.ToInt32(v.DefaultTimeToLive) != 0 ||
		v.CapacitySpecification.LastUpdateToPayPerRequestTimestamp == nil {
		t.Fatalf("updated table: %+v", v)
	}

	other := keyspacesClient(s, "team-b", "us-east-1")
	_, err = other.GetTable(ctx, &sdk.GetTableInput{KeyspaceName: new(keyspace), TableName: new(table)})
	wantCode(t, err, "ResourceNotFoundException")
	other = keyspacesClient(s, "test", "us-west-2")
	_, err = other.GetKeyspace(ctx, &sdk.GetKeyspaceInput{KeyspaceName: new(keyspace)})
	wantCode(t, err, "ResourceNotFoundException")

	_, err = c.DeleteTable(ctx, &sdk.DeleteTableInput{KeyspaceName: new(keyspace), TableName: new(table)})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteType(ctx, &sdk.DeleteTypeInput{KeyspaceName: new(keyspace), TypeName: new("address")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteKeyspace(ctx, &sdk.DeleteKeyspaceInput{KeyspaceName: new(keyspace)})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.GetKeyspace(ctx, &sdk.GetKeyspaceInput{KeyspaceName: new(keyspace)})
	wantCode(t, err, "ResourceNotFoundException")
}

func TestKeyspacesValidationAndPagination(t *testing.T) {
	c := keyspacesClient(server(t), "test", "us-east-1")
	ctx := context.Background()
	const ks = "app"
	keyspace := &sdk.CreateKeyspaceInput{KeyspaceName: new(ks)}
	if _, err := c.CreateKeyspace(ctx, keyspace); err != nil {
		t.Fatal(err)
	}

	_, err := c.CreateKeyspace(ctx, keyspace)
	wantCode(t, err, "ConflictException")
	_, err = c.UpdateKeyspace(ctx, &sdk.UpdateKeyspaceInput{
		KeyspaceName: new(ks), ReplicationSpecification: &types.ReplicationSpecification{
			ReplicationStrategy: types.RsMultiRegion, RegionList: []string{"us-east-1", "us-east-1"},
		},
	})
	wantCode(t, err, "ValidationException")
	_, err = c.DeleteKeyspace(ctx, &sdk.DeleteKeyspaceInput{KeyspaceName: new("system_schema_mcs")})
	wantCode(t, err, "AccessDeniedException")
	_, err = c.CreateTable(ctx, keyspacesTable("system", "custom"))
	wantCode(t, err, "AccessDeniedException")

	for name, modify := range map[string]func(*sdk.CreateTableInput){
		"bad type":         func(in *sdk.CreateTableInput) { in.SchemaDefinition.AllColumns[0].Type = new("garbage") },
		"no partition key": func(in *sdk.CreateTableInput) { in.SchemaDefinition.PartitionKeys = []types.PartitionKey{} },
		"duplicate column": func(in *sdk.CreateTableInput) { in.SchemaDefinition.AllColumns[1].Name = new("id") },
		"invalid capacity": func(in *sdk.CreateTableInput) {
			in.CapacitySpecification = &types.CapacitySpecification{ThroughputMode: types.ThroughputModeProvisioned}
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := keyspacesTable(ks, "events")
			modify(in)
			_, createErr := c.CreateTable(ctx, in)
			wantCode(t, createErr, "ValidationException")
		})
	}

	unsupported := keyspacesTable(ks, "unsupported")
	unsupported.CdcSpecification = &types.CdcSpecification{Status: types.CdcStatusEnabled}
	_, err = c.CreateTable(ctx, unsupported)
	wantCode(t, err, "ValidationException")

	for _, table := range []string{"first", "second"} {
		if _, err = c.CreateTable(ctx, keyspacesTable(ks, table)); err != nil {
			t.Fatal(err)
		}
	}

	update := &sdk.UpdateKeyspaceInput{
		KeyspaceName: new(ks), ReplicationSpecification: &types.ReplicationSpecification{
			ReplicationStrategy: types.RsMultiRegion, RegionList: []string{"us-east-1", "us-west-2"},
		},
	}
	_, err = c.UpdateKeyspace(ctx, update)
	wantCode(t, err, "ValidationException")
	update.ClientSideTimestamps = &types.ClientSideTimestamps{Status: types.ClientSideTimestampsStatusEnabled}
	if _, err = c.UpdateKeyspace(ctx, update); err != nil {
		t.Fatal(err)
	}

	if timestamps := getKeyspacesTable(ctx, t, c, ks, "first").ClientSideTimestamps; timestamps == nil ||
		timestamps.Status != types.ClientSideTimestampsStatusEnabled {
		t.Fatalf("timestamps after replication update: %+v", timestamps)
	}

	update.ClientSideTimestamps = nil
	update.ReplicationSpecification = &types.ReplicationSpecification{ReplicationStrategy: types.RsSingleRegion}
	_, err = c.UpdateKeyspace(ctx, update)
	wantCode(t, err, "ValidationException")

	_, err = c.CreateTable(ctx, keyspacesTable(ks, "first"))
	wantCode(t, err, "ConflictException")
	_, err = c.DeleteKeyspace(ctx, &sdk.DeleteKeyspaceInput{KeyspaceName: new(ks)})
	wantCode(t, err, "ConflictException")

	keyspaces := sdk.NewListKeyspacesPaginator(c, &sdk.ListKeyspacesInput{MaxResults: new(int32(2))})
	var names []string
	for keyspaces.HasMorePages() {
		page, e := keyspaces.NextPage(ctx)
		if e != nil {
			t.Fatal(e)
		}

		for _, item := range page.Keyspaces {
			names = append(names, aws.ToString(item.KeyspaceName))
		}
	}

	if !slices.Equal(names, []string{ks, "system", "system_multiregion_info", "system_schema", "system_schema_mcs"}) {
		t.Fatal(names)
	}

	page, err := c.ListTables(ctx, &sdk.ListTablesInput{KeyspaceName: new(ks), MaxResults: new(int32(1))})
	if err != nil || page.NextToken == nil || len(page.Tables) != 1 ||
		aws.ToString(page.Tables[0].TableName) != "first" {
		t.Fatalf("first table page: %+v %v", page, err)
	}

	page, err = c.ListTables(ctx, &sdk.ListTablesInput{KeyspaceName: new(ks), NextToken: page.NextToken})
	if err != nil || page.NextToken != nil || len(page.Tables) != 1 ||
		aws.ToString(page.Tables[0].TableName) != "second" {
		t.Fatalf("last table page: %+v %v", page, err)
	}
}

func TestKeyspacesTagsAndResourceGroups(t *testing.T) {
	s := server(t)
	c := keyspacesClient(s, "test", "us-east-1")
	ctx := context.Background()
	ks, err := c.CreateKeyspace(ctx, &sdk.CreateKeyspaceInput{
		KeyspaceName: new("app"), Tags: []types.Tag{{Key: new("stack"), Value: new("test")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	table, err := c.CreateTable(ctx, keyspacesTable("app", "events"))
	if err != nil {
		t.Fatal(err)
	}

	arn := aws.ToString(table.ResourceArn)
	_, err = c.TagResource(ctx, &sdk.TagResourceInput{
		ResourceArn: table.ResourceArn, Tags: []types.Tag{{Key: new("owner"), Value: new("local")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	catalog := taggingClient(s, "us-east-1")
	add, err := catalog.TagResources(ctx, &tagging.TagResourcesInput{
		ResourceARNList: []string{arn, aws.ToString(ks.ResourceArn)}, Tags: map[string]string{"team": "dev"},
	})
	if err != nil || len(add.FailedResourcesMap) != 0 {
		t.Fatalf("tag resources: %+v %v", add, err)
	}

	page, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{
		ResourceArn: table.ResourceArn, MaxResults: new(int32(1)),
	})
	if err != nil || len(page.Tags) != 1 || page.NextToken == nil || aws.ToString(page.Tags[0].Key) != "owner" {
		t.Fatalf("tag page: %+v %v", page, err)
	}

	groups, err := catalog.GetResources(
		ctx,
		&tagging.GetResourcesInput{ResourceTypeFilters: []string{"cassandra:table"}},
	)
	if err != nil || len(groups.ResourceTagMappingList) != 1 ||
		aws.ToString(groups.ResourceTagMappingList[0].ResourceARN) != arn {
		t.Fatalf("tagged tables: %+v %v", groups, err)
	}

	remove, err := catalog.UntagResources(ctx, &tagging.UntagResourcesInput{
		ResourceARNList: []string{arn}, TagKeys: []string{"team"},
	})
	if err != nil || len(remove.FailedResourcesMap) != 0 {
		t.Fatalf("untag table: %+v %v", remove, err)
	}

	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{
		ResourceArn: table.ResourceArn, Tags: []types.Tag{{Key: new("owner"), Value: new("local")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	page, err = c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: table.ResourceArn})
	if err != nil || len(page.Tags) != 0 {
		t.Fatalf("remaining tags: %+v %v", page, err)
	}

	tags := make([]types.Tag, 51)
	for i := range tags {
		tags[i] = types.Tag{Key: new(fmt.Sprintf("tag-%d", i)), Value: new("test")}
	}

	_, err = c.TagResource(ctx, &sdk.TagResourceInput{ResourceArn: table.ResourceArn, Tags: tags})
	wantCode(t, err, "ValidationException")
}

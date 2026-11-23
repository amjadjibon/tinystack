package integration_test

import (
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/rds/types"
)

func TestRDSDiscovery(t *testing.T) {
	c := rdsClient(server(t), "test", "us-east-1")
	ctx := t.Context()
	versions, err := c.DescribeDBEngineVersions(ctx, &rds.DescribeDBEngineVersionsInput{
		Engine: new("postgres"), DefaultOnly: new(true),
	})
	if err != nil || len(versions.DBEngineVersions) != 1 ||
		aws.ToString(versions.DBEngineVersions[0].DBParameterGroupFamily) != "postgres16" ||
		aws.ToString(versions.DBEngineVersions[0].EngineVersion) != "16" {
		t.Fatal(versions, err)
	}

	versions, err = c.DescribeDBEngineVersions(ctx, &rds.DescribeDBEngineVersionsInput{
		Engine: new("aurora-postgresql"), DBParameterGroupFamily: new("aurora-postgresql16"),
		Filters: []types.Filter{{Name: new("engine-version"), Values: []string{"16.6"}}},
	})
	if err != nil || len(versions.DBEngineVersions) != 1 ||
		aws.ToString(versions.DBEngineVersions[0].EngineVersion) != "16.6" {
		t.Fatal(versions, err)
	}

	versions, err = c.DescribeDBEngineVersions(ctx, &rds.DescribeDBEngineVersionsInput{
		Engine: new("aurora-postgresql"), EngineVersion: new("16"), DefaultOnly: new(true),
	})
	if err != nil || len(versions.DBEngineVersions) != 1 ||
		aws.ToString(versions.DBEngineVersions[0].EngineVersion) != "16.11" {
		t.Fatal(versions, err)
	}

	_, err = c.DescribeDBEngineVersions(ctx, &rds.DescribeDBEngineVersionsInput{
		Filters: []types.Filter{{Name: new("engine"), Values: []string{"post*"}}},
	})
	code(t, err, "InvalidParameterValue")

	orderable, err := c.DescribeOrderableDBInstanceOptions(ctx, &rds.DescribeOrderableDBInstanceOptionsInput{
		Engine: new("postgres"), MaxRecords: new(int32(20)),
	})
	if err != nil || len(orderable.OrderableDBInstanceOptions) != 20 || orderable.Marker != nil {
		t.Fatal(orderable, err)
	}

	orderable, err = c.DescribeOrderableDBInstanceOptions(ctx, &rds.DescribeOrderableDBInstanceOptionsInput{
		Engine: new("postgres"), EngineVersion: new("16"), DBInstanceClass: new("db.t4g.micro"), Vpc: new(true),
	})
	if err != nil || len(orderable.OrderableDBInstanceOptions) != 1 ||
		!aws.ToBool(orderable.OrderableDBInstanceOptions[0].ReadReplicaCapable) {
		t.Fatal(orderable, err)
	}

	in := rdsInput("discovery")
	if _, err = c.CreateDBInstance(ctx, in); err != nil {
		t.Fatal(err)
	}

	modifications, err := c.DescribeValidDBInstanceModifications(ctx, &rds.DescribeValidDBInstanceModificationsInput{
		DBInstanceIdentifier: in.DBInstanceIdentifier,
	})
	if err != nil || modifications.ValidDBInstanceModificationsMessage == nil ||
		len(modifications.ValidDBInstanceModificationsMessage.Storage) != 1 ||
		aws.ToInt32(modifications.ValidDBInstanceModificationsMessage.Storage[0].StorageSize[0].From) != 20 {
		t.Fatal(modifications, err)
	}

	account, err := c.DescribeAccountAttributes(ctx, &rds.DescribeAccountAttributesInput{})
	if err != nil || !slices.ContainsFunc(account.AccountQuotas, func(q types.AccountQuota) bool {
		return aws.ToString(q.AccountQuotaName) == "DBInstances" && aws.ToInt64(q.Used) == 1
	}) {
		t.Fatal(account, err)
	}

	options, err := c.DescribeOptionGroupOptions(ctx, &rds.DescribeOptionGroupOptionsInput{
		EngineName: new("mysql"), MajorEngineVersion: new("8.0"),
	})
	if err != nil || len(options.OptionGroupOptions) != 1 ||
		aws.ToString(options.OptionGroupOptions[0].Name) != "MARIADB_AUDIT_PLUGIN" ||
		len(options.OptionGroupOptions[0].OptionGroupOptionSettings) != 3 {
		t.Fatal(options, err)
	}
}

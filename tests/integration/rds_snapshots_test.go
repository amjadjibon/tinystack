package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	tagtypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"
)

func waitRDSSnapshot(t *testing.T, c *rds.Client, id string) types.DBSnapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		out, err := c.DescribeDBSnapshots(ctx, &rds.DescribeDBSnapshotsInput{DBSnapshotIdentifier: new(id)})
		if err != nil {
			t.Fatal(err)
		}

		if len(out.DBSnapshots) == 1 && aws.ToString(out.DBSnapshots[0].Status) == "available" {
			if aws.ToInt32(out.DBSnapshots[0].PercentProgress) != 100 {
				t.Fatal("available snapshot must report 100 percent progress")
			}

			return out.DBSnapshots[0]
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal(ctx.Err())
	return types.DBSnapshot{}
}

func TestRDSSnapshots(t *testing.T) {
	s := server(t)
	c := rdsClient(s, "test", "us-east-1")
	ctx := context.Background()
	in := rdsInput("source")
	in.CopyTagsToSnapshot = new(true)
	in.Tags = []types.Tag{{Key: new("original"), Value: new("yes")}}
	original, err := c.CreateDBInstance(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	created, err := c.CreateDBSnapshot(ctx, &rds.CreateDBSnapshotInput{
		DBInstanceIdentifier: new("SOURCE"), DBSnapshotIdentifier: new("Backup"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToString(created.DBSnapshot.Status) != "creating" || aws.ToInt32(created.DBSnapshot.PercentProgress) != 0 {
		t.Fatalf("creating snapshot: %+v", created.DBSnapshot)
	}

	// State errors must be visible before the short metadata lifecycle completes.
	_, err = c.CopyDBSnapshot(ctx, &rds.CopyDBSnapshotInput{
		SourceDBSnapshotIdentifier: new("backup"), TargetDBSnapshotIdentifier: new("premature"),
	})
	code(t, err, "InvalidDBSnapshotState")
	_, err = c.DeleteDBSnapshot(ctx, &rds.DeleteDBSnapshotInput{DBSnapshotIdentifier: new("backup")})
	code(t, err, "InvalidDBSnapshotState")
	snap := waitRDSSnapshot(t, c, "backup")
	if aws.ToString(snap.DBSnapshotIdentifier) != "backup" || aws.ToString(snap.SnapshotType) != "manual" ||
		aws.ToString(snap.DbiResourceId) != aws.ToString(original.DBInstance.DbiResourceId) ||
		len(snap.TagList) != 1 || snap.SnapshotCreateTime == nil || snap.OriginalSnapshotCreateTime == nil {
		t.Fatalf("snapshot metadata: %+v", snap)
	}

	_, err = c.CreateDBSnapshot(ctx, &rds.CreateDBSnapshotInput{
		DBInstanceIdentifier: new("source"), DBSnapshotIdentifier: new("backup"),
	})
	code(t, err, "DBSnapshotAlreadyExists")
	_, err = c.AddTagsToResource(ctx, &rds.AddTagsToResourceInput{
		ResourceName: snap.DBSnapshotArn, Tags: []types.Tag{{Key: new("extra"), Value: new("value")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	listed, err := c.ListTagsForResource(ctx, &rds.ListTagsForResourceInput{ResourceName: snap.DBSnapshotArn})
	if err != nil || len(listed.TagList) != 2 {
		t.Fatalf("snapshot tags: %+v %v", listed, err)
	}

	tagger := resourcegroupstaggingapi.New(resourcegroupstaggingapi.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: c.Options().Credentials,
	})
	tagged, err := tagger.TagResources(ctx, &resourcegroupstaggingapi.TagResourcesInput{
		ResourceARNList: []string{aws.ToString(snap.DBSnapshotArn)}, Tags: map[string]string{"rgta": "yes"},
	})
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("snapshot tagger: %+v %v", tagged, err)
	}

	resources, err := tagger.GetResources(ctx, &resourcegroupstaggingapi.GetResourcesInput{
		ResourceTypeFilters: []string{"rds:snapshot"}, TagFilters: []tagtypes.TagFilter{{Key: new("rgta")}},
	})
	if err != nil || len(resources.ResourceTagMappingList) != 1 {
		t.Fatalf("snapshot inventory: %+v %v", resources, err)
	}

	copied, err := c.CopyDBSnapshot(ctx, &rds.CopyDBSnapshotInput{
		SourceDBSnapshotIdentifier: snap.DBSnapshotArn, TargetDBSnapshotIdentifier: new("copy"), CopyTags: new(true),
		Tags: []types.Tag{{Key: new("extra"), Value: new("override")}},
	})
	if err != nil || aws.ToString(copied.DBSnapshot.Status) != "creating" {
		t.Fatalf("copy: %+v %v", copied, err)
	}

	copySnap := waitRDSSnapshot(t, c, "copy")
	if len(copySnap.TagList) != 3 ||
		aws.ToString(copySnap.SourceDBSnapshotIdentifier) != aws.ToString(snap.DBSnapshotArn) {
		t.Fatalf("copy metadata: %+v", copySnap)
	}

	_, err = c.ModifyDBInstance(ctx, &rds.ModifyDBInstanceInput{
		DBInstanceIdentifier: new("source"), DBInstanceClass: new("db.t3.large"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteDBInstance(ctx, &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier: new("source"), FinalDBSnapshotIdentifier: new("backup"),
	})
	code(t, err, "DBSnapshotAlreadyExists")
	_, err = c.DeleteDBInstance(ctx, &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier: new("source"), FinalDBSnapshotIdentifier: new("final"),
	})
	if err != nil {
		t.Fatal(err)
	}

	final := waitRDSSnapshot(t, c, "final")
	if aws.ToString(final.DBInstanceIdentifier) != "source" {
		t.Fatal("final snapshot lost source identity")
	}

	restored, err := c.RestoreDBInstanceFromDBSnapshot(ctx, &rds.RestoreDBInstanceFromDBSnapshotInput{
		DBInstanceIdentifier: new("restored"), DBSnapshotIdentifier: snap.DBSnapshotArn,
	})
	if err != nil {
		t.Fatal(err)
	}

	db := restored.DBInstance
	if aws.ToString(db.DBInstanceStatus) != "creating" || aws.ToString(db.DBInstanceClass) != "db.t3.micro" ||
		len(db.TagList) != 1 || aws.ToString(db.TagList[0].Key) != "original" ||
		aws.ToString(db.MasterUsername) != "testuser" || aws.ToString(db.DBName) != "testdb" ||
		aws.ToString(db.DBInstanceArn) == aws.ToString(original.DBInstance.DBInstanceArn) ||
		aws.ToString(db.DbiResourceId) == aws.ToString(original.DBInstance.DbiResourceId) {
		t.Fatalf("restored settings: %+v", db)
	}

	if err = rds.NewDBInstanceAvailableWaiter(c).Wait(ctx,
		&rds.DescribeDBInstancesInput{DBInstanceIdentifier: new("restored")}, 5*time.Second,
		func(options *rds.DBInstanceAvailableWaiterOptions) {
			options.MinDelay, options.MaxDelay = 10*time.Millisecond, 20*time.Millisecond
		}); err != nil {
		t.Fatal(err)
	}

	ready, err := c.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{DBInstanceIdentifier: new("restored")})
	if err != nil || aws.ToString(ready.DBInstances[0].DBInstanceStatus) != "available" {
		t.Fatalf("restored availability: %+v %v", ready, err)
	}

	_, err = c.RestoreDBInstanceFromDBSnapshot(ctx, &rds.RestoreDBInstanceFromDBSnapshotInput{
		DBInstanceIdentifier: new("restored"), DBSnapshotIdentifier: new("copy"),
	})
	code(t, err, "DBInstanceAlreadyExists")
	for _, id := range []string{"backup", "copy", "final"} {
		deleted, e := c.DeleteDBSnapshot(ctx, &rds.DeleteDBSnapshotInput{DBSnapshotIdentifier: new(id)})
		if e != nil || aws.ToString(deleted.DBSnapshot.Status) != "deleted" {
			t.Fatalf("delete %s: %+v %v", id, deleted, e)
		}
	}

	_, err = c.DescribeDBSnapshots(ctx, &rds.DescribeDBSnapshotsInput{DBSnapshotIdentifier: new("backup")})
	code(t, err, "DBSnapshotNotFound")
}

func TestRDSSnapshotFiltersAndValidation(t *testing.T) {
	s := server(t)
	c := rdsClient(s, "test", "us-east-1")
	ctx := context.Background()
	instance, err := c.CreateDBInstance(ctx, rdsInput("source"))
	if err != nil {
		t.Fatal(err)
	}

	for i := range 23 {
		_, err = c.CreateDBSnapshot(ctx, &rds.CreateDBSnapshotInput{
			DBInstanceIdentifier: new("source"), DBSnapshotIdentifier: new(fmt.Sprintf("snap-%02d", i)),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	first, err := c.DescribeDBSnapshots(ctx, &rds.DescribeDBSnapshotsInput{MaxRecords: new(int32(20))})
	if err != nil || len(first.DBSnapshots) != 20 || first.Marker == nil {
		t.Fatalf("first page: %+v %v", first, err)
	}

	next, err := c.DescribeDBSnapshots(ctx, &rds.DescribeDBSnapshotsInput{
		MaxRecords: new(int32(20)), Marker: first.Marker,
	})
	if err != nil || len(next.DBSnapshots) != 3 || next.Marker != nil {
		t.Fatalf("second page: %+v %v", next, err)
	}

	for _, input := range []*rds.DescribeDBSnapshotsInput{
		{SnapshotType: new("automated")},
		{Filters: []types.Filter{{Name: new("db-snapshot-id"), Values: []string{"missing"}}}},
		{Filters: []types.Filter{{Name: new("engine"), Values: []string{"mysql"}}}},
	} {
		out, e := c.DescribeDBSnapshots(ctx, input)
		if e != nil || len(out.DBSnapshots) != 0 {
			t.Fatalf("empty selection: %+v %v", out, e)
		}
	}

	selected, err := c.DescribeDBSnapshots(ctx, &rds.DescribeDBSnapshotsInput{
		DBInstanceIdentifier: new(
			"SOURCE",
		),
		SnapshotType:  new("manual"),
		DbiResourceId: instance.DBInstance.DbiResourceId,
		Filters: []types.Filter{
			{Name: new("db-instance-id"), Values: []string{aws.ToString(instance.DBInstance.DBInstanceArn)}},
			{Name: new("db-snapshot-id"), Values: []string{"snap-01", "snap-02"}},
			{Name: new("snapshot-type"), Values: []string{"manual"}},
			{Name: new("dbi-resource-id"), Values: []string{aws.ToString(instance.DBInstance.DbiResourceId)}},
		},
	})
	if err != nil || len(selected.DBSnapshots) != 2 {
		t.Fatalf("filters: %+v %v", selected, err)
	}

	for _, input := range []*rds.DescribeDBSnapshotsInput{
		{SnapshotType: new("invalid")},
		{MaxRecords: new(int32(19))},
		{Marker: new("invalid!")},
		{Filters: []types.Filter{{Name: new("engine"), Values: []string{"post*"}}}},
		{Filters: []types.Filter{{Name: new("invalid"), Values: []string{"x"}}}},
	} {
		_, err = c.DescribeDBSnapshots(ctx, input)
		code(t, err, "InvalidParameterValue")
	}

	_, err = c.CreateDBSnapshot(ctx, &rds.CreateDBSnapshotInput{
		DBInstanceIdentifier: new("source"), DBSnapshotIdentifier: new("invalid--name"),
	})
	code(t, err, "InvalidParameterValue")
	_, err = c.CreateDBSnapshot(ctx, &rds.CreateDBSnapshotInput{
		DBInstanceIdentifier: new("missing"), DBSnapshotIdentifier: new("valid"),
	})
	code(t, err, "DBInstanceNotFound")
	_, err = c.DeleteDBInstance(ctx, &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier: new("source"), SkipFinalSnapshot: new(true), FinalDBSnapshotIdentifier: new("conflict"),
	})
	code(t, err, "InvalidParameterValue")
	_, err = c.StopDBInstance(ctx, &rds.StopDBInstanceInput{DBInstanceIdentifier: new("source")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateDBSnapshot(ctx, &rds.CreateDBSnapshotInput{
		DBInstanceIdentifier: new("source"), DBSnapshotIdentifier: new("stopped"),
	})
	code(t, err, "InvalidDBInstanceState")
	if _, err = c.DeleteDBInstance(ctx, &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier: new("source"), FinalDBSnapshotIdentifier: new("stopped-final"),
	}); err != nil {
		t.Fatal(err)
	}

	waitRDSSnapshot(t, c, "stopped-final")

	for _, client := range []*rds.Client{rdsClient(s, "test", "eu-west-1"), rdsClient(s, "111111111111", "us-east-1")} {
		out, e := client.DescribeDBSnapshots(ctx, &rds.DescribeDBSnapshotsInput{})
		if e != nil || len(out.DBSnapshots) != 0 {
			t.Fatalf("scope isolation: %+v %v", out, e)
		}

		_, err = client.DeleteDBSnapshot(ctx, &rds.DeleteDBSnapshotInput{DBSnapshotIdentifier: new("snap-00")})
		code(t, err, "DBSnapshotNotFound")
	}

	waitRDSSnapshot(t, c, "snap-00")
	_, err = c.CopyDBSnapshot(ctx, &rds.CopyDBSnapshotInput{
		SourceDBSnapshotIdentifier: new("arn:aws:rds:eu-west-1:000000000000:snapshot:snap-00"),
		TargetDBSnapshotIdentifier: new("cross-region"),
	})
	code(t, err, "InvalidParameterValue")
	_, err = c.RestoreDBInstanceFromDBSnapshot(ctx, &rds.RestoreDBInstanceFromDBSnapshotInput{
		DBInstanceIdentifier: new("smaller"), DBSnapshotIdentifier: new("snap-00"), AllocatedStorage: new(int32(19)),
	})
	code(t, err, "InvalidParameterValue")
	_, err = c.RestoreDBInstanceFromDBSnapshot(ctx, &rds.RestoreDBInstanceFromDBSnapshotInput{
		DBInstanceIdentifier: new("missing"), DBSnapshotIdentifier: new("missing"),
	})
	code(t, err, "DBSnapshotNotFound")
}

func TestRDSCloudFormationSnapshotRestore(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := rdsClient(s, "test", "us-east-1")
	if _, err := c.CreateDBInstance(ctx, rdsInput("source")); err != nil {
		t.Fatal(err)
	}

	if _, err := c.CreateDBSnapshot(ctx, &rds.CreateDBSnapshotInput{
		DBInstanceIdentifier: new("source"), DBSnapshotIdentifier: new("restore-point"),
	}); err != nil {
		t.Fatal(err)
	}

	waitRDSSnapshot(t, c, "restore-point")
	cf := cfnClient(s)
	template := `{"Resources":{"Database":{"Type":"AWS::RDS::DBInstance","Properties":{
 "DBInstanceIdentifier":"restored-stack","DBSnapshotIdentifier":"restore-point","DBInstanceClass":"db.t3.small"}}}}`
	if _, err := cf.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: new("restore-stack"), TemplateBody: new(template),
	}); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, cf, "restore-stack", cfntypes.StackStatusCreateComplete)
	restored, err := c.DescribeDBInstances(
		ctx,
		&rds.DescribeDBInstancesInput{DBInstanceIdentifier: new("restored-stack")},
	)
	if err != nil || aws.ToString(restored.DBInstances[0].DBInstanceClass) != "db.t3.small" {
		t.Fatalf("stack restore: %+v %v", restored, err)
	}

	if _, err := cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("restore-stack")}); err != nil {
		t.Fatal(err)
	}
}

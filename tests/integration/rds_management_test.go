package integration_test

import (
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/rds/types"
)

func TestRDSDeferredInstanceModifications(t *testing.T) {
	c := rdsClient(server(t), "test", "us-east-1")
	ctx := t.Context()
	in := rdsInput("deferred-db")
	start := time.Now().UTC().Add(24 * time.Hour)
	in.PreferredMaintenanceWindow = new(start.Format("Mon:15:04") + "-" + start.Add(time.Hour).Format("Mon:15:04"))
	if _, err := c.CreateDBInstance(ctx, in); err != nil {
		t.Fatal(err)
	}

	id := in.DBInstanceIdentifier
	_, err := c.ModifyDBInstance(ctx, &rds.ModifyDBInstanceInput{
		DBInstanceIdentifier: id, DBInstanceClass: new("db.t3.small"), ApplyImmediately: new(false),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ModifyDBInstance(ctx, &rds.ModifyDBInstanceInput{
		DBInstanceIdentifier: id, AllocatedStorage: new(int32(40)), CopyTagsToSnapshot: new(true),
	})
	if err != nil {
		t.Fatal(err)
	}

	described, err := c.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{DBInstanceIdentifier: id})
	if err != nil {
		t.Fatal(err)
	}

	db := described.DBInstances[0]
	p := db.PendingModifiedValues
	if aws.ToString(db.DBInstanceClass) != "db.t3.micro" || aws.ToInt32(db.AllocatedStorage) != 20 ||
		!aws.ToBool(db.CopyTagsToSnapshot) || p == nil || aws.ToString(p.DBInstanceClass) != "db.t3.small" ||
		aws.ToInt32(p.AllocatedStorage) != 40 {
		t.Fatalf("deferred settings: %+v, pending: %+v", db, p)
	}

	_, err = c.ModifyDBInstance(ctx, &rds.ModifyDBInstanceInput{
		DBInstanceIdentifier: id, PreferredMaintenanceWindow: new("tue:12:00-tue:12:10"), ApplyImmediately: new(true),
	})
	code(t, err, "InvalidParameterValue")
	rebooted, err := c.RebootDBInstance(ctx, &rds.RebootDBInstanceInput{DBInstanceIdentifier: id})
	if err != nil || aws.ToString(rebooted.DBInstance.DBInstanceClass) != "db.t3.small" ||
		aws.ToInt32(rebooted.DBInstance.AllocatedStorage) != 40 || rebooted.DBInstance.PendingModifiedValues != nil {
		t.Fatal(rebooted, err)
	}

	_, err = c.ModifyDBInstance(ctx, &rds.ModifyDBInstanceInput{
		DBInstanceIdentifier: id, BackupRetentionPeriod: new(int32(5)), ApplyImmediately: new(false),
	})
	if err != nil {
		t.Fatal(err)
	}

	immediate, err := c.ModifyDBInstance(ctx, &rds.ModifyDBInstanceInput{
		DBInstanceIdentifier: id, ApplyImmediately: new(true),
	})
	if err != nil || aws.ToInt32(immediate.DBInstance.BackupRetentionPeriod) != 5 ||
		immediate.DBInstance.PendingModifiedValues != nil {
		t.Fatal(immediate, err)
	}
}

func TestRDSDeferredMaintenanceWindow(t *testing.T) {
	c := rdsClient(server(t), "test", "us-east-1")
	ctx := t.Context()
	now := time.Now().UTC()
	future := now.Add(24 * time.Hour)
	window := future.Format("Mon:15:04") + "-" + future.Add(time.Hour).Format("Mon:15:04")
	in := rdsInput("maintenance-db")
	in.PreferredMaintenanceWindow = new(window)
	if _, err := c.CreateDBInstance(ctx, in); err != nil {
		t.Fatal(err)
	}

	cluster := auroraInput("maintenance-cluster", "aurora-postgresql")
	cluster.PreferredMaintenanceWindow = new(window)
	if _, err := c.CreateDBCluster(ctx, cluster); err != nil {
		t.Fatal(err)
	}

	_, err := c.ModifyDBInstance(ctx, &rds.ModifyDBInstanceInput{
		DBInstanceIdentifier: in.DBInstanceIdentifier,
		DBInstanceClass:      new("db.t3.small"),
		ApplyImmediately:     new(false),
	})
	if err != nil {
		t.Fatal(err)
	}

	queued, err := c.ModifyDBCluster(ctx, &rds.ModifyDBClusterInput{
		DBClusterIdentifier: cluster.DBClusterIdentifier, BackupRetentionPeriod: new(int32(7)),
		DeletionProtection: new(true), ApplyImmediately: new(false),
	})
	if err != nil || aws.ToInt32(queued.DBCluster.BackupRetentionPeriod) != 1 ||
		!aws.ToBool(queued.DBCluster.DeletionProtection) || queued.DBCluster.PendingModifiedValues == nil ||
		aws.ToInt32(queued.DBCluster.PendingModifiedValues.BackupRetentionPeriod) != 7 {
		t.Fatal(queued, err)
	}

	active := now.Add(-time.Minute).Format("Mon:15:04") + "-" + now.Add(40*time.Minute).Format("Mon:15:04")
	_, err = c.ModifyDBInstance(ctx, &rds.ModifyDBInstanceInput{
		DBInstanceIdentifier: in.DBInstanceIdentifier, PreferredMaintenanceWindow: new(active),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ModifyDBCluster(ctx, &rds.ModifyDBClusterInput{
		DBClusterIdentifier: cluster.DBClusterIdentifier, PreferredMaintenanceWindow: new(active),
	})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.NewTimer(6 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		d, describeErr := c.DescribeDBInstances(
			ctx,
			&rds.DescribeDBInstancesInput{DBInstanceIdentifier: in.DBInstanceIdentifier},
		)
		if describeErr != nil {
			t.Fatal(describeErr)
		}

		out, clusterErr := c.DescribeDBClusters(
			ctx,
			&rds.DescribeDBClustersInput{DBClusterIdentifier: cluster.DBClusterIdentifier},
		)
		if clusterErr != nil {
			t.Fatal(clusterErr)
		}

		db, cl := d.DBInstances[0], out.DBClusters[0]
		if aws.ToString(db.DBInstanceClass) == "db.t3.small" && aws.ToInt32(cl.BackupRetentionPeriod) == 7 {
			if db.PendingModifiedValues != nil || cl.PendingModifiedValues != nil {
				t.Fatalf("pending settings remained after maintenance: %+v, %+v", db, cl)
			}

			break
		}

		select {
		case <-deadline.C:
			t.Fatalf("maintenance did not apply: %+v, %+v", db, cl)
		case <-tick.C:
		}
	}

	_, err = c.ModifyDBCluster(ctx, &rds.ModifyDBClusterInput{
		DBClusterIdentifier: cluster.DBClusterIdentifier, PreferredMaintenanceWindow: new(window),
		BackupRetentionPeriod: new(int32(9)),
	})
	if err != nil {
		t.Fatal(err)
	}

	member := new("maintenance-member")
	_, err = c.CreateDBInstance(ctx, &rds.CreateDBInstanceInput{
		DBInstanceIdentifier: member, DBInstanceClass: new("db.r6g.large"),
		DBClusterIdentifier: cluster.DBClusterIdentifier, Engine: cluster.Engine,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := c.ModifyDBInstance(ctx, &rds.ModifyDBInstanceInput{
		DBInstanceIdentifier: member, DBInstanceClass: new("db.r6g.xlarge"),
	})
	if err != nil || result.DBInstance.PendingModifiedValues == nil ||
		aws.ToString(result.DBInstance.DBInstanceClass) != "db.r6g.large" {
		t.Fatal(result, err)
	}

	rebooted, err := c.RebootDBCluster(ctx, &rds.RebootDBClusterInput{DBClusterIdentifier: cluster.DBClusterIdentifier})
	if err != nil || aws.ToInt32(rebooted.DBCluster.BackupRetentionPeriod) != 9 ||
		rebooted.DBCluster.PendingModifiedValues != nil {
		t.Fatal(rebooted, err)
	}
	described, err := c.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{DBInstanceIdentifier: member})
	if err != nil || described.DBInstances[0].PendingModifiedValues != nil ||
		aws.ToString(described.DBInstances[0].DBInstanceClass) != "db.r6g.xlarge" {
		t.Fatal(described, err)
	}
}

func TestRDSReadReplicas(t *testing.T) {
	s := server(t)
	c := rdsClient(s, "test", "us-east-1")
	ctx := t.Context()
	in := rdsInput("primary-db")
	if _, err := c.CreateDBInstance(ctx, in); err != nil {
		t.Fatal(err)
	}

	create := func(name string) *rds.CreateDBInstanceReadReplicaInput {
		return &rds.CreateDBInstanceReadReplicaInput{
			DBInstanceIdentifier: new(name), SourceDBInstanceIdentifier: in.DBInstanceIdentifier,
			DBInstanceClass: new("db.t3.small"),
		}
	}

	other := rdsClient(s, "test", "us-west-2")
	_, err := other.CreateDBInstanceReadReplica(ctx, create("replica-db"))
	code(t, err, "DBInstanceNotFound")

	out, err := c.CreateDBInstanceReadReplica(ctx, create("replica-db"))
	if err != nil || aws.ToString(out.DBInstance.ReadReplicaSourceDBInstanceIdentifier) != "primary-db" ||
		aws.ToString(out.DBInstance.DBInstanceClass) != "db.t3.small" ||
		aws.ToString(out.DBInstance.EngineVersion) != "16" || aws.ToInt32(out.DBInstance.BackupRetentionPeriod) != 0 {
		t.Fatal(out, err)
	}

	_, err = c.CreateDBInstanceReadReplica(ctx, create("replica-db"))
	code(t, err, "DBInstanceAlreadyExists")
	db := func(name string) types.DBInstance {
		t.Helper()
		result, describeErr := c.DescribeDBInstances(
			ctx,
			&rds.DescribeDBInstancesInput{DBInstanceIdentifier: new(name)},
		)
		if describeErr != nil {
			t.Fatal(describeErr)
		}

		return result.DBInstances[0]
	}

	if ids := db("primary-db").ReadReplicaDBInstanceIdentifiers; !slices.Equal(ids, []string{"replica-db"}) {
		t.Fatal(ids)
	}

	promoted, err := c.PromoteReadReplica(ctx, &rds.PromoteReadReplicaInput{
		DBInstanceIdentifier: new("replica-db"), BackupRetentionPeriod: new(int32(2)),
	})
	if err != nil || promoted.DBInstance.ReadReplicaSourceDBInstanceIdentifier != nil ||
		aws.ToInt32(promoted.DBInstance.BackupRetentionPeriod) != 2 ||
		len(db("primary-db").ReadReplicaDBInstanceIdentifiers) != 0 {
		t.Fatal(promoted, err)
	}

	_, err = c.PromoteReadReplica(ctx, &rds.PromoteReadReplicaInput{DBInstanceIdentifier: new("replica-db")})
	code(t, err, "InvalidDBInstanceState")
	for _, name := range []string{"delete-replica", "keep-replica"} {
		if _, err := c.CreateDBInstanceReadReplica(ctx, create(name)); err != nil {
			t.Fatal(err)
		}
	}

	for _, name := range []string{"delete-replica", "primary-db"} {
		if _, err := c.DeleteDBInstance(ctx, &rds.DeleteDBInstanceInput{
			DBInstanceIdentifier: new(name), SkipFinalSnapshot: new(true),
		}); err != nil {
			t.Fatal(err)
		}
	}

	if db("keep-replica").ReadReplicaSourceDBInstanceIdentifier != nil {
		t.Fatal("deleting the source did not promote the remaining replica")
	}
}

func TestRDSLifecycleEvents(t *testing.T) {
	s := server(t)
	c := rdsClient(s, "test", "us-east-1")
	ctx := t.Context()
	in := rdsInput("events-db")
	start := time.Now().Add(-time.Second)
	if _, err := c.CreateDBInstance(ctx, in); err != nil {
		t.Fatal(err)
	}

	for i := range 19 {
		_, err := c.ModifyDBInstance(ctx, &rds.ModifyDBInstanceInput{
			DBInstanceIdentifier: in.DBInstanceIdentifier, CopyTagsToSnapshot: new(i%2 == 0),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err := c.RebootDBInstance(ctx, &rds.RebootDBInstanceInput{DBInstanceIdentifier: in.DBInstanceIdentifier})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteDBInstance(ctx, &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier: in.DBInstanceIdentifier, FinalDBSnapshotIdentifier: new("events-snapshot"),
	})
	if err != nil {
		t.Fatal(err)
	}

	query := &rds.DescribeEventsInput{
		SourceType: types.SourceTypeDbInstance, SourceIdentifier: in.DBInstanceIdentifier,
		StartTime: new(start), MaxRecords: new(int32(20)),
	}
	page, err := c.DescribeEvents(ctx, query)
	if err != nil || len(page.Events) != 20 || page.Marker == nil ||
		!slices.Equal(page.Events[0].EventCategories, []string{"creation"}) {
		t.Fatal(page, err)
	}

	query.Marker = page.Marker
	page, err = c.DescribeEvents(ctx, query)
	if err != nil || len(page.Events) != 2 || page.Marker != nil ||
		!slices.Equal(page.Events[0].EventCategories, []string{"availability"}) ||
		!slices.Equal(page.Events[1].EventCategories, []string{"deletion"}) ||
		aws.ToString(page.Events[1].SourceArn) != "arn:aws:rds:us-east-1:000000000000:db:events-db" {
		t.Fatal(page, err)
	}

	snapshots, err := c.DescribeEvents(ctx, &rds.DescribeEventsInput{
		SourceType: types.SourceTypeDbSnapshot, SourceIdentifier: new("events-snapshot"),
		EventCategories: []string{"creation"},
	})
	if err != nil || len(snapshots.Events) != 1 {
		t.Fatal(snapshots, err)
	}

	categories, err := c.DescribeEventCategories(
		ctx,
		&rds.DescribeEventCategoriesInput{SourceType: new(string(query.SourceType))},
	)
	if err != nil || len(categories.EventCategoriesMapList) != 1 ||
		!slices.Contains(categories.EventCategoriesMapList[0].EventCategories, "configuration change") {
		t.Fatal(categories, err)
	}

	other := rdsClient(s, "test", "us-west-2")
	page, err = other.DescribeEvents(ctx, &rds.DescribeEventsInput{})
	if err != nil || len(page.Events) != 0 {
		t.Fatal(page, err)
	}
}

func TestRDSOptionGroups(t *testing.T) {
	s := server(t)
	c := rdsClient(s, "test", "us-east-1")
	ctx := t.Context()
	defaults, err := c.DescribeOptionGroups(ctx, &rds.DescribeOptionGroupsInput{
		EngineName: new("mysql"), MajorEngineVersion: new("8.0"),
	})
	if err != nil || len(defaults.OptionGroupsList) != 1 ||
		aws.ToString(defaults.OptionGroupsList[0].OptionGroupName) != "default:mysql-8-0" {
		t.Fatal(defaults, err)
	}

	name := new("audit-options")
	_, err = c.CreateOptionGroup(ctx, &rds.CreateOptionGroupInput{
		EngineName: new("mysql"), MajorEngineVersion: new("8.0"), OptionGroupName: name,
		OptionGroupDescription: new("Audit configuration"),
	})
	if err != nil {
		t.Fatal(err)
	}

	in := rdsInput("option-db")
	start := time.Now().UTC().Add(24 * time.Hour)
	in.PreferredMaintenanceWindow = new(start.Format("Mon:15:04") + "-" + start.Add(time.Hour).Format("Mon:15:04"))
	in.OptionGroupName = name
	_, err = c.CreateDBInstance(ctx, in)
	code(t, err, "InvalidParameterValue")

	in.Engine = new("mysql")
	out, err := c.CreateDBInstance(ctx, in)
	if err != nil || len(out.DBInstance.OptionGroupMemberships) != 1 ||
		aws.ToString(out.DBInstance.OptionGroupMemberships[0].OptionGroupName) != *name {
		t.Fatal(out, err)
	}

	modified, err := c.ModifyOptionGroup(ctx, &rds.ModifyOptionGroupInput{
		OptionGroupName: name, ApplyImmediately: new(true),
		OptionsToInclude: []types.OptionConfiguration{{
			OptionName:     new("MARIADB_AUDIT_PLUGIN"),
			OptionSettings: []types.OptionSetting{{Name: new("SERVER_AUDIT_EVENTS"), Value: new("CONNECT,QUERY")}},
		}},
	})
	if err != nil || len(modified.OptionGroup.Options) != 1 ||
		aws.ToString(modified.OptionGroup.Options[0].OptionSettings[0].Value) != "CONNECT,QUERY" {
		t.Fatal(modified, err)
	}

	_, err = c.DeleteOptionGroup(ctx, &rds.DeleteOptionGroupInput{OptionGroupName: name})
	code(t, err, "InvalidOptionGroupStateFault")
	removed, err := c.ModifyOptionGroup(ctx, &rds.ModifyOptionGroupInput{
		OptionGroupName: name, ApplyImmediately: new(true), OptionsToRemove: []string{"MARIADB_AUDIT_PLUGIN"},
	})
	if err != nil || len(removed.OptionGroup.Options) != 0 {
		t.Fatal(removed, err)
	}

	pending, err := c.ModifyDBInstance(ctx, &rds.ModifyDBInstanceInput{
		DBInstanceIdentifier: in.DBInstanceIdentifier, OptionGroupName: new("default:mysql-8-0"),
	})
	if err != nil || len(pending.DBInstance.OptionGroupMemberships) != 2 ||
		aws.ToString(pending.DBInstance.OptionGroupMemberships[1].Status) != "pending-apply" {
		t.Fatal(pending, err)
	}

	_, err = c.DeleteOptionGroup(ctx, &rds.DeleteOptionGroupInput{OptionGroupName: name})
	code(t, err, "InvalidOptionGroupStateFault")
	rebooted, err := c.RebootDBInstance(ctx, &rds.RebootDBInstanceInput{DBInstanceIdentifier: in.DBInstanceIdentifier})
	if err != nil || len(rebooted.DBInstance.OptionGroupMemberships) != 1 ||
		aws.ToString(rebooted.DBInstance.OptionGroupMemberships[0].OptionGroupName) != "default:mysql-8-0" ||
		aws.ToString(rebooted.DBInstance.OptionGroupMemberships[0].Status) != "in-sync" {
		t.Fatal(rebooted, err)
	}

	_, err = c.DeleteOptionGroup(ctx, &rds.DeleteOptionGroupInput{OptionGroupName: name})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeOptionGroups(ctx, &rds.DescribeOptionGroupsInput{OptionGroupName: name})
	code(t, err, "OptionGroupNotFoundFault")
}

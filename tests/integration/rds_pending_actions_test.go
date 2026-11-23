package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/rds/types"
)

func TestRDSPendingMaintenanceActions(t *testing.T) {
	c := rdsClient(server(t), "test", "us-east-1")
	ctx := t.Context()
	in := rdsInput("pending")
	start := time.Now().UTC().Add(24 * time.Hour)
	in.PreferredMaintenanceWindow = new(start.Format("Mon:15:04") + "-" + start.Add(time.Hour).Format("Mon:15:04"))
	db, err := c.CreateDBInstance(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ModifyDBInstance(ctx, &rds.ModifyDBInstanceInput{
		DBInstanceIdentifier: in.DBInstanceIdentifier, AllocatedStorage: new(int32(35)),
	})
	if err != nil {
		t.Fatal(err)
	}

	identifier := db.DBInstance.DBInstanceArn
	inquiry := &rds.DescribePendingMaintenanceActionsInput{
		Filters: []types.Filter{{Name: new("db-instance-id"), Values: []string{*identifier}}},
	}
	pending, err := c.DescribePendingMaintenanceActions(ctx, inquiry)
	if err != nil || len(pending.PendingMaintenanceActions) != 1 {
		t.Fatal(pending, err)
	}

	details := pending.PendingMaintenanceActions[0].PendingMaintenanceActionDetails
	if len(details) != 1 || aws.ToString(details[0].Action) != "system-update" || details[0].CurrentApplyDate == nil {
		t.Fatal(details)
	}

	for _, optIn := range []string{"next-maintenance", "undo-opt-in", "immediate"} {
		out, applyErr := c.ApplyPendingMaintenanceAction(ctx, &rds.ApplyPendingMaintenanceActionInput{
			ResourceIdentifier: identifier, ApplyAction: details[0].Action, OptInType: new(optIn),
		})
		if applyErr != nil {
			t.Fatal(applyErr)
		}

		actions := out.ResourcePendingMaintenanceActions.PendingMaintenanceActionDetails
		switch optIn {
		case "next-maintenance":
			if len(actions) != 1 || aws.ToString(actions[0].OptInStatus) != optIn {
				t.Fatal(actions)
			}
		case "undo-opt-in":
			if len(actions) != 1 || actions[0].OptInStatus != nil {
				t.Fatal(actions)
			}
		case "immediate":
			if len(actions) != 0 {
				t.Fatal(actions)
			}
		}
	}

	pending, err = c.DescribePendingMaintenanceActions(ctx, inquiry)
	if err != nil || len(pending.PendingMaintenanceActions) != 0 {
		t.Fatal(pending, err)
	}

	instances, err := c.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: in.DBInstanceIdentifier,
	})
	if err != nil || aws.ToInt32(instances.DBInstances[0].AllocatedStorage) != 35 {
		t.Fatal(instances, err)
	}
}

func TestRDSAuroraFailover(t *testing.T) {
	c := rdsClient(server(t), "test", "us-east-1")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	in := auroraInput("failover", "aurora-postgresql")
	if _, err := c.CreateDBCluster(ctx, in); err != nil {
		t.Fatal(err)
	}

	waitAurora(t, ctx, c, *in.DBClusterIdentifier)
	for _, name := range []string{"first", "second"} {
		if _, err := c.CreateDBInstance(ctx, &rds.CreateDBInstanceInput{
			DBInstanceIdentifier: new(name), DBClusterIdentifier: in.DBClusterIdentifier,
			DBInstanceClass: new("db.t4g.medium"), Engine: in.Engine,
		}); err != nil {
			t.Fatal(err)
		}
	}

	for _, target := range []string{"first", "missing"} {
		_, err := c.FailoverDBCluster(ctx, &rds.FailoverDBClusterInput{
			DBClusterIdentifier: in.DBClusterIdentifier, TargetDBInstanceIdentifier: new(target),
		})
		code(t, err, "InvalidDBInstanceState")
	}

	out, err := c.FailoverDBCluster(ctx, &rds.FailoverDBClusterInput{
		DBClusterIdentifier: in.DBClusterIdentifier, TargetDBInstanceIdentifier: new("second"),
	})
	if err != nil || len(out.DBCluster.DBClusterMembers) != 2 {
		t.Fatal(out, err)
	}

	for _, member := range out.DBCluster.DBClusterMembers {
		if aws.ToBool(member.IsClusterWriter) != (aws.ToString(member.DBInstanceIdentifier) == "second") {
			t.Fatal(member)
		}
	}

	events, err := c.DescribeEvents(ctx, &rds.DescribeEventsInput{
		SourceType:       types.SourceTypeDbCluster,
		SourceIdentifier: in.DBClusterIdentifier,
		EventCategories:  []string{"failover"},
	})
	if err != nil || len(events.Events) != 2 ||
		aws.ToString(events.Events[1].Message) != "DB cluster failover completed" {
		t.Fatal(events, err)
	}
}

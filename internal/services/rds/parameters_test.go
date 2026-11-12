package rds

import (
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestParameterRuntimeStatus(t *testing.T) {
	s := New(memory.New(nil, nil))
	rt := &fakeRuntime{ready: true}
	s.runtime, s.owner = rt, "parameter-test"
	ctx := runtimeContext()
	_, err := s.CreateDBParameterGroup(
		ctx,
		&api.CreateDBParameterGroupMessage{
			DBParameterGroupName:   new("custom"),
			DBParameterGroupFamily: new("postgres16"),
			Description:            new("test"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.CreateDBInstance(
		ctx,
		&api.CreateDBInstanceMessage{
			DBInstanceIdentifier: new("parameter-db"),
			DBInstanceClass:      new("db.t3.micro"),
			Engine:               new(EnginePostgres),
			MasterUsername:       new("master"),
			MasterUserPassword:   new("password123"),
			DBParameterGroupName: new("custom"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	s.visit(ctx)
	_, err = s.ModifyDBParameterGroup(
		ctx,
		&api.ModifyDBParameterGroupMessage{
			DBParameterGroupName: new("custom"),
			Parameters: []api.Parameter{
				{ParameterName: new("work_mem"), ParameterValue: new("8192"), ApplyMethod: new(parameterImmediate)},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	s.visit(ctx)
	out, err := s.DescribeDBInstances(ctx, &api.DescribeDBInstancesMessage{DBInstanceIdentifier: new("parameter-db")})
	if err != nil {
		t.Fatal(err)
	}

	if status := common.Value(
		out.DBInstances[0].DBParameterGroups[0].ParameterApplyStatus,
	); status != parameterPending {
		t.Fatalf("pending changes must survive readiness probes: %s", status)
	}

	_, err = s.RebootDBInstance(ctx, &api.RebootDBInstanceMessage{DBInstanceIdentifier: new("parameter-db")})
	if err != nil {
		t.Fatal(err)
	}

	s.visit(ctx)
	out, err = s.DescribeDBInstances(ctx, &api.DescribeDBInstancesMessage{DBInstanceIdentifier: new("parameter-db")})
	if err != nil {
		t.Fatal(err)
	}

	if status := common.Value(out.DBInstances[0].DBParameterGroups[0].ParameterApplyStatus); status != parameterInSync {
		t.Fatalf("reboot: %s", status)
	}

	if rt.specs[len(rt.specs)-1].Parameters["work_mem"] != "8192" {
		t.Fatalf("startup parameters missing: %+v", rt.specs)
	}
}

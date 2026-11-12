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

// Docker engines accept only the values they honour for parameters they do not implement.
func TestDockerClusterParameterValues(t *testing.T) {
	s := New(memory.New(nil, nil))
	s.runtime, s.owner = &fakeRuntime{ready: true}, "parameter-test"
	ctx := runtimeContext()
	if _, err := s.CreateDBClusterParameterGroup(ctx, &api.CreateDBClusterParameterGroupMessage{
		DBClusterParameterGroupName: new("documents"), DBParameterGroupFamily: new("docdb5.0"),
		Description: new("test"),
	}); err != nil {
		t.Fatal(err)
	}

	modify := func(name, value string) error {
		_, err := s.ModifyDBClusterParameterGroup(ctx, &api.ModifyDBClusterParameterGroupMessage{
			DBClusterParameterGroupName: new("documents"),
			Parameters: []api.Parameter{
				{ParameterName: new(name), ParameterValue: new(value), ApplyMethod: new(parameterPending)},
			},
		})
		return err
	}

	if err := modify("tls", "enabled"); err == nil {
		t.Fatal("Docker accepted tls=enabled")
	}

	for name, value := range map[string]string{"tls": "disabled", "audit_logs": "none", "profiler": "enabled"} {
		if err := modify(name, value); err != nil {
			t.Fatal(name, err)
		}
	}
}

// Docker Neptune members share their cluster's server, so instance groups keep engine defaults.
func TestDockerNeptuneInstanceParameters(t *testing.T) {
	s := New(memory.New(nil, nil))
	s.runtime, s.owner = &fakeRuntime{ready: true}, "parameter-test"
	ctx := runtimeContext()
	if _, err := s.CreateDBParameterGroup(ctx, &api.CreateDBParameterGroupMessage{
		DBParameterGroupName: new("member"), DBParameterGroupFamily: new("neptune1.3"), Description: new("test"),
	}); err != nil {
		t.Fatal(err)
	}

	modify := func(value string) error {
		_, err := s.ModifyDBParameterGroup(ctx, &api.ModifyDBParameterGroupMessage{
			DBParameterGroupName: new("member"),
			Parameters: []api.Parameter{{
				ParameterName: new("neptune_query_timeout"), ParameterValue: new(value),
				ApplyMethod: new(parameterPending),
			}},
		})
		return err
	}

	if err := modify("5000"); err == nil {
		t.Fatal("Docker accepted an instance-level Neptune timeout")
	}

	if err := modify("120000"); err != nil {
		t.Fatal(err)
	}
}

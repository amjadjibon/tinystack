package rds

import (
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestDocumentClusterRuntime(t *testing.T) {
	for _, version := range []string{"3.6", "4.0", "5.0"} {
		t.Run(version, func(t *testing.T) {
			s := New(memory.New(nil, nil))
			rt := &fakeRuntime{ready: true}
			s.runtime = rt
			in := clusterInput("documents", EngineDocDB)
			in.EngineVersion = new(version)
			out, err := s.CreateDBCluster(runtimeContext(), in)
			if err != nil || common.Value(out.DBCluster.Status) != creating || out.DBCluster.Endpoint != nil {
				t.Fatal(out, err)
			}

			s.visit(runtimeContext())
			if len(rt.specs) != 1 || rt.specs[0].Image != "mongo:"+version || rt.specs[0].Port != 27017 ||
				rt.specs[0].Password != *in.MasterUserPassword {
				t.Fatal(rt.specs)
			}

			_, err = s.ModifyDBCluster(runtimeContext(), &api.ModifyDBClusterMessage{
				DBClusterIdentifier: in.DBClusterIdentifier, MasterUserPassword: new("changed-password"),
			})
			if err != nil || rt.password != "changed-password" {
				t.Fatal(err, rt.password)
			}

			if _, err := s.StopDBCluster(runtimeContext(), &api.StopDBClusterMessage{
				DBClusterIdentifier: in.DBClusterIdentifier,
			}); err != nil {
				t.Fatal(err)
			}

			if _, err := s.StartDBCluster(runtimeContext(), &api.StartDBClusterMessage{
				DBClusterIdentifier: in.DBClusterIdentifier,
			}); err != nil {
				t.Fatal(err)
			}

			s.visit(runtimeContext())
			if _, err := s.DeleteDBCluster(runtimeContext(), &api.DeleteDBClusterMessage{
				DBClusterIdentifier: in.DBClusterIdentifier, SkipFinalSnapshot: new(true),
			}); err != nil || len(rt.removed) != 1 || !rt.removed[0] {
				t.Fatal(err, rt.removed)
			}
		})
	}
}

func TestNeptuneClusterRuntime(t *testing.T) {
	for version, image := range map[string]string{
		"1.2.1.0": "tinkerpop/gremlin-server:3.6.2",
		"1.3.2.0": "tinkerpop/gremlin-server:3.7.1",
	} {
		t.Run(version, func(t *testing.T) {
			s := New(memory.New(nil, nil))
			rt := &fakeRuntime{ready: true}
			s.runtime = rt
			in := clusterInput("graph", EngineNeptune)
			in.EngineVersion = new(version)
			if _, err := s.CreateDBCluster(runtimeContext(), in); err != nil {
				t.Fatal(err)
			}

			s.visit(runtimeContext())
			if len(rt.specs) != 1 || rt.specs[0].Image != image || rt.specs[0].Engine != EngineNeptune ||
				rt.specs[0].Port != 8182 || rt.specs[0].Password != "" {
				t.Fatal(rt.specs)
			}
		})
	}
}

func TestClusterRuntimeRejectsCustomPort(t *testing.T) {
	for _, engine := range []string{EngineDocDB, EngineNeptune} {
		s := New(memory.New(nil, nil))
		s.runtime = &fakeRuntime{}
		in := clusterInput("custom-port", engine)
		in.Port = new(int64(27018))
		if _, err := s.CreateDBCluster(runtimeContext(), in); err == nil {
			t.Fatal("custom runtime port accepted", engine)
		}
	}
}

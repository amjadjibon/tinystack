package rds

import (
	"fmt"
	"strings"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func clusterInput(id, engine string) *api.CreateDBClusterMessage {
	in := &api.CreateDBClusterMessage{
		DBClusterIdentifier: new(id), Engine: new(engine),
		Tags: []api.Tag{{Key: new("owner"), Value: new("test")}},
	}

	if engine == engineDocDB {
		in.MasterUsername, in.MasterUserPassword = new("masteruser"), new(strings.Repeat("p", 100))
	}

	return in
}

func TestClusterLifecycle(t *testing.T) {
	for _, engine := range []string{engineDocDB, engineNeptune} {
		t.Run(engine, func(t *testing.T) {
			ctx := runtimeContext()
			store := memory.New(nil, nil)
			s := New(store)
			rt := &fakeRuntime{ready: true}
			created, err := s.CreateDBCluster(ctx, clusterInput("Mixed-Cluster", engine))
			if err != nil {
				t.Fatal(err)
			}

			c := created.DBCluster
			if common.Value(c.DBClusterIdentifier) != "mixed-cluster" || common.Value(c.Status) != available ||
				!strings.HasSuffix(common.Value(c.Endpoint), ".us-east-1."+engine+".amazonaws.com") ||
				!strings.Contains(common.Value(c.ReaderEndpoint), ".cluster-ro-") {
				t.Fatal(c)
			}

			if _, createErr := s.CreateDBCluster(ctx, clusterInput("mixed-cluster", engine)); createErr == nil {
				t.Fatal("duplicate accepted")
			}

			if _, describeErr := s.DescribeDBClusters(ctx, &api.DescribeDBClustersMessage{
				DBClusterIdentifier: new("missing"),
			}); describeErr == nil {
				t.Fatal("missing accepted")
			}

			for _, scope := range []request.Scope{
				{AccountID: "other", Region: "us-east-1"},
				{AccountID: "000000000000", Region: "us-west-2"},
			} {
				isolated := request.WithMetadata(ctx, request.Metadata{Scope: scope})
				described, scopeErr := s.DescribeDBClusters(isolated, &api.DescribeDBClustersMessage{})
				if scopeErr != nil || len(described.DBClusters) != 0 {
					t.Fatal(described, scopeErr)
				}
			}

			member, err := s.CreateDBInstance(ctx, &api.CreateDBInstanceMessage{
				DBInstanceIdentifier: new("member"),
				DBInstanceClass: new(
					"db.r5.large",
				),
				Engine:              new(engine),
				DBClusterIdentifier: c.DBClusterIdentifier,
			})
			if err != nil || common.Value(member.DBInstance.Endpoint.Address) != common.Value(c.Endpoint) {
				t.Fatal(member, err)
			}

			if _, deleteErr := s.DeleteDBCluster(ctx, &api.DeleteDBClusterMessage{
				DBClusterIdentifier: c.DBClusterIdentifier, SkipFinalSnapshot: new(true),
			}); deleteErr == nil {
				t.Fatal("deleted parent with member")
			}

			_, err = s.ModifyDBCluster(ctx, &api.ModifyDBClusterMessage{
				DBClusterIdentifier: c.DBClusterIdentifier,
				Port: new(
					int64(8183),
				),
				BackupRetentionPeriod: new(int64(0)),
				DeletionProtection:    new(true),
			})
			if err == nil {
				t.Fatal("invalid candidate accepted")
			}

			described, err := s.DescribeDBClusters(
				ctx,
				&api.DescribeDBClustersMessage{DBClusterIdentifier: c.DBClusterArn},
			)
			if err != nil || common.Value(described.DBClusters[0].Port) != common.Value(c.Port) ||
				common.Value(
					described.DBClusters[0].DeletionProtection,
				) || len(described.DBClusters[0].DBClusterMembers) != 1 {
				t.Fatal(described, err)
			}

			modified, err := s.ModifyDBCluster(ctx, &api.ModifyDBClusterMessage{
				DBClusterIdentifier: c.DBClusterIdentifier,
				Port: new(
					int64(8183),
				),
				BackupRetentionPeriod: new(int64(35)),
				DeletionProtection:    new(true),
				ApplyImmediately:      new(false),
			})
			if err != nil ||
				common.Value(modified.DBCluster.DbClusterResourceId) != common.Value(c.DbClusterResourceId) {
				t.Fatal(modified, err)
			}

			members, err := s.DescribeDBInstances(ctx, &api.DescribeDBInstancesMessage{})
			if err != nil || common.Value(members.DBInstances[0].Endpoint.Port) != 8183 {
				t.Fatal(members, err)
			}

			_, err = s.AddTagsToResource(ctx, &api.AddTagsToResourceMessage{
				ResourceName: c.DBClusterArn,
				Tags:         []api.Tag{{Key: new("extra"), Value: new("yes")}},
			})
			if err != nil {
				t.Fatal(err)
			}

			_, err = s.RemoveTagsFromResource(ctx, &api.RemoveTagsFromResourceMessage{
				ResourceName: c.DBClusterArn,
				TagKeys:      []string{"owner"},
			})
			if err != nil {
				t.Fatal(err)
			}

			tags, err := s.ListTagsForResource(ctx, &api.ListTagsForResourceMessage{ResourceName: c.DBClusterArn})
			if err != nil || len(tags.TagList) != 1 || common.Value(tags.TagList[0].Key) != "extra" {
				t.Fatal(tags, err)
			}

			if len(rt.specs) != 0 {
				t.Fatal("member reached runtime")
			}

			_, err = s.ModifyDBInstance(ctx, &api.ModifyDBInstanceMessage{
				DBInstanceIdentifier: new("member"),
				DBInstanceClass:      new("db.r5.xlarge"), ApplyImmediately: new(false),
			})
			if err != nil {
				t.Fatal(err)
			}

			deleted, err := s.DeleteDBInstance(ctx, &api.DeleteDBInstanceMessage{DBInstanceIdentifier: new("member")})
			if err != nil || common.Value(deleted.DBInstance.DBInstanceStatus) != "deleting" || len(rt.removed) != 0 {
				t.Fatal(deleted, err)
			}

			if _, deleteErr := s.DeleteDBCluster(ctx, &api.DeleteDBClusterMessage{
				DBClusterIdentifier: c.DBClusterIdentifier, SkipFinalSnapshot: new(true),
			}); deleteErr == nil {
				t.Fatal("deleted protected parent")
			}

			_, err = s.ModifyDBCluster(ctx, &api.ModifyDBClusterMessage{
				DBClusterIdentifier: c.DBClusterIdentifier,
				DeletionProtection:  new(false),
			})
			if err != nil {
				t.Fatal(err)
			}

			if _, deleteErr := s.DeleteDBCluster(ctx, &api.DeleteDBClusterMessage{
				DBClusterIdentifier: c.DBClusterIdentifier,
			}); deleteErr == nil {
				t.Fatal("snapshot request accepted")
			}

			if _, deleteErr := s.DeleteDBCluster(ctx, &api.DeleteDBClusterMessage{
				DBClusterIdentifier: c.DBClusterIdentifier,
				SkipFinalSnapshot:   new(true), FinalDBSnapshotIdentifier: new("snapshot"),
			}); deleteErr == nil {
				t.Fatal("conflicting snapshot accepted")
			}

			deletedCluster, err := s.DeleteDBCluster(ctx, &api.DeleteDBClusterMessage{
				DBClusterIdentifier: c.DBClusterIdentifier,
				SkipFinalSnapshot:   new(true),
			})
			if err != nil || common.Value(deletedCluster.DBCluster.Status) != "deleting" {
				t.Fatal(deletedCluster, err)
			}

			if err := store.View(ctx, func(tx storage.ReadTx) error {
				for _, key := range tx.Keys("") {
					raw, _ := tx.Get(key)
					if strings.Contains(string(raw), strings.Repeat("p", 100)) {
						t.Fatal("plaintext password stored")
					}
				}

				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestClusterValidationAndPagination(t *testing.T) {
	ctx := runtimeContext()
	s := New(memory.New(nil, nil))
	for _, engine := range []string{"aurora-mysql", "aurora-postgresql", "postgres", "unknown"} {
		if _, err := s.CreateDBCluster(ctx, clusterInput("unsupported", engine)); err == nil {
			t.Fatal(engine)
		}
	}

	for i := range 21 {
		in := clusterInput(fmt.Sprintf("cluster-%02d", i), engineDocDB)
		if _, err := s.CreateDBCluster(ctx, in); err != nil {
			t.Fatal(err)
		}
	}

	first, err := s.DescribeDBClusters(ctx, &api.DescribeDBClustersMessage{MaxRecords: new(int64(20))})
	if err != nil || len(first.DBClusters) != 20 || first.Marker == nil {
		t.Fatal(first, err)
	}

	_, err = s.DeleteDBCluster(ctx, &api.DeleteDBClusterMessage{
		DBClusterIdentifier: first.DBClusters[19].DBClusterIdentifier,
		SkipFinalSnapshot:   new(true),
	})
	if err != nil {
		t.Fatal(err)
	}

	second, err := s.DescribeDBClusters(
		ctx,
		&api.DescribeDBClustersMessage{MaxRecords: new(int64(20)), Marker: first.Marker},
	)
	if err != nil || len(second.DBClusters) != 1 ||
		common.Value(second.DBClusters[0].DBClusterIdentifier) != "cluster-20" {
		t.Fatal(second, err)
	}

	for _, size := range []int64{19, 101} {
		if _, pageErr := s.DescribeDBClusters(
			ctx,
			&api.DescribeDBClustersMessage{MaxRecords: new(size)},
		); pageErr == nil {
			t.Fatal("invalid page size accepted", size)
		}
	}

	if _, markerErr := s.DescribeDBClusters(
		ctx,
		&api.DescribeDBClustersMessage{Marker: new("%%invalid%%")},
	); markerErr == nil {
		t.Fatal("invalid marker accepted")
	}

	in := clusterInput("invalid", engineDocDB)
	for _, mutate := range []func(*api.CreateDBClusterMessage){
		func(in *api.CreateDBClusterMessage) { in.DBClusterIdentifier = new("a--b") },
		func(in *api.CreateDBClusterMessage) { in.MasterUsername = new("admin") },
		func(in *api.CreateDBClusterMessage) { in.MasterUserPassword = new("bad@pass") },
		func(in *api.CreateDBClusterMessage) { in.EngineVersion = new("unknown") },
		func(in *api.CreateDBClusterMessage) { in.KmsKeyId = new("key") },
	} {
		candidate := common.Clone(in)
		mutate(candidate)
		if _, validationErr := s.CreateDBCluster(ctx, candidate); validationErr == nil {
			t.Fatal("invalid create accepted", candidate)
		}
	}

	_, err = s.ModifyDBCluster(ctx, &api.ModifyDBClusterMessage{
		DBClusterIdentifier: new("cluster-00"),
		EngineVersion:       new("4.0"), BackupRetentionPeriod: new(int64(7)),
	})
	if err == nil {
		t.Fatal("version change accepted")
	}

	unchanged, err := s.DescribeDBClusters(ctx, &api.DescribeDBClustersMessage{DBClusterIdentifier: new("cluster-00")})
	if err != nil || common.Value(unchanged.DBClusters[0].BackupRetentionPeriod) != 1 {
		t.Fatal(unchanged, err)
	}
}

// Keep the metadata-only password check before deletion too, while records still exist.
func TestClusterPasswordNotStored(t *testing.T) {
	ctx := runtimeContext()
	store := memory.New(nil, nil)
	s := New(store)
	in := clusterInput("secret", engineDocDB)
	if _, err := s.CreateDBCluster(ctx, in); err != nil {
		t.Fatal(err)
	}

	if _, err := s.ModifyDBCluster(ctx, &api.ModifyDBClusterMessage{
		DBClusterIdentifier: in.DBClusterIdentifier,
		MasterUserPassword:  new("changed-password"),
	}); err != nil {
		t.Fatal(err)
	}

	if err := store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			raw, _ := tx.Get(key)
			if strings.Contains(string(raw), "changed-password") ||
				strings.Contains(string(raw), *in.MasterUserPassword) {
				t.Fatal("plaintext password stored")
			}
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

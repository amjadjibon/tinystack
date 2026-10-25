package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/docdb"
	"github.com/aws/aws-sdk-go-v2/service/neptune"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	cfnapi "github.com/amjadjibon/tinystack/internal/aws/generated/cloudformation"
	rdsapi "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	secretapi "github.com/amjadjibon/tinystack/internal/aws/generated/secretsmanager"
	"github.com/amjadjibon/tinystack/internal/gateway"
	cfnsvc "github.com/amjadjibon/tinystack/internal/services/cloudformation"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	rdssvc "github.com/amjadjibon/tinystack/internal/services/rds"
	rdsdocker "github.com/amjadjibon/tinystack/internal/services/rds/docker"
	secretsvc "github.com/amjadjibon/tinystack/internal/services/secretsmanager"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

func clusterDockerServer(t *testing.T, store storage.Store) (server *httptest.Server, stop func()) {
	t.Helper()
	service := rdssvc.New(store)
	if err := service.UseRuntime(isolatedRDSRuntime{rdsdocker.New()}); err != nil {
		t.Fatal(err)
	}

	secrets := secretsvc.New(store, nil, nil, nil)
	stacks := cfnsvc.New(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	services := []catalog.Service{rdsapi.Descriptor(service), secretapi.Descriptor(secrets), cfnapi.Descriptor(stacks)}
	caller := catalog.NewCaller(services...)
	service.UseCaller(caller)
	stacks.UseCaller(caller)
	h, err := gateway.New(testConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)),
		services...)
	if err != nil {
		t.Fatal(err)
	}

	s := httptest.NewServer(h)
	return s, func() { stacks.Stop(); service.Stop(); secrets.Stop(); s.Close() }
}

func TestRDSDockerDocumentDB(t *testing.T) {
	requireClusterDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	store, err := sqlite.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	defer store.Close()
	s, stop := clusterDockerServer(t, store)
	defer func() { stop() }() //nolint:gocritic // stop is reassigned after persistent restart.
	c := docdb.New(docdb.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	id := fmt.Sprintf("documents-%d", time.Now().UnixNano())
	member := id + "-member"
	password := "initial-password"

	_, err = c.CreateDBCluster(ctx, &docdb.CreateDBClusterInput{
		DBClusterIdentifier: new(id + "-invalid"), Engine: new("docdb"), MasterUsername: new("master"),
		MasterUserPassword: new(password), Port: new(int32(27018)),
	})
	code(t, err, "InvalidParameterValue")
	var spec *rdssvc.Spec
	defer func() {
		if spec != nil {
			_ = rdsdocker.New().Remove(context.Background(), spec, true)
		}
	}()
	out, err := c.CreateDBCluster(ctx, &docdb.CreateDBClusterInput{
		DBClusterIdentifier: new(
			id,
		),
		Engine:             new("docdb"),
		MasterUsername:     new("master"),
		MasterUserPassword: new(password),
	})
	if err != nil || aws.ToString(out.DBCluster.Status) != "creating" || out.DBCluster.Endpoint != nil {
		t.Fatal(out, err)
	}

	_, err = c.CreateDBInstance(ctx, &docdb.CreateDBInstanceInput{
		DBInstanceIdentifier: new(
			member,
		),
		DBClusterIdentifier: new(id),
		DBInstanceClass:     new("db.r5.large"),
		Engine:              new("docdb"),
	})
	if err != nil {
		t.Fatal(err)
	}

	r := rdsClient(s, "test", "us-east-1")
	cluster := waitAurora(t, ctx, r, id)

	_, err = r.CreateDBClusterSnapshot(ctx, &rds.CreateDBClusterSnapshotInput{
		DBClusterIdentifier: new(id), DBClusterSnapshotIdentifier: new(id + "-snapshot"),
	})
	code(t, err, "InvalidParameterValue")
	_, err = c.ModifyDBCluster(ctx, &docdb.ModifyDBClusterInput{
		DBClusterIdentifier: new(id), ApplyImmediately: new(false),
	})
	code(t, err, "InvalidParameterValue")
	_, err = c.ModifyDBCluster(ctx, &docdb.ModifyDBClusterInput{
		DBClusterIdentifier: new(id), Port: new(int32(27018)),
	})
	code(t, err, "InvalidParameterValue")
	instances, err := c.DescribeDBInstances(ctx, &docdb.DescribeDBInstancesInput{DBInstanceIdentifier: new(member)})
	if err != nil || aws.ToString(instances.DBInstances[0].Endpoint.Address) != "127.0.0.1" ||
		aws.ToInt32(instances.DBInstances[0].Endpoint.Port) != aws.ToInt32(cluster.Port) {
		t.Fatal(instances, err)
	}

	// Identify only this test's container; discovery must never clean other runtimes.
	container, err := clusterContainer(ctx, aws.ToString(cluster.DBClusterArn))
	if err != nil || container == "" {
		t.Fatal(container, err)
	}

	{
		name := container
		volume, inspectErr := dockercli.Run(
			ctx,
			nil,
			"inspect",
			"--format",
			"{{range .Mounts}}{{if eq .Destination \"/data/db\"}}{{.Name}}{{end}}{{end}}",
			name,
		)
		if inspectErr != nil {
			t.Fatal(inspectErr)
		}

		spec = &rdssvc.Spec{Name: name, Volume: strings.TrimSpace(volume)}
	}

	query := func(script string) {
		t.Helper()
		_, queryErr := dockercli.Run(ctx, nil, "exec", spec.Name, "mongosh", "--quiet", "--username", "master",
			"--password", password, "--authenticationDatabase", "admin", "test", "--eval", script)
		if queryErr != nil {
			t.Fatal(queryErr)
		}
	}
	query(
		"db.documents.insertOne({_id:1,value:'durable'}); if(db.documents.findOne({_id:1}).value !== 'durable') quit(1)",
	)
	_, err = c.ModifyDBCluster(
		ctx,
		&docdb.ModifyDBClusterInput{DBClusterIdentifier: new(id), MasterUserPassword: new("changed-password")},
	)
	if err != nil {
		t.Fatal(err)
	}

	password = "changed-password"
	query("if(db.documents.countDocuments({_id:1}) !== 1) quit(1)")
	_, err = c.StopDBCluster(ctx, &docdb.StopDBClusterInput{DBClusterIdentifier: new(id)})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.StartDBCluster(ctx, &docdb.StartDBClusterInput{DBClusterIdentifier: new(id)})
	if err != nil {
		t.Fatal(err)
	}

	waitAurora(t, ctx, r, id)
	query("if(db.documents.countDocuments({_id:1}) !== 1) quit(1)")
	stop()
	s, stop = clusterDockerServer(t, store)
	r = rdsClient(s, "test", "us-east-1")
	cluster = waitAurora(t, ctx, r, id)
	container, err = clusterContainer(ctx, aws.ToString(cluster.DBClusterArn))
	if err != nil || container == "" {
		t.Fatal(container, err)
	}

	spec.Name = container

	query("if(db.documents.findOne({_id:1}).value !== 'durable') quit(1)")
	c = docdb.New(docdb.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	_, err = c.DeleteDBInstance(ctx, &docdb.DeleteDBInstanceInput{DBInstanceIdentifier: new(member)})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteDBCluster(
		ctx,
		&docdb.DeleteDBClusterInput{DBClusterIdentifier: new(id), SkipFinalSnapshot: new(true)},
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, resource := range [][]string{{"inspect", spec.Name}, {"volume", "inspect", spec.Volume}} {
		if _, inspectErr := dockercli.Run(ctx, nil, resource...); inspectErr == nil || !dockercli.Gone(inspectErr) {
			t.Fatal("resource survived delete", resource, inspectErr)
		}
	}

	_, err = r.DescribeDBClusters(ctx, &rds.DescribeDBClustersInput{DBClusterIdentifier: new(id)})
	code(t, err, "DBClusterNotFoundFault")
}

func clusterContainer(ctx context.Context, arn string) (string, error) {
	out, err := dockercli.Run(ctx, nil, "ps", "-a", "--filter", "label=tinystack.rds="+arn, "--format", "{{.ID}}")
	return strings.TrimSpace(out), err
}

func TestRDSDockerNeptune(t *testing.T) {
	requireClusterDocker(t)

	for _, version := range []string{"1.2.1.0", "1.3.2.0"} {
		t.Run(version, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			store, err := sqlite.Open(ctx, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}

			defer store.Close()
			s, stop := clusterDockerServer(t, store)
			defer stop()
			c := neptune.New(neptune.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
			id := fmt.Sprintf("graph-%d", time.Now().UnixNano())
			defer func() {
				_, _ = c.DeleteDBCluster(context.Background(), &neptune.DeleteDBClusterInput{
					DBClusterIdentifier: new(id), SkipFinalSnapshot: new(true),
				})
			}()
			_, err = c.CreateDBCluster(ctx, &neptune.CreateDBClusterInput{
				DBClusterIdentifier: new(id), Engine: new("neptune"), EngineVersion: new(version),
			})
			if err != nil {
				t.Fatal(err)
			}

			cluster := waitAurora(t, ctx, rdsClient(s, "test", "us-east-1"), id)
			container, err := clusterContainer(ctx, aws.ToString(cluster.DBClusterArn))
			if err != nil || container == "" {
				t.Fatal(container, err)
			}

			req, err := http.NewRequestWithContext(
				ctx,
				http.MethodPost,
				fmt.Sprintf("http://127.0.0.1:%d/gremlin", aws.ToInt32(cluster.Port)),
				strings.NewReader(
					`{"gremlin":"g.addV('person').property('name','tiny').iterate(); g.V().values('name')"}`,
				),
			)
			if err != nil {
				t.Fatal(err)
			}

			req.Header.Set("Content-Type", "application/json")
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}

			defer response.Body.Close()
			var result map[string]any
			if decodeErr := json.NewDecoder(response.Body).Decode(&result); decodeErr != nil {
				t.Fatal(decodeErr)
			}

			encoded, _ := json.Marshal(result)
			if response.StatusCode != http.StatusOK || !strings.Contains(string(encoded), "tiny") {
				t.Fatal(string(encoded))
			}

			_, err = c.DeleteDBCluster(ctx, &neptune.DeleteDBClusterInput{
				DBClusterIdentifier: new(id), SkipFinalSnapshot: new(true),
			})
			if err != nil {
				t.Fatal(err)
			}

			if _, err := dockercli.Run(ctx, nil, "inspect", container); err == nil || !dockercli.Gone(err) {
				t.Fatal("container survived delete", err)
			}
		})
	}
}

func TestRDSDockerDocumentDBManagedPassword(t *testing.T) {
	requireClusterDocker(t)

	s := auroraDockerServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	c := docdb.New(docdb.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	id := fmt.Sprintf("managed-documents-%d", time.Now().UnixNano())
	out, err := c.CreateDBCluster(ctx, &docdb.CreateDBClusterInput{
		DBClusterIdentifier: new(
			id,
		),
		Engine:                   new("docdb"),
		MasterUsername:           new("master"),
		ManageMasterUserPassword: new(true),
	})
	if err != nil || out.DBCluster.MasterUserSecret == nil {
		t.Fatal(out, err)
	}

	secretClient := secretsmanager.New(secretsmanager.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test"),
	})
	value, err := secretClient.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: out.DBCluster.MasterUserSecret.SecretArn,
	})
	if err != nil {
		t.Fatal(err)
	}

	var credentials map[string]string
	if decodeErr := json.Unmarshal([]byte(aws.ToString(value.SecretString)), &credentials); decodeErr != nil {
		t.Fatal(decodeErr)
	}

	cluster := waitAurora(t, ctx, rdsClient(s, "test", "us-east-1"), id)
	container, err := clusterContainer(ctx, aws.ToString(cluster.DBClusterArn))
	if err != nil {
		t.Fatal(err)
	}

	_, err = dockercli.Run(
		ctx,
		nil,
		"exec",
		container,
		"mongosh",
		"--quiet",
		"--username",
		credentials["username"],
		"--password",
		credentials["password"],
		"--authenticationDatabase",
		"admin",
		"--eval",
		"db.adminCommand({ping:1})",
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ModifyDBCluster(ctx, &docdb.ModifyDBClusterInput{
		DBClusterIdentifier: new(id), MasterUserPassword: new("cannot-change-managed"),
	})
	code(t, err, "InvalidParameterValue")
	_, err = c.DeleteDBCluster(ctx, &docdb.DeleteDBClusterInput{
		DBClusterIdentifier: new(id), SkipFinalSnapshot: new(true),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = secretClient.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: out.DBCluster.MasterUserSecret.SecretArn,
	})
	code(t, err, "ResourceNotFoundException")
}

func TestRDSDockerClusterCloudFormation(t *testing.T) {
	requireClusterDocker(t)

	for _, engine := range []string{"docdb", "neptune"} {
		t.Run(engine, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			store, err := sqlite.Open(ctx, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}

			defer store.Close()
			s, stop := clusterDockerServer(t, store)
			defer stop()
			cf := cfnClient(s)
			r := rdsClient(s, "test", "us-east-1")
			id := fmt.Sprintf("cfn-%s-%d", engine, time.Now().UnixNano())
			member := id + "-member"
			defer func() {
				_, _ = r.DeleteDBInstance(context.Background(), &rds.DeleteDBInstanceInput{
					DBInstanceIdentifier: new(member), SkipFinalSnapshot: new(true),
				})
				_, _ = r.DeleteDBCluster(context.Background(), &rds.DeleteDBClusterInput{
					DBClusterIdentifier: new(id), SkipFinalSnapshot: new(true),
				})
			}()
			version, port := "5.0", 27017
			if engine == "neptune" {
				version, port = "1.3.2.0", 8182
			}

			template := strings.ReplaceAll(clusterStackTemplate(engine, version, member, "db.r5.large", "one", port),
				engine+"-cluster", id)
			_, err = cf.CreateStack(ctx, &cfn.CreateStackInput{StackName: new(id), TemplateBody: new(template)})
			if err != nil {
				t.Fatal(err)
			}

			stack := wantStatus(ctx, t, cf, id, cfntypes.StackStatusCreateComplete)
			outputs := outputsOf(&stack)
			cluster := waitAurora(t, ctx, r, id)
			if outputs["Endpoint"] != "127.0.0.1" || outputs["Endpoint"] != outputs["InstanceEndpoint"] ||
				outputs["Port"] != fmt.Sprint(aws.ToInt32(cluster.Port)) || outputs["Port"] != outputs["InstancePort"] {
				t.Fatal(outputs, cluster)
			}
		})
	}
}

func requireClusterDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1 for live cluster engines")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := dockercli.Run(ctx, nil, "info"); err != nil {
		t.Skipf("Docker daemon unavailable: %v", err)
	}
}

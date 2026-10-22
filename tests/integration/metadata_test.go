package integration_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/endpointcreds"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/amjadjibon/tinystack/internal/app"
	ecsapi "github.com/amjadjibon/tinystack/internal/aws/generated/ecs"
	"github.com/amjadjibon/tinystack/internal/gateway"
	orchestration "github.com/amjadjibon/tinystack/internal/services/ecs"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func serverWithApp(t *testing.T, runtime ...*containerRuntime) (*httptest.Server, *app.App) {
	t.Helper()

	store := &memory.Store{}
	tinystack, err := app.New(
		context.Background(),
		testConfig(),
		store,
		&fakeRuntime{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}

	var handler http.Handler = tinystack.Gateway
	var tasks *orchestration.Service
	if len(runtime) > 0 {
		tasks = orchestration.New(store)
		h, gatewayErr := gateway.New(
			testConfig(),
			slog.New(slog.NewTextHandler(io.Discard, nil)),
			ecsapi.Descriptor(tasks),
		)
		if gatewayErr != nil {
			t.Fatal(gatewayErr)
		}

		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.Header.Get("X-Amz-Target"), "AmazonEC2ContainerServiceV20141113.") {
				h.ServeHTTP(w, r)
				return
			}

			tinystack.Gateway.ServeHTTP(w, r)
		})
	}

	s := httptest.NewServer(handler)
	if tasks != nil {
		tasks.UseEndpoint(s.URL)
		tasks.UseRuntime(runtime[0], nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
		t.Cleanup(tasks.Stop)
	}

	t.Cleanup(func() {
		s.Close()
		tinystack.Stop()
	})

	return s, tinystack
}

func createServiceRole(t *testing.T, s *httptest.Server, key, name, service string) string {
	t.Helper()

	trust := fmt.Sprintf(
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":%q},"Action":"sts:AssumeRole"}]}`,
		service,
	)
	created, err := iamClient(s, key, "us-east-1").CreateRole(context.Background(), &iam.CreateRoleInput{
		RoleName: aws.String(name), AssumeRolePolicyDocument: aws.String(trust),
	})
	if err != nil {
		t.Fatal(err)
	}

	return aws.ToString(created.Role.Arn)
}

func TestIMDS_SDK(t *testing.T) {
	s, tinystack := serverWithApp(t)
	ctx := context.Background()

	// 1. Create IAM Role and Instance Profile for EC2
	iamCli := iamClient(s, "test", "us-east-1")
	roleARN := createServiceRole(t, s, "test", "ec2-worker-role", "ec2.amazonaws.com")
	_ = roleARN
	_, err := iamCli.CreateInstanceProfile(ctx, &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String("ec2-worker-profile"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = iamCli.AddRoleToInstanceProfile(ctx, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String("ec2-worker-profile"),
		RoleName:            aws.String("ec2-worker-role"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// 2. Launch EC2 instance with user-data and instance profile
	ec2Cli := ec2Client(s, "test", "us-east-1")
	rawUserData := "echo tinystack-metadata-test"
	encodedUserData := base64.StdEncoding.EncodeToString([]byte(rawUserData))

	runOut, err := ec2Cli.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId:            latestImage(t, s),
		InstanceType:       ec2types.InstanceTypeT3Micro,
		MinCount:           aws.Int32(1),
		MaxCount:           aws.Int32(1),
		IamInstanceProfile: &ec2types.IamInstanceProfileSpecification{Name: aws.String("ec2-worker-profile")},
		UserData:           aws.String(encodedUserData),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(runOut.Instances) == 0 {
		t.Fatal("expected launched instance")
	}

	instanceID := aws.ToString(runOut.Instances[0].InstanceId)
	imdsPrefix := s.URL + "/_tinystack/imds/" + instanceID

	// The Go SDK replaces endpoint paths, so use the dedicated root listener.
	imdsServer := httptest.NewServer(tinystack.Metadata.IMDSHandler())
	t.Cleanup(imdsServer.Close)
	// 3. Connect AWS SDK IMDS client to the dedicated listener
	imdsClient := imds.New(imds.Options{
		Endpoint: imdsServer.URL,
	})

	// 4. Test IMDS SDK methods (uses IMDSv2 token flow automatically)
	metaOut, err := imdsClient.GetMetadata(ctx, &imds.GetMetadataInput{Path: "instance-id"})
	if err != nil {
		t.Fatalf("GetMetadata instance-id: %v", err)
	}

	bodyBytes, _ := io.ReadAll(metaOut.Content)
	if string(bodyBytes) != instanceID {
		t.Fatalf("got instance-id %q, want %q", string(bodyBytes), instanceID)
	}

	typeOut, err := imdsClient.GetMetadata(ctx, &imds.GetMetadataInput{Path: "instance-type"})
	if err != nil {
		t.Fatalf("GetMetadata instance-type: %v", err)
	}

	typeBytes, _ := io.ReadAll(typeOut.Content)
	if string(typeBytes) != "t3.micro" {
		t.Fatalf("got instance-type %q, want t3.micro", string(typeBytes))
	}

	regOut, err := imdsClient.GetRegion(ctx, &imds.GetRegionInput{})
	if err != nil {
		t.Fatalf("GetRegion: %v", err)
	}

	if regOut.Region != "us-east-1" {
		t.Fatalf("got region %q, want us-east-1", regOut.Region)
	}

	docOut, err := imdsClient.GetInstanceIdentityDocument(ctx, &imds.GetInstanceIdentityDocumentInput{})
	if err != nil {
		t.Fatalf("GetInstanceIdentityDocument: %v", err)
	}

	if docOut.InstanceID != instanceID || docOut.Region != "us-east-1" {
		t.Fatalf("unexpected doc: %+v", docOut)
	}

	userOut, err := imdsClient.GetUserData(ctx, &imds.GetUserDataInput{})
	if err != nil {
		t.Fatalf("GetUserData: %v", err)
	}

	userBytes, _ := io.ReadAll(userOut.Content)
	if string(userBytes) != rawUserData {
		t.Fatalf("got userdata %q, want %q", string(userBytes), rawUserData)
	}

	iamOut, err := imdsClient.GetIAMInfo(ctx, &imds.GetIAMInfoInput{})
	if err != nil {
		t.Fatalf("GetIAMInfo: %v", err)
	}

	if iamOut.InstanceProfileArn == "" || iamOut.Code != "Success" {
		t.Fatalf("unexpected IAM info: %+v", iamOut)
	}

	// 5. Test IAM security credentials via direct IMDS HTTP GET
	credResp, err := http.Get(imdsPrefix + "/latest/meta-data/iam/security-credentials/ec2-worker-role")
	if err != nil {
		t.Fatalf("security-credentials GET: %v", err)
	}

	defer credResp.Body.Close()
	if credResp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(credResp.Body)
		t.Fatalf("security-credentials status: %d, body: %s", credResp.StatusCode, string(errBody))
	}

	var creds map[string]any
	if err = json.NewDecoder(credResp.Body).Decode(&creds); err != nil {
		t.Fatalf("decode creds JSON: %v", err)
	}

	if creds["Code"] != "Success" || creds["AccessKeyId"] == "" || creds["SecretAccessKey"] == "" {
		t.Fatalf("incomplete security credentials: %+v", creds)
	}

	// Verify the issued credentials work against TinyStack STS
	tempAccessKey := creds["AccessKeyId"].(string)
	tempSecret := creds["SecretAccessKey"].(string)
	tempToken := creds["Token"].(string)

	stsCli := sts.New(sts.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{
				AccessKeyID:     tempAccessKey,
				SecretAccessKey: tempSecret,
				SessionToken:    tempToken,
			}, nil
		}),
		Retryer: aws.NopRetryer{},
	})

	callerOut, err := stsCli.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		t.Fatalf("GetCallerIdentity with IMDS temporary credentials: %v", err)
	}

	if !strings.Contains(aws.ToString(callerOut.Arn), "ec2-worker-role") {
		t.Fatalf("caller ARN = %q, want containing ec2-worker-role", aws.ToString(callerOut.Arn))
	}

	// 6. Test IMDSv1 vs IMDSv2 enforcement
	// Default is HttpTokens: optional -> IMDSv1 GET without token works
	resp, err := http.Get(imdsPrefix + "/latest/meta-data/instance-id")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("IMDSv1 GET expected 200 when optional, got status %d, err %v", resp.StatusCode, err)
	}

	_ = resp.Body.Close()

	// Modify to HttpTokens: required
	_, err = ec2Cli.ModifyInstanceMetadataOptions(ctx, &ec2.ModifyInstanceMetadataOptionsInput{
		InstanceId: aws.String(instanceID),
		HttpTokens: ec2types.HttpTokensStateRequired,
	})
	if err != nil {
		t.Fatalf("ModifyInstanceMetadataOptions: %v", err)
	}

	// Direct IMDSv1 GET without token must now fail with 401 Unauthorized
	resp, err = http.Get(imdsPrefix + "/latest/meta-data/instance-id")
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for tokenless GET when HttpTokens=required, got %d", resp.StatusCode)
	}

	// SDK IMDS client still succeeds because it acquires IMDSv2 token via PUT
	metaOut, err = imdsClient.GetMetadata(ctx, &imds.GetMetadataInput{Path: "instance-id"})
	if err != nil {
		t.Fatalf("SDK IMDS client failed after HttpTokens=required: %v", err)
	}

	bodyBytes, _ = io.ReadAll(metaOut.Content)
	if string(bodyBytes) != instanceID {
		t.Fatalf("got instance-id %q, want %q", string(bodyBytes), instanceID)
	}
}

func TestIMDS_DedicatedListener(t *testing.T) {
	s, tinystack := serverWithApp(t)

	// Create dedicated IMDS server using TinyStack's IMDSHandler
	imdsServer := httptest.NewServer(tinystack.Metadata.IMDSHandler())
	defer imdsServer.Close()

	ctx := context.Background()
	ec2Cli := ec2Client(s, "test", "us-east-1")

	runOut, err := ec2Cli.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId:      latestImage(t, s),
		InstanceType: ec2types.InstanceTypeT3Micro,
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
	})
	if err != nil {
		t.Fatal(err)
	}

	instanceID := aws.ToString(runOut.Instances[0].InstanceId)

	// 1. Without header: falls back to the running instance
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		imdsServer.URL+"/latest/meta-data/instance-id",
		http.NoBody,
	)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != instanceID {
		t.Fatalf("expected resolved instance %s, got %s", instanceID, string(body))
	}

	// 2. With X-TinyStack-Instance-ID header
	req, _ = http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		imdsServer.URL+"/latest/meta-data/instance-id",
		http.NoBody,
	)
	req.Header.Set("X-TinyStack-Instance-ID", instanceID)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	if string(body) != instanceID {
		t.Fatalf("expected resolved instance %s, got %s", instanceID, string(body))
	}
}

func TestECS_CredentialsAndMetadata(t *testing.T) {
	rt := &containerRuntime{containers: map[string]*fakeContainer{}}
	s, _ := serverWithApp(t, rt)
	ctx := context.Background()

	// 1. Create task IAM role
	taskRoleARN := createServiceRole(t, s, "test", "ecs-sample-task-role", "ecs-tasks.amazonaws.com")

	c := ecsClient(s, "us-east-1")
	if _, err := c.CreateCluster(ctx, &ecs.CreateClusterInput{}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.RegisterTaskDefinition(ctx, &ecs.RegisterTaskDefinitionInput{
		Family:      aws.String("webapp"),
		TaskRoleArn: new(taskRoleARN),
		ContainerDefinitions: []ecstypes.ContainerDefinition{
			{
				Name:      aws.String("web"),
				Image:     aws.String("nginx:latest"),
				Memory:    aws.Int32(128),
				Essential: aws.Bool(true),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	run, err := c.RunTask(ctx, &ecs.RunTaskInput{TaskDefinition: aws.String("webapp")})
	if err != nil {
		t.Fatal(err)
	}

	task := waitStatus(ctx, t, c, run.Tasks[0].TaskArn, "RUNNING")
	spec := rt.find(aws.ToString(task.Containers[0].RuntimeId)).spec
	token := spec.Environment["AWS_CONTAINER_AUTHORIZATION_TOKEN"]
	if token == "" || spec.Environment["ECS_CONTAINER_METADATA_URI_V4"] != s.URL+"/_tinystack/ecs/v4/"+token ||
		spec.Environment["AWS_CONTAINER_CREDENTIALS_FULL_URI"] != s.URL+"/_tinystack/ecs/credentials" {
		t.Fatalf("metadata environment: %v", spec.Environment)
	}

	// 3. Test credentials retrieval via endpointcreds provider
	provider := endpointcreds.New(
		s.URL+"/_tinystack/ecs/credentials",
		func(o *endpointcreds.Options) {
			o.AuthorizationToken = token
		},
	)

	creds, err := provider.Retrieve(ctx)
	if err != nil {
		t.Fatalf("endpointcreds Retrieve: %v", err)
	}

	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" || creds.SessionToken == "" {
		t.Fatalf("incomplete credentials: %+v", creds)
	}

	// Verify credentials work against TinyStack STS
	stsCli := sts.New(sts.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return creds, nil
		}),
		Retryer: aws.NopRetryer{},
	})

	callerOut, err := stsCli.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		t.Fatalf("GetCallerIdentity with ECS task credentials: %v", err)
	}

	if !strings.Contains(aws.ToString(callerOut.Arn), "ecs-sample-task-role") {
		t.Fatalf("caller ARN = %q, want containing ecs-sample-task-role", aws.ToString(callerOut.Arn))
	}

	// 4. Test invalid token on credentials endpoint
	badProvider := endpointcreds.New(
		s.URL+"/_tinystack/ecs/credentials",
		func(o *endpointcreds.Options) {
			o.AuthorizationToken = "nonexistent-token"
		},
	)

	_, err = badProvider.Retrieve(ctx)
	if err == nil {
		t.Fatal("expected error with nonexistent token, got nil")
	}

	// 5. Test ECS Task Metadata v4 HTTP endpoints
	for _, base := range []string{s.URL + "/_tinystack/ecs/v4/" + token} {
		// Container metadata
		resp, getErr := http.Get(base)
		if getErr != nil {
			t.Fatalf("GET container metadata %s: %v", base, getErr)
		}

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET container metadata status %d", resp.StatusCode)
		}

		var cMeta map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&cMeta)
		_ = resp.Body.Close()
		if cMeta["Name"] != "web" || cMeta["Image"] != "nginx:latest" {
			t.Fatalf("unexpected container metadata: %+v", cMeta)
		}

		// Task metadata
		resp, getErr = http.Get(base + "/task")
		if getErr != nil {
			t.Fatalf("GET task metadata: %v", getErr)
		}

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET task metadata status %d", resp.StatusCode)
		}

		var tMeta map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&tMeta)
		_ = resp.Body.Close()
		if tMeta["Family"] != "webapp" || tMeta["Cluster"] != "default" {
			t.Fatalf("unexpected task metadata: %+v", tMeta)
		}

		// Stats
		resp, getErr = http.Get(base + "/stats")
		if getErr != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET stats status %d, err %v", resp.StatusCode, getErr)
		}

		_ = resp.Body.Close()

		// Task stats
		resp, getErr = http.Get(base + "/task/stats")
		if getErr != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET task/stats status %d, err %v", resp.StatusCode, getErr)
		}

		_ = resp.Body.Close()
	}

	// 6. Unknown token gets 404
	resp, err := http.Get(s.URL + "/_tinystack/ecs/v4/unknown-token")
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown token, got %d", resp.StatusCode)
	}

	if _, err = c.StopTask(ctx, &ecs.StopTaskInput{Task: task.TaskArn}); err != nil {
		t.Fatal(err)
	}

	if _, err = provider.Retrieve(ctx); err == nil {
		t.Fatal("stopped task credentials still available")
	}

	resp, err = http.Get(s.URL + "/_tinystack/ecs/v4/" + token)
	if err != nil {
		t.Fatal(err)
	}

	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stopped task metadata status %d", resp.StatusCode)
	}
}

func TestMetadataS3BucketRoutes(t *testing.T) {
	s, _ := serverWithApp(t)
	ctx := context.Background()
	c := s3Client(s, "test", "us-east-1")
	for _, name := range []string{"latest"} {
		if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new(name)}); err != nil {
			t.Fatal(err)
		}

		if _, err := c.PutObject(
			ctx,
			&s3.PutObjectInput{Bucket: new(name), Key: aws.String("object"), Body: strings.NewReader("value")},
		); err != nil {
			t.Fatal(err)
		}

		out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: new(name), Key: aws.String("object")})
		if err != nil {
			t.Fatal(err)
		}

		body, err := io.ReadAll(out.Body)
		out.Body.Close()
		if err != nil || string(body) != "value" {
			t.Fatalf("bucket %s: %s, %v", name, body, err)
		}
	}
}

func TestIMDSInstanceSelection(t *testing.T) {
	s, a := serverWithApp(t)
	dedicated := httptest.NewServer(a.Metadata.IMDSHandler())
	defer dedicated.Close()
	ctx := context.Background()
	check := func(header, id string, status int) {
		t.Helper()
		req, err := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			dedicated.URL+"/latest/meta-data/instance-id",
			http.NoBody,
		)
		if err != nil {
			t.Fatal(err)
		}

		if header != "" {
			req.Header.Set(header, id)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != status || (status == http.StatusOK && string(body) != id) {
			t.Fatalf("selection %s: status=%d body=%s", header, resp.StatusCode, body)
		}

		if status == http.StatusNotFound && !strings.Contains(string(body), "no unique running instance") {
			t.Fatalf("unclear selection error: %s", body)
		}
	}

	check("", "", http.StatusNotFound)
	c := ec2Client(s, "test", "us-east-1")
	out, err := c.RunInstances(
		ctx,
		&ec2.RunInstancesInput{
			ImageId:      latestImage(t, s),
			InstanceType: ec2types.InstanceTypeT3Micro,
			MinCount:     aws.Int32(1),
			MaxCount:     aws.Int32(1),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	id := aws.ToString(out.Instances[0].InstanceId)
	check("", id, http.StatusOK)
	if _, err = c.RunInstances(
		ctx,
		&ec2.RunInstancesInput{
			ImageId:      latestImage(t, s),
			InstanceType: ec2types.InstanceTypeT3Micro,
			MinCount:     aws.Int32(1),
			MaxCount:     aws.Int32(1),
		},
	); err != nil {
		t.Fatal(err)
	}

	check("", "", http.StatusNotFound)
	check("X-Instance-ID", id, http.StatusNotFound)
	check("X-TinyStack-Instance-Id", id, http.StatusOK)
}

func TestIMDSVerifyCredentialRefusal(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Mode = "verify"
	cfg.Identity.SecretKeys = map[string]string{"test": "test"}
	a, err := app.New(
		context.Background(),
		cfg,
		&memory.Store{},
		&fakeRuntime{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}

	defer a.Stop()
	s := httptest.NewServer(a.Gateway)
	defer s.Close()
	for _, path := range []string{"meta-data/iam/security-credentials/", "meta-data/iam/security-credentials/role"} {
		resp, getErr := http.Get(s.URL + "/_tinystack/imds/i-example/latest/" + path)
		if getErr != nil {
			t.Fatal(getErr)
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "dedicated IMDS listener") {
			t.Fatalf("credential refusal: %d %s", resp.StatusCode, body)
		}
	}

	dedicated := httptest.NewServer(a.Metadata.IMDSHandler())
	defer dedicated.Close()
	resp, err := http.Get(dedicated.URL + "/latest/meta-data/iam/security-credentials/role")
	if err != nil {
		t.Fatal(err)
	}

	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("dedicated listener status %d", resp.StatusCode)
	}
}

package integration_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
)

// This executable touches only t.TempDir. No real Lima command is invoked.
const fakeLimaScript = `#!/usr/bin/env python3
import sys, os, json
root=os.path.dirname(os.path.abspath(__file__))
statefile=root+'/state.json'
state=json.load(open(statefile)) if os.path.exists(statefile) else {}
a=sys.argv[1:]
with open(root+'/args','a') as f: f.write(json.dumps(a)+'\n')
if a[0]=='list':
 for name, status in state.items(): print(json.dumps(dict(name=name,status=status,sshLocalPort=32122)))
elif a[0]=='create':
 name=next(x.split('=',1)[1] for x in a if x.startswith('--name='))
 with open(root+'/'+name+'.yaml','w') as f: f.write(sys.stdin.read())
 state[name]='Stopped'
elif a[0] in ('start','restart'):
 if os.path.exists(root+'/fail'): sys.exit(1)
 state[a[-1]]='Running'
elif a[0]=='stop': state[a[-1]]='Stopped'
elif a[0]=='delete': state.pop(a[-1],None)
elif a[0]=='shell': sys.exit(0 if os.path.exists(root+'/ready') else 1)
else: sys.exit(2)
with open(statefile,'w') as f: json.dump(state,f)
`

func waitVMState(t *testing.T, c *sdk.Client, id string, state types.InstanceStateName) types.Instance {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		out, err := c.DescribeInstances(ctx, &sdk.DescribeInstancesInput{InstanceIds: []string{id}})
		if err != nil {
			t.Fatal(err)
		}

		if len(out.Reservations) == 1 && out.Reservations[0].Instances[0].State.Name == state {
			return out.Reservations[0].Instances[0]
		}

		select {
		case <-ctx.Done():
			t.Fatalf("instance %s did not become %s: %+v", id, state, out)
		case <-ticker.C:
		}
	}
}

func TestEC2LimaLifecycleIMDS(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "limactl-fake")
	if err := os.WriteFile(command, []byte(fakeLimaScript), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"foreign-vm":"Running"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig()
	cfg.EC2.Runtime, cfg.EC2.Lima.Command, cfg.EC2.VM.MaxVMs = "lima", command, 1
	s := serverConfig(t, cfg, &fakeRuntime{})
	c := ec2Client(s, "test", "us-east-1")
	ctx := context.Background()
	var err error
	role := "vm-worker"
	createServiceRole(t, s, "alias", role, "ec2.amazonaws.com")
	ic := iamClient(s, "test", "us-east-1")
	if _, err = ic.CreateInstanceProfile(
		ctx,
		&iam.CreateInstanceProfileInput{InstanceProfileName: new(role)},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = ic.AddRoleToInstanceProfile(ctx, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: new(role), RoleName: new(role),
	}); err != nil {
		t.Fatal(err)
	}

	input := &sdk.RunInstancesInput{
		ImageId: latestImage(t, s), InstanceType: types.InstanceTypeT3Micro,
		MinCount: new(int32(1)), MaxCount: new(int32(1)),
		IamInstanceProfile: &types.IamInstanceProfileSpecification{Name: new(role)},
	}

	out, err := c.RunInstances(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	id := aws.ToString(out.Instances[0].InstanceId)
	waitVMState(t, c, id, types.InstanceStateNamePending)
	input.ClientToken = nil
	_, err = c.RunInstances(ctx, input)
	code(t, err, "InsufficientInstanceCapacity")
	if err = os.WriteFile(filepath.Join(dir, "ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	running := waitVMState(t, c, id, types.InstanceStateNameRunning)
	foundSSH := false
	for _, tag := range running.Tags {
		if aws.ToString(tag.Key) == "tinystack:ssh-endpoint" {
			foundSSH = true
		}
	}

	if !foundSSH {
		t.Fatal("missing SSH endpoint tag")
	}

	files, err := filepath.Glob(filepath.Join(dir, "tinystack-ec2-*.yaml"))
	if err != nil || len(files) != 1 {
		t.Fatalf("config files: %v %v", files, err)
	}

	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}

	match := regexp.MustCompile(`192\.168\.5\.2:(\d+)`).FindSubmatch(data)
	if len(match) != 2 {
		t.Fatalf("no IMDS listener in %s", data)
	}

	endpoint := "http://127.0.0.1:" + string(match[1])
	mc := imds.New(imds.Options{Endpoint: endpoint, HTTPClient: &http.Client{Timeout: 5 * time.Second}})
	creds, err := mc.GetMetadata(ctx, &imds.GetMetadataInput{Path: "iam/security-credentials/" + role})
	if err != nil {
		t.Fatal(err)
	}

	defer creds.Content.Close()
	var credentials map[string]any
	if err = json.NewDecoder(creds.Content).Decode(&credentials); err != nil {
		t.Fatal(err)
	}

	if credentials["AccessKeyId"] == "" || credentials["Token"] == "" {
		t.Fatalf("credentials: %v", credentials)
	}

	identity, err := mc.GetMetadata(ctx, &imds.GetMetadataInput{Path: "instance-id"})
	if err != nil {
		t.Fatal(err)
	}

	defer identity.Content.Close()
	body, _ := io.ReadAll(identity.Content)
	if string(body) != id {
		t.Fatalf("wrong VM identity: %s", body)
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		endpoint+"/latest/meta-data/instance-id",
		http.NoBody,
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-TinyStack-Instance-Id", "i-foreign")
	v1, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	v1Body, err := io.ReadAll(v1.Body)
	v1.Body.Close()
	if err != nil || string(v1Body) != id {
		t.Fatalf("IMDSv1 listener identity: %s %v", v1Body, err)
	}

	if _, err = c.StopInstances(ctx, &sdk.StopInstancesInput{InstanceIds: []string{id}}); err != nil {
		t.Fatal(err)
	}

	waitVMState(t, c, id, types.InstanceStateNameStopped)
	waitVMCommand(t, dir, "stop")
	if _, err = c.StartInstances(ctx, &sdk.StartInstancesInput{InstanceIds: []string{id}}); err != nil {
		t.Fatal(err)
	}

	waitVMState(t, c, id, types.InstanceStateNameRunning)
	if _, err = c.RebootInstances(ctx, &sdk.RebootInstancesInput{InstanceIds: []string{id}}); err != nil {
		t.Fatal(err)
	}

	// Wait for the observable restart command before issuing terminate.
	waitVMCommand(t, dir, "restart")
	if _, err = c.TerminateInstances(ctx, &sdk.TerminateInstancesInput{InstanceIds: []string{id}}); err != nil {
		t.Fatal(err)
	}

	waitVMState(t, c, id, types.InstanceStateNameTerminated)
	waitVMCommand(t, dir, "delete")
	// A second launch is cleaned by reset; the unrelated VM must survive both operations.
	input.ClientToken = nil
	if _, err = c.RunInstances(ctx, input); err != nil {
		t.Fatal(err)
	}

	waitVMCommand(t, dir, "create", 2)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/_tinystack/reset", http.NoBody)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("reset: %s", response.Status)
	}

	state, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil || string(state) != `{"foreign-vm": "Running"}` {
		t.Fatalf("reset ownership: %s %v", state, err)
	}

	images, err := c.DescribeImages(ctx, &sdk.DescribeImagesInput{Filters: []types.Filter{
		{Name: new("name"), Values: []string{"amzn2-*"}},
	}})
	if err != nil || len(images.Images) != 1 {
		t.Fatalf("AL2 image: %v %v", images, err)
	}

	input.ClientToken = nil
	input.ImageId = images.Images[0].ImageId
	_, err = c.RunInstances(ctx, input)
	code(t, err, "InvalidParameterValue")
}

func waitVMCommand(t *testing.T, dir, command string, counts ...int) {
	t.Helper()
	count := 1
	if len(counts) > 0 {
		count = counts[0]
	}

	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, _ := os.ReadFile(filepath.Join(dir, "args"))
		if strings.Count(string(data), `["`+command+`"`) >= count {
			return
		}

		select {
		case <-deadline.C:
			t.Fatalf("missing %s: %s", command, data)
		case <-ticker.C:
		}
	}
}

// TestLimaRealVM is deliberately opt-in: it downloads a cloud image and creates a VM.
func TestLimaRealVM(t *testing.T) {
	if os.Getenv("TINYSTACK_VM_TESTS") != "1" {
		t.Skip("set TINYSTACK_VM_TESTS=1 to boot a real Lima VM")
	}

	cfg := testConfig()
	cfg.EC2.Runtime = "lima"
	s := serverConfig(t, cfg, &fakeRuntime{})
	c := ec2Client(s, "test", "us-east-1")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	arch, typ := "x86_64", types.InstanceTypeT3Micro
	if runtime.GOARCH == "arm64" {
		arch, typ = "arm64", types.InstanceTypeT4gMicro
	}

	images, err := c.DescribeImages(ctx, &sdk.DescribeImagesInput{Filters: []types.Filter{
		{Name: new("name"), Values: []string{"al2023-*"}}, {Name: new("architecture"), Values: []string{arch}},
	}})
	if err != nil || len(images.Images) == 0 {
		t.Fatalf("images: %v %v", images, err)
	}

	role := "real-vm-worker"
	createServiceRole(t, s, "alias", role, "ec2.amazonaws.com")
	ic := iamClient(s, "test", "us-east-1")
	if _, err = ic.CreateInstanceProfile(
		ctx,
		&iam.CreateInstanceProfileInput{InstanceProfileName: new(role)},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = ic.AddRoleToInstanceProfile(
		ctx,
		&iam.AddRoleToInstanceProfileInput{InstanceProfileName: new(role), RoleName: new(role)},
	); err != nil {
		t.Fatal(err)
	}

	userData := "#!/bin/sh\necho userdata-ok > /var/lib/tinystack-user-test\n"
	out, err := c.RunInstances(ctx, &sdk.RunInstancesInput{
		ImageId: images.Images[0].ImageId, InstanceType: typ,
		IamInstanceProfile: &types.IamInstanceProfileSpecification{Name: new(role)},
		UserData:           new(base64.StdEncoding.EncodeToString([]byte(userData))),
		MinCount:           new(int32(1)), MaxCount: new(int32(1)),
	})
	if err != nil {
		t.Fatal(err)
	}

	id := aws.ToString(out.Instances[0].InstanceId)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, _ = c.TerminateInstances(cleanupCtx, &sdk.TerminateInstancesInput{InstanceIds: []string{id}})
		_ = sdk.NewInstanceTerminatedWaiter(c).
			Wait(cleanupCtx, &sdk.DescribeInstancesInput{InstanceIds: []string{id}}, time.Minute)
	})
	waiter := sdk.NewInstanceRunningWaiter(c)
	if err = waiter.Wait(ctx, &sdk.DescribeInstancesInput{InstanceIds: []string{id}}, 12*time.Minute); err != nil {
		t.Fatal(err)
	}

	// limactl shell is used only inside this explicit opt-in test.
	names, err := exec.CommandContext(ctx, "limactl", "list", "--json").Output()
	if err != nil {
		t.Fatal(err)
	}

	decoder := json.NewDecoder(strings.NewReader(string(names)))
	for {
		var machine struct {
			Name string `json:"name"`
		}

		if err := decoder.Decode(&machine); err != nil {
			t.Fatal(err)
		}

		if strings.HasSuffix(machine.Name, "-"+id) {
			output, err := exec.CommandContext(
				ctx,
				"limactl",
				"shell",
				machine.Name,
				"sh",
				"-c",
				"test $(cat /var/lib/tinystack-user-test) = userdata-ok && "+
					"curl -fsS http://169.254.169.254/latest/meta-data/iam/security-credentials/real-vm-worker",
			).CombinedOutput()
			if err != nil || !strings.Contains(string(output), "AccessKeyId") {
				t.Fatalf("guest IMDS: %s %v", output, err)
			}

			break
		}
	}
}

func TestEC2LimaBootFailureAndMemoryCap(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "fake-limactl")
	if err := os.WriteFile(command, []byte(fakeLimaScript), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "fail"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig()
	cfg.EC2.Runtime, cfg.EC2.Lima.Command = "lima", command
	s := serverConfig(t, cfg, &fakeRuntime{})
	c := ec2Client(s, "test", "us-east-1")
	ctx := context.Background()
	input := &sdk.RunInstancesInput{
		ImageId: latestImage(t, s), InstanceType: types.InstanceTypeM5Large,
		MinCount: new(int32(1)), MaxCount: new(int32(1)),
	}

	_, err := c.RunInstances(ctx, input)
	code(t, err, "InsufficientInstanceCapacity")
	input.InstanceType = types.InstanceTypeT3Micro
	out, err := c.RunInstances(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	failed := waitVMState(t, c, aws.ToString(out.Instances[0].InstanceId), types.InstanceStateNameTerminated)
	if failed.StateReason == nil || aws.ToString(failed.StateReason.Code) != "Server.InternalError" {
		t.Fatalf("boot failure: %+v", failed)
	}

	waitVMCommand(t, dir, "delete")
}

package integration_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
)

// The real-VM tests are never part of ordinary runs: each downloads cloud images and boots real guests.
func TestLimaRealVM(t *testing.T) { realVMs(t, "lima") }

func TestQEMURealVM(t *testing.T) { realVMs(t, "qemu") }

// realVMs boots AL2023 and Ubuntu one at a time, then checks user data and guest IMDS role credentials over SSH
// through the tinystack:ssh-endpoint tag, as a user would.
func realVMs(t *testing.T, driver string) {
	if os.Getenv("TINYSTACK_VM_TESTS") != "1" {
		t.Skipf("set TINYSTACK_VM_TESTS=1 to boot real %s VMs", driver)
	}

	dir := t.TempDir()
	key := filepath.Join(dir, "id_ed25519")
	// ssh-keygen and ssh are used only inside this explicit opt-in test.
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %s %v", out, err)
	}

	publicKey, err := os.ReadFile(key + ".pub") // #nosec G304 -- the test's own temporary key.
	if err != nil {
		t.Fatal(err)
	}

	cfg := testConfig()
	cfg.EC2.Runtime = driver
	s := serverConfig(t, cfg, &fakeRuntime{})
	c := ec2Client(s, "test", "us-east-1")
	setup := context.Background()
	if _, err = c.ImportKeyPair(
		setup,
		&sdk.ImportKeyPairInput{KeyName: new("real-vm"), PublicKeyMaterial: publicKey},
	); err != nil {
		t.Fatal(err)
	}

	role := "real-vm-worker"
	createServiceRole(t, s, "test", role, "ec2.amazonaws.com")
	ic := iamClient(s, "test", "us-east-1")
	if _, err = ic.CreateInstanceProfile(
		setup,
		&iam.CreateInstanceProfileInput{InstanceProfileName: new(role)},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = ic.AddRoleToInstanceProfile(
		setup,
		&iam.AddRoleToInstanceProfileInput{InstanceProfileName: new(role), RoleName: new(role)},
	); err != nil {
		t.Fatal(err)
	}

	arch, typ := "x86_64", types.InstanceTypeT3Micro
	if runtime.GOARCH == "arm64" {
		arch, typ = "arm64", types.InstanceTypeT4gMicro
	}

	for _, guest := range []struct{ name, pattern, user string }{
		{"al2023", "al2023-*", "ec2-user"},
		{"ubuntu", "ubuntu/images/*", "ubuntu"},
	} {
		t.Run(guest.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(setup, 20*time.Minute)
			defer cancel()
			images, err := c.DescribeImages(ctx, &sdk.DescribeImagesInput{Filters: []types.Filter{
				{
					Name:   new("name"),
					Values: []string{guest.pattern},
				},
				{Name: new("architecture"), Values: []string{arch}},
			}})
			if err != nil || len(images.Images) == 0 {
				t.Fatalf("images: %v %v", images, err)
			}

			userData := "#!/bin/sh\necho userdata-ok > /var/lib/tinystack-user-test\n"
			out, err := c.RunInstances(ctx, &sdk.RunInstancesInput{
				ImageId: images.Images[0].ImageId, InstanceType: typ, KeyName: new("real-vm"),
				IamInstanceProfile: &types.IamInstanceProfileSpecification{Name: new(role)},
				UserData:           new(base64.StdEncoding.EncodeToString([]byte(userData))),
				MinCount:           new(int32(1)), MaxCount: new(int32(1)),
			})
			if err != nil {
				t.Fatal(err)
			}

			ids := []string{aws.ToString(out.Instances[0].InstanceId)}
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
				defer cancel()
				_, _ = c.TerminateInstances(cleanup, &sdk.TerminateInstancesInput{InstanceIds: ids})
				_ = sdk.NewInstanceTerminatedWaiter(c).
					Wait(cleanup, &sdk.DescribeInstancesInput{InstanceIds: ids}, time.Minute)
			})
			if err = sdk.NewInstanceRunningWaiter(c).
				Wait(ctx, &sdk.DescribeInstancesInput{InstanceIds: ids}, 12*time.Minute); err != nil {
				t.Fatalf("%v: %s", err, stateReason(ctx, c, ids[0]))
			}

			described, err := c.DescribeInstances(ctx, &sdk.DescribeInstancesInput{InstanceIds: ids})
			if err != nil {
				t.Fatal(err)
			}

			endpoint := ""
			for _, tag := range described.Reservations[0].Instances[0].Tags {
				if aws.ToString(tag.Key) == "tinystack:ssh-endpoint" {
					endpoint = aws.ToString(tag.Value)
				}
			}

			host, port, found := strings.Cut(endpoint, ":")
			if !found {
				t.Fatalf("no ssh endpoint tag: %q", endpoint)
			}

			// SSH readiness can precede cloud-init, so retry until user data and IMDS answer.
			check := "test \"$(cat /var/lib/tinystack-user-test)\" = userdata-ok && " +
				"curl -fsS http://169.254.169.254/latest/meta-data/iam/security-credentials/" + role
			deadline := time.Now().Add(5 * time.Minute)
			for {
				output, err := exec.CommandContext(
					ctx, "ssh", "-i", key, "-p", port,
					"-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "IdentitiesOnly=yes",
					"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR",
					guest.user+"@"+host, check,
				).CombinedOutput()
				if err == nil && strings.Contains(string(output), "AccessKeyId") {
					break
				}

				if time.Now().After(deadline) {
					t.Fatalf("guest check: %s %v", output, err)
				}

				time.Sleep(5 * time.Second)
			}
		})
	}
}

// stateReason returns the instance's state reason, which carries the VM runtime error.
func stateReason(ctx context.Context, c *sdk.Client, id string) string {
	out, err := c.DescribeInstances(ctx, &sdk.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil || len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
		return fmt.Sprint(err)
	}

	reason := out.Reservations[0].Instances[0].StateReason
	if reason == nil {
		return "no state reason"
	}

	return aws.ToString(reason.Message)
}

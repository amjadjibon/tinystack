package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/amjadjibon/tinystack/internal/config"
)

func TestEC2QEMULifecycle(t *testing.T) {
	t.Setenv("TINYSTACK_FAKE_QEMU", "1")
	logDir := t.TempDir()
	t.Setenv("TINYSTACK_FAKE_QEMU_LOG", filepath.Join(logDir, "args"))
	image := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/image" {
			http.NotFound(w, r)
			return
		}

		_, _ = w.Write([]byte("fake qcow2"))
	}))
	defer image.Close()
	previousClient := http.DefaultClient
	http.DefaultClient = image.Client()
	t.Cleanup(func() { http.DefaultClient = previousClient })
	cfg := testConfig()
	cfg.EC2.Runtime = "qemu"
	cfg.EC2.VM.MaxVMs = 1
	cfg.EC2.QEMU.AllowTCG = true
	cfg.EC2.QEMU.SystemCommand = map[string]string{"x86_64": os.Args[0], "arm64": os.Args[0]}
	cfg.EC2.QEMU.ImgCommand = os.Args[0]
	cfg.EC2.QEMU.CacheDir = t.TempDir()
	// Keep the Unix QMP socket below sockaddr_un's macOS length limit.
	state, err := os.MkdirTemp("", "ts-qemu-")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(state) })
	cfg.EC2.QEMU.StateDir = state
	foreign := filepath.Join(state, "tinystack-ec2-foreign-i-test")
	if mkdirErr := os.Mkdir(foreign, 0o700); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}

	s := serverConfig(t, cfg, &fakeRuntime{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	idImage := latestImage(t, s)
	// Select the deterministic catalogue ID before configuring its fixture image.
	// The TLS fixture exercises the same HTTPS-only config validation as production.
	cfg.EC2.VM.Images = map[string]config.VMImage{aws.ToString(idImage): {
		URL: image.URL + "/image", Digest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("fake qcow2"))),
	}}
	// The first server was used only to select its deterministic catalogue AMI.
	s = serverConfig(t, cfg, &fakeRuntime{})
	c := ec2Client(s, "test", "us-east-1")
	input := &sdk.RunInstancesInput{
		ImageId: idImage, InstanceType: types.InstanceTypeT3Micro,
		MinCount: new(int32(1)), MaxCount: new(int32(1)),
	}

	out, err := c.RunInstances(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	id := aws.ToString(out.Instances[0].InstanceId)
	waitVMState(t, c, id, types.InstanceStateNameRunning)
	files, err := filepath.Glob(filepath.Join(state, "tinystack-ec2-*", "machine.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("state: %v %v", files, err)
	}

	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}

	var saved struct{ Spec struct{ Port int } }
	if decodeErr := json.Unmarshal(data, &saved); decodeErr != nil {
		t.Fatal(decodeErr)
	}

	for _, path := range []string{"/latest/meta-data/instance-id", "/meta-data", "/user-data", "/vendor-data"} {
		req, _ := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			fmt.Sprintf("http://127.0.0.1:%d%s", saved.Spec.Port, path),
			http.NoBody,
		)
		req.Header.Set("X-TinyStack-Instance-Id", "i-foreign")
		response, requestErr := http.DefaultClient.Do(req)
		if requestErr != nil {
			t.Fatal(requestErr)
		}

		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatal(response.Status)
		}

		if strings.Contains(path, "meta-data") && !strings.Contains(string(body), id) {
			t.Fatalf("identity: %s", body)
		}
	}

	input.ClientToken = nil
	_, err = c.RunInstances(ctx, input)
	code(t, err, "InsufficientInstanceCapacity")
	ids := []string{id}
	if _, err = c.StopInstances(ctx, &sdk.StopInstancesInput{InstanceIds: ids}); err != nil {
		t.Fatal(err)
	}

	waitVMState(t, c, id, types.InstanceStateNameStopped)
	if _, err = c.StartInstances(ctx, &sdk.StartInstancesInput{InstanceIds: ids}); err != nil {
		t.Fatal(err)
	}

	waitVMState(t, c, id, types.InstanceStateNameRunning)
	if _, err = c.RebootInstances(ctx, &sdk.RebootInstancesInput{InstanceIds: ids}); err != nil {
		t.Fatal(err)
	}

	waitVMState(t, c, id, types.InstanceStateNameRunning)
	waitVMCommand(t, logDir, "system_reset")
	if _, err = c.TerminateInstances(ctx, &sdk.TerminateInstancesInput{InstanceIds: ids}); err != nil {
		t.Fatal(err)
	}

	waitVMState(t, c, id, types.InstanceStateNameTerminated)
	input.ClientToken = nil
	out, err = c.RunInstances(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	waitVMState(t, c, aws.ToString(out.Instances[0].InstanceId), types.InstanceStateNameRunning)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/_tinystack/reset", http.NoBody)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal(response.Status)
	}

	if _, err := os.Stat(foreign); err != nil {
		t.Fatal("foreign VM removed", err)
	}
}

package tinystack_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"

	tinystack "github.com/amjadjibon/tinystack/testcontainers"
)

func TestContainer(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := t.Context()
	directory := t.TempDir()
	configFile := filepath.Join(directory, "tinystack.yaml")
	require.NoError(t, os.WriteFile(configFile, []byte("defaults:\n  region: us-west-2\n"), 0o600))
	scriptFile := filepath.Join(directory, "10-ready.sh")
	script := "#!/bin/sh\nset -eu\nsleep 1\nprintf '%s' \"$AWS_DEFAULT_REGION\" > /tmp/tinystack-ready\n"
	require.NoError(t, os.WriteFile(scriptFile, []byte(script), 0o644))

	image := os.Getenv("TINYSTACK_IMAGE")
	opts := []testcontainers.ContainerCustomizer{
		tinystack.WithConfig(configFile),
		tinystack.WithInitScripts("ready", scriptFile),
		tinystack.WithReadyScriptsComplete(),
	}
	if image == "" {
		root, err := filepath.Abs("..")
		require.NoError(t, err)
		opts = append(opts, testcontainers.WithDockerfile(testcontainers.FromDockerfile{
			Context:        root,
			Dockerfile:     "Dockerfile",
			BuildLogWriter: os.Stdout,
			BuildOptionsModifier: func(options *client.ImageBuildOptions) {
				options.Target = "headless"
				options.Version = build.BuilderBuildKit
			},
		}))
	}

	ctr, err := tinystack.Run(ctx, image, opts...)
	if ctr != nil {
		testcontainers.CleanupContainer(t, ctr.Container)
	}

	require.NoError(t, err)
	endpoint, err := ctr.Endpoint(ctx)
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/_tinystack/init/ready", http.NoBody)
	require.NoError(t, err)
	httpClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var status struct {
		State   string `json:"state"`
		Scripts []struct {
			ExitCode *int   `json:"exit_code"`
			Name     string `json:"name"`
			State    string `json:"state"`
		} `json:"scripts"`
		Completed bool `json:"completed"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&status))
	require.True(t, status.Completed)
	require.Equal(t, "SUCCESSFUL", status.State)
	require.Len(t, status.Scripts, 1)
	require.Equal(t, "10-ready.sh", status.Scripts[0].Name)
	require.Equal(t, "SUCCESSFUL", status.Scripts[0].State)
	require.Equal(t, new(0), status.Scripts[0].ExitCode)
	code, output, err := ctr.Exec(ctx, []string{"cat", "/tmp/tinystack-ready"})
	require.NoError(t, err)
	require.Zero(t, code)
	marker, err := io.ReadAll(output)
	require.NoError(t, err)
	require.Equal(t, "us-west-2", string(marker))

	cfg := aws.Config{
		Region:       "us-west-2",
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		BaseEndpoint: new(endpoint),
	}
	buckets := s3.NewFromConfig(cfg, func(options *s3.Options) { options.UsePathStyle = true })
	queues := sqs.NewFromConfig(cfg)
	_, err = buckets.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: new("testcontainers-bucket"),
		CreateBucketConfiguration: &types.CreateBucketConfiguration{
			LocationConstraint: types.BucketLocationConstraintUsWest2,
		},
	})
	require.NoError(t, err)
	_, err = queues.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: new("testcontainers-queue")})
	require.NoError(t, err)
	beforeBuckets, err := buckets.ListBuckets(ctx, &s3.ListBucketsInput{})
	require.NoError(t, err)
	require.Len(t, beforeBuckets.Buckets, 1)
	beforeQueues, err := queues.ListQueues(ctx, &sqs.ListQueuesInput{})
	require.NoError(t, err)
	require.Len(t, beforeQueues.QueueUrls, 1)

	require.NoError(t, ctr.Reset(ctx))
	afterBuckets, err := buckets.ListBuckets(ctx, &s3.ListBucketsInput{})
	require.NoError(t, err)
	require.Empty(t, afterBuckets.Buckets)
	afterQueues, err := queues.ListQueues(ctx, &sqs.ListQueuesInput{})
	require.NoError(t, err)
	require.Empty(t, afterQueues.QueueUrls)
}

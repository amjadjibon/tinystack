package tinystack_test

import (
	"context"
	"log"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"

	tinystack "github.com/amjadjibon/tinystack/tests/testcontainers"
)

func ExampleRun() {
	ctx := context.Background()
	// Use a locally built image until the registry image is published.
	ctr, err := tinystack.Run(ctx, "tinystack:test")
	defer func() {
		if ctr != nil {
			if terminateErr := testcontainers.TerminateContainer(ctr.Container); terminateErr != nil {
				log.Printf("terminate tinystack: %v", terminateErr)
			}
		}
	}()
	if err != nil {
		log.Printf("start tinystack: %v", err)
		return
	}

	endpoint, err := ctr.Endpoint(ctx)
	if err != nil {
		log.Printf("tinystack endpoint: %v", err)
		return
	}

	cfg := aws.Config{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		BaseEndpoint: new(endpoint),
	}
	buckets := s3.NewFromConfig(cfg, func(options *s3.Options) { options.UsePathStyle = true })
	if _, err := buckets.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("uploads")}); err != nil {
		log.Printf("create bucket: %v", err)
	}
}

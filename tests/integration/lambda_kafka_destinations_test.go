package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
)

func TestLambdaKafkaFailureDestinations(t *testing.T) {
	for _, destination := range []string{"arn:aws:s3:::kafka-failures", "kafka://failed"} {
		t.Run(destination, func(t *testing.T) {
			cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, "orders", "failed"))
			if err != nil {
				t.Fatal(err)
			}

			defer cluster.Close()
			runtime := &kafkaFailureRuntime{mode: "retry", batches: make(chan []int64, 8)}
			s := serverWith(t, runtime)
			c := lambdaClient(s, "test")
			createFunction(t, c, "kafka-destinations", "app.fail")
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			objects := s3Client(s, "test", "us-east-1")
			if _, createErr := objects.CreateBucket(
				ctx,
				&s3.CreateBucketInput{Bucket: new("kafka-failures")},
			); createErr != nil {
				t.Fatal(createErr)
			}

			producer, err := kgo.NewClient(kgo.SeedBrokers(cluster.ListenAddrs()...))
			if err != nil {
				t.Fatal(err)
			}

			defer producer.Close()
			if produceErr := producer.ProduceSync(
				ctx,
				&kgo.Record{Topic: "orders", Value: []byte("good")},
				&kgo.Record{
					Topic: "orders",
					Key:   []byte("original-key"),
					Value: []byte("bad"),
				},
			).FirstErr(); produceErr != nil {
				t.Fatal(produceErr)
			}

			mapping, err := c.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
				FunctionName: new(
					"kafka-destinations",
				),
				Topics:           []string{"orders"},
				StartingPosition: types.EventSourcePositionTrimHorizon,
				SelfManagedEventSource: &types.SelfManagedEventSource{Endpoints: map[string][]string{
					"KAFKA_BOOTSTRAP_SERVERS": cluster.ListenAddrs(),
				}},
				BatchSize:                      new(int32(1)),
				MaximumBatchingWindowInSeconds: new(int32(0)),
				MaximumRetryAttempts:           new(int32(0)),
				ProvisionedPollerConfig:        &types.ProvisionedPollerConfig{},
				DestinationConfig: &types.DestinationConfig{
					OnFailure: &types.OnFailure{Destination: new(destination)},
				},
			})
			if err != nil {
				t.Fatal(err)
			}

			awaitKafkaOffset(t, ctx, producer, *mapping.UUID, 2)
			var body []byte
			if strings.HasPrefix(destination, "kafka://") {
				consumer, clientErr := kgo.NewClient(
					kgo.SeedBrokers(cluster.ListenAddrs()...),
					kgo.ConsumeTopics("failed"),
				)
				if clientErr != nil {
					t.Fatal(clientErr)
				}

				defer consumer.Close()
				fetched := consumer.PollRecords(ctx, 1)
				if fetchErr := fetched.Err(); fetchErr != nil {
					t.Fatal(fetchErr)
				}

				records := fetched.Records()
				if len(records) != 1 || string(records[0].Key) != "original-key" {
					t.Fatalf("destination records: %+v", records)
				}

				body = records[0].Value
			} else {
				listed, listErr := objects.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: new("kafka-failures")})
				if listErr != nil || len(listed.Contents) != 1 {
					t.Fatalf("objects: %+v, %v", listed, listErr)
				}

				object, getErr := objects.GetObject(
					ctx,
					&s3.GetObjectInput{Bucket: new("kafka-failures"), Key: listed.Contents[0].Key},
				)
				if getErr != nil {
					t.Fatal(getErr)
				}

				defer object.Body.Close()
				body, err = io.ReadAll(object.Body)
				if err != nil {
					t.Fatal(err)
				}
			}

			var record map[string]json.RawMessage
			if err := json.Unmarshal(body, &record); err != nil {
				t.Fatal(err)
			}

			fields := []string{"requestContext", "responseContext", "timestamp", "version", "KafkaBatchInfo", "payload"}
			for _, field := range fields {
				if len(record[field]) == 0 {
					t.Fatalf("missing %s in %s", field, body)
				}
			}

			if _, updateErr := c.UpdateEventSourceMapping(ctx, &sdklambda.UpdateEventSourceMappingInput{
				UUID: mapping.UUID, Enabled: new(false),
			}); updateErr != nil {
				t.Fatal(updateErr)
			}
		})
	}
}
